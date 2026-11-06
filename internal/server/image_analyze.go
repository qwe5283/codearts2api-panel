// 带图请求的 analyze 处理（复刻官方 CodeArts Agent 的 AnalyzeImagePlugin）。
//
// 背景：上游的纯文本模型收到图片会回 InferHub.001001020（not multimodal），
// 且视觉模型上下文普遍更小（131k vs 1M）、没有推理输出，直接改派会毁掉
// 上下文与思考链。官方 IDE 的做法是：
//
//  1. 发请求前把图片 part 换成一行文本占位符 [ImageId][ImageName]，
//     真正发往上游时过滤掉图片 part —— 主模型永远收不到图；
//  2. 注入 analyze_image 工具，主模型按需调用它（可带自定义 task 描述）；
//  3. 工具调用由网关内部用视觉模型完成，只喂【单张图 + task】，
//     把文本结果作为 tool result 回灌，主模型继续推理。
//
// 这样主模型的上下文和推理能力完全不受影响，视觉模型只承担「看图说话」。
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"codearts2api-panel/internal/upstream"
)

const (
	// analyzeImageToolName 注入给主模型的工具名（与官方一致）。
	analyzeImageToolName = "analyze_image"
	// analyzeMaxRounds 主模型连续调用工具的轮数上限。超过就停止并把已有结果
	// 当最终答复返回，避免死循环烧 token。
	analyzeMaxRounds = 4
	// analyzeImageTTL 图片在内存里保留多久。只服务同一次对话的后续轮次。
	analyzeImageTTL = 30 * time.Minute
	// visionSlotWait 识图子请求等账号并发槽位的上限。
	visionSlotWait = 30 * time.Second
	// analyzeVisionAttempts 单张图片识图的总尝试次数（含 81104 间歇失败与并发排队重试）。
	analyzeVisionAttempts = 4
)

// analyzeImageStore 保存本次进程内收到的图片，供 analyze_image 工具按 ID 取用。
// 官方把图片存在会话数据库里；网关没有持久化需求，用带 TTL 的内存表即可。
type analyzeImageStore struct {
	mu     sync.Mutex
	images map[string]storedImage
}

type storedImage struct {
	dataURL  string
	mime     string
	filename string
	added    time.Time
}

var imageStore = &analyzeImageStore{images: map[string]storedImage{}}

func newImageID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("img_%d", time.Now().UnixNano())
	}
	return "img_" + hex.EncodeToString(b[:])
}

func (s *analyzeImageStore) put(dataURL, mime, filename string) string {
	id := newImageID()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.images { // 顺手清理过期项
		if now.Sub(v.added) > analyzeImageTTL {
			delete(s.images, k)
		}
	}
	s.images[id] = storedImage{dataURL: dataURL, mime: mime, filename: filename, added: now}
	return id
}

func (s *analyzeImageStore) get(id string) (storedImage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.images[id]
	if !ok || time.Since(v.added) > analyzeImageTTL {
		return storedImage{}, false
	}
	return v, true
}

// analyzeImageSpec 注入给主模型的工具定义。
func analyzeImageSpec() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": analyzeImageToolName,
			"description": "分析指定 ID 的图片，提取文字（OCR）、对象、布局、颜色等信息。" +
				"当用户需要查看或理解图片内容时调用。" +
				"imageId 来自用户消息里的 [ImageId: ...] 标记。",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"imageId": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "要分析的图片 ID 列表",
					},
					"task": map[string]any{
						"type":        "string",
						"description": "详细的分析任务描述，越详细结果越好",
					},
				},
				"required": []string{"imageId", "task"},
			},
		},
	}
}

// rewriteImagesForAnalyze 把消息里的图片分片替换成占位符文本，并返回注入工具。
//
// 主模型看到的是：
//
//	[ImageId: img_xxx][ImageName: a.png] This is an image. ...
//
// 图片本体存进 imageStore，等主模型调用 analyze_image 时再取。
// 同时追加一条 system-reminder 提示模型在需要时调用工具（与官方一致）。
func rewriteImagesForAnalyze(req *chatRequest) (injected []map[string]any) {
	var hasImages bool
	for i := range req.Messages {
		m := &req.Messages[i]
		if !m.HasImages {
			continue
		}
		blocks, ok := m.Content.([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(blocks))
		for _, raw := range blocks {
			part, ok := raw.(map[string]any)
			if !ok {
				kept = append(kept, raw)
				continue
			}
			if part["type"] != "image_url" {
				kept = append(kept, raw)
				continue
			}
			image, _ := part["image_url"].(map[string]any)
			url, _ := image["url"].(string)
			if strings.TrimSpace(url) == "" {
				continue
			}
			mime := "image/png"
			if strings.HasPrefix(url, "data:") {
				if semi := strings.Index(url, ";"); semi > 5 {
					mime = url[5:semi]
				}
			}
			id := imageStore.put(url, mime, "")
			kept = append(kept, map[string]any{
				"type": "text",
				"text": fmt.Sprintf("[ImageId: %s] This is an image. "+
					"If the user needs to view or analyze this image, call the %s tool with this imageId. "+
					"Otherwise do not call it.", id, analyzeImageToolName),
			})
			hasImages = true
		}
		m.Content = kept
		m.Text = flattenContent(mustJSON(kept))
		m.HasImages = false
	}
	if !hasImages {
		return nil
	}
	// system-reminder：与官方一致，提醒模型在需要时调用工具。
	req.Messages = append(req.Messages, openAIMessage{
		Role: "system",
		Text: "<system-reminder>If the user asks about the image's content, " +
			"invoke the analyze_image tool with the [ImageId] from the message " +
			"before answering.</system-reminder>",
		Content: "<system-reminder>If the user asks about the image's content, " +
			"invoke the analyze_image tool with the [ImageId] from the message " +
			"before answering.</system-reminder>",
	})
	return []map[string]any{analyzeImageSpec()}
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("[]")
	}
	return raw
}

// runAnalyzeTool 执行一次 analyze_image 调用，返回给主模型的文本结果。
// 只把【单张图 + task】发给视觉模型，主模型的上下文不进视觉模型。
func (h *Handler) runAnalyzeTool(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		ImageID []string `json:"imageId"`
		Task    string   `json:"task"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid analyze_image arguments: %w", err)
	}
	if len(args.ImageID) == 0 {
		return "", fmt.Errorf("imageId must not be empty")
	}
	if strings.TrimSpace(args.Task) == "" {
		return "", fmt.Errorf("task must not be empty")
	}
	vision := h.visionModelForAnalyze()
	if vision == "" {
		return "", fmt.Errorf("no vision model available in the account catalog")
	}
	var sb strings.Builder
	for _, id := range args.ImageID {
		img, ok := imageStore.get(id)
		if !ok {
			return "", fmt.Errorf("image %s not found or expired; ask the user to send it again", id)
		}
		text, err := h.describeImage(ctx, vision, img, args.Task)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&sb, "\nThis is the analysis result of image id [%s] : %s", id, text)
	}
	return strings.TrimSpace(sb.String()), nil
}

// describeImage 用视觉模型分析单张图片。
func (h *Handler) describeImage(ctx context.Context, model string, img storedImage, task string) (string, error) {
	// 拿不到槽位就让这次识图失败，由调用方按“图片原样保留”处理。
	// held 必须显式跟踪：排队重试时会先让出槽位再重新获取，失败时若还盲目
	// Release，会把别人正在用的槽位减掉（计数器是账号级的，不分持有者）。
	held := false
	acct := h.acquireAuxiliarySlot(ctx, visionSlotWait, model)
	if acct == nil {
		return "", fmt.Errorf("vision model %s busy: no free slot", model)
	}
	held = true
	defer func() {
		if held {
			h.cfg.Pool.ReleaseLock(acct.Name)
		}
	}()

	token, ak, sk := acct.Auth.Credentials()
	cred := upstream.SignCredential{AccessKeyID: ak, SecretAccessKey: sk, SecurityToken: token}
	// 与官方一致：文本在前、图片在后。实测图片在前时上游偶发
	// ModelArts.81104.503（Conversion failed），调整顺序更稳。
	messages := []upstream.ChatMessage{{
		Role: "user",
		Content: []any{
			map[string]any{"type": "text", "text": task},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": img.dataURL}},
		},
	}}
	maxOut := 2048 // 与官方 createVisionModel 的 output 上限对齐，避免超长转换失败

	// 会话 id 固定复用：上游按 chat_id 计并发会话，每个账号只允许 3 个。识图子请求
	// 如果在每次工具调用（甚至每次重试）都新建 id，几次之后槽位就被占满，上游一律回
	// TM.00001041「并发会话数已达上限」，主模型只会看到"工具一直失败、看不到图"。
	// 与 vision_bridge 共用同一个派生规则（visionChatID），两条识图路径共享同一份会话预算。
	chatID := visionChatID(acct.Name)

	var lastErr error
	for attempt := 0; attempt < analyzeVisionAttempts; attempt++ {
		rc, err := acct.Client.ChatStreamWithOptions(ctx, chatID, messages, "", cred, acct.UserName, model,
			upstream.ChatOptions{MaxTokens: &maxOut})
		if err != nil {
			lastErr = err
			var ae *upstream.ApiError
			if errors.As(err, &ae) && isQueueLimitError(ae) {
				// 并发会话槽位被占满：让出账号并发槽等一下再试。这不是"图看不了"，
				// 直接失败会把并发上限误报成图片识别失败。
				log.Printf("analyze_image queue limit, yielding slot account=%s: %s", acct.Name, truncateMsg(ae.Message, 80))
				if held {
					h.cfg.Pool.ReleaseLock(acct.Name)
					held = false
				}
				select {
				case <-time.After(visionQueueRetryDelay):
					acct = h.acquireAuxiliarySlot(ctx, visionSlotWait, model)
					if acct == nil {
						return "", fmt.Errorf("vision model %s busy: no free slot", model)
					}
					held = true
					continue
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			continue
		}
		comp, aerr := upstream.AggregateRaw(rc)
		rc.Close()
		if aerr != nil {
			lastErr = aerr
			continue
		}
		return strings.TrimSpace(comp.Content), nil
	}
	return "", lastErr
}

// visionModelForAnalyze 选择用于 analyze_image 的视觉模型。
// 优先 Qwen3-VL-235B（官方 hc 环境同款），否则取目录里第一个声明支持图片的。
func (h *Handler) visionModelForAnalyze() string {
	ids := h.visionModelIDs()
	if len(ids) == 0 {
		return ""
	}
	for _, id := range ids {
		if strings.EqualFold(id, "Qwen3-VL-235B") {
			return id
		}
	}
	return ids[0]
}

// openAIMessagesToUpstream 把网关内部生成的 tool 结果转成上游消息。
func openAIMessagesToUpstream(msgs []openAIMessage) []upstream.ChatMessage {
	out := make([]upstream.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, upstream.ChatMessage{
			Role:       m.Role,
			Content:    m.Text,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		})
	}
	return out
}

// handleAnalyzeToolCalls 处理主模型返回的工具调用：把 analyze_image 的调用
// 在网关内部执行掉，其他工具原样留给客户端。
//
// 返回 (toolResults, handled)：handled=false 表示本次没有 analyze_image 调用，
// 调用方按正常流程返回给客户端。
func (h *Handler) handleAnalyzeToolCalls(ctx context.Context, calls []openAIToolCall) ([]openAIMessage, bool) {
	var results []openAIMessage
	handled := false
	for _, c := range calls {
		if c.Name != analyzeImageToolName {
			continue
		}
		handled = true
		out, err := h.runAnalyzeTool(ctx, c.Arguments)
		if err != nil {
			log.Printf("analyze_image failed: %v", err)
			out = "analyze_image failed: " + err.Error()
		}
		results = append(results, openAIMessage{
			Role:       "tool",
			ToolCallID: c.ID,
			Name:       analyzeImageToolName,
			Text:       out,
			Content:    out,
		})
	}
	return results, handled
}
