package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"codearts2api-panel/internal/livecfg"
	"codearts2api-panel/internal/upstream"
)

// 「识图桥接」：让纯文本模型也能吃图。
//
// 背景（2026-11-03 实测）：CodeArts 这条上游只有 `Qwen3-VL-235B` / `kimi-k2.6-vl` 声明
// `supports_images: true`，DeepSeek / GLM / Kimi-k3 / Pangu 全部显式 false，带图必回
// `InferHub.001001020.406`。但同一条上游里就摆着能看图的 VL 模型，所以网关可以替调用方
// 做一次「图片转文字」：先用 VL 模型把图描述出来，再把描述作为文本塞回消息里，
// 最后用原来的纯文本模型继续跑。
//
// 对客户端完全透明：模型名不变、返回格式不变，只是背后多了一次 VL 调用。
// 代价与边界（必须知道）：
//   - 纯文本模型看到的**是文字描述，不是像素**：截图/文字/表格很准，细节视觉（颜色微差、
//     精细布局）会失真；需要精确视觉时应直接调 VL 模型；
//   - 每张新图多一次 VL 调用（2~5s）；同一张图在后续轮次命中缓存，不再重复调用；
//   - 描述会占用上下文（几千字/张），比原始图片的视觉 token 省，但比"什么都不发"多。

// VisionBridgePolicy 是识图桥接策略，定义在 internal/livecfg：
// 它属于「面板保存后热生效」的配置，所以要和快照放在一起，避免两处结构各写一遍。
type VisionBridgePolicy = livecfg.VisionBridge

// bridgeStats 一次桥接的统计（日志/测试用）。
type bridgeStats struct {
	Images    int
	Chars     int
	VLModel   string
	CacheHits int
}

// 转述结果缓存：同一张图在后续轮次会被重新发送（客户端每轮重放完整历史），
// 不缓存就会每轮多做一次 VL 调用。
const (
	visionCacheMaxEntries = 128
	visionCacheMaxBytes   = 8 << 20
)

type visionCacheT struct {
	mu    sync.Mutex
	order []string
	items map[string]string
	bytes int
}

var visionCache = &visionCacheT{items: map[string]string{}}

func visionCacheKey(url, model, prompt string) string {
	imgSum := sha256.Sum256([]byte(url))
	promptSum := sha256.Sum256([]byte(prompt))
	return fmt.Sprintf("%s|%s|%x", hex.EncodeToString(imgSum[:16]), model, promptSum[:8])
}

func (c *visionCacheT) get(k string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[k]
	return v, ok
}

func (c *visionCacheT) put(k, v string) {
	if v == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[k]; exists {
		return
	}
	c.items[k] = v
	c.order = append(c.order, k)
	c.bytes += len(v)
	for len(c.order) > visionCacheMaxEntries || (c.bytes > visionCacheMaxBytes && len(c.order) > 1) {
		oldest := c.order[0]
		c.order = c.order[1:]
		if old, ok := c.items[oldest]; ok {
			c.bytes -= len(old)
			delete(c.items, oldest)
		}
	}
}

// imageParts 收集请求里的内联图片（只认 data URL；远端 http 图上游未必接受，原样放过）。
func imagePartsOf(req *chatRequest) []map[string]any {
	var out []map[string]any
	for _, m := range req.Messages {
		parts, ok := m.Content.([]any)
		if !ok {
			continue
		}
		for _, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if kind, _ := part["type"].(string); kind != "image_url" {
				continue
			}
			img, ok := part["image_url"].(map[string]any)
			if !ok {
				continue
			}
			if url, _ := img["url"].(string); strings.HasPrefix(url, "data:") {
				out = append(out, part)
			}
		}
	}
	return out
}

// modelSupportsImages 查上游目录里该模型的图片能力。
// 返回 (是否支持, 是否有明确结论)：上游没表态（nil）时 second=false，调用方应保守不动手。
func (h *Handler) modelSupportsImages(model string) (bool, bool) {
	want := strings.ToLower(upstream.CanonicalModel(model))
	for _, mi := range h.fetchDynamicModels(false) {
		if strings.ToLower(mi.ID) == want {
			if mi.SupportsImages == nil {
				return false, false
			}
			return *mi.SupportsImages, true
		}
	}
	return false, false
}

// bridgeImages 把请求里的内联图片换成「VL 模型的文字转述」，供纯文本模型使用。
// 任何一步失败都**保持原样**（宁可让上游按 406 明确报错，也不要把图悄悄吃掉）。
func (h *Handler) bridgeImages(ctx context.Context, req *chatRequest, policy VisionBridgePolicy) (bridgeStats, error) {
	var st bridgeStats
	policy = policy.Normalized()
	st.VLModel = policy.Model

	parts := imagePartsOf(req)
	if len(parts) == 0 {
		return st, nil
	}
	limit := len(parts)
	if limit > policy.MaxImages {
		return st, fmt.Errorf("图片数量 %d 超过识图上限 %d（config vision_bridge.max_images）", limit, policy.MaxImages)
	}
	// 先把所有图片都转述出来，全部成功再落地替换。
	// 否则中途失败会留下「前几张变文字、后几张还是图」的畸形请求 ——
	// 那种请求既过不了上游的 406 检查，也让调用方看不懂。
	type resolved struct {
		part map[string]any
		desc string
	}
	// 总预算：多图时每张图的超时取「单张上限」与「剩余预算」的较小值，
	// 否则 8 张图在排队时能串成好几分钟，客户端早就超时了。
	deadline := time.Now().Add(policy.TotalTimeout)
	plan := make([]resolved, 0, limit)
	for i := 0; i < limit; i++ {
		part := parts[i]
		img, _ := part["image_url"].(map[string]any)
		url, _ := img["url"].(string)
		key := visionCacheKey(url, policy.Model, policy.Prompt)
		desc, ok := visionCache.get(key)
		if ok {
			st.CacheHits++
		} else {
			budget := time.Until(deadline)
			if budget <= 0 {
				return st, fmt.Errorf("识图总预算（%s）已用完，第 %d 张图未处理", policy.TotalTimeout, i+1)
			}
			if budget > policy.Timeout {
				budget = policy.Timeout
			}
			sub, cancel := context.WithTimeout(ctx, budget)
			got, err := h.askVision(sub, url, policy)
			cancel()
			if err != nil {
				return st, fmt.Errorf("第 %d 张图识图失败: %w", i+1, err)
			}
			desc = strings.TrimSpace(got)
			if desc == "" {
				return st, fmt.Errorf("第 %d 张图转述为空", i+1)
			}
			// 必须按「字符」截断：按字节切会把汉字切半，JSON 编码后变成一串 U+FFFD。
			if len(desc) > policy.MaxChars {
				desc = truncateRunes(desc, policy.MaxChars) + "…（已截断）"
			}
			visionCache.put(key, desc)
		}
		plan = append(plan, resolved{part: part, desc: desc})
	}
	for i, r := range plan {
		// 用文字块替换图片块：后面的纯文本模型就能"读到"这张图。
		r.part["type"] = "text"
		r.part["text"] = fmt.Sprintf("[第 %d 张图片｜由 %s 识别]\n%s", i+1, policy.Model, r.desc)
		delete(r.part, "image_url")
		st.Images++
		st.Chars += len(r.desc)
	}
	return st, nil
}

// askVision 用指定的 VL 模型对一张图做一次转述。
func (h *Handler) askVision(ctx context.Context, imageURL string, policy VisionBridgePolicy) (string, error) {
	// held 必须显式跟踪：重试路径里会先让出槽位再重新获取，失败时若还盲目 Release，
	// 会把别人正在用的槽位减掉（计数器是账号级的，不分持有者）→ 上游并发被超额。
	held := false
	wait := policy.Timeout
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); left < wait {
			wait = left
		}
	}
	if wait <= 0 {
		return "", errors.New("识图预算已耗尽")
	}
	acct := h.acquireAuxiliarySlot(ctx, wait, policy.Model)
	if acct == nil || acct.Auth == nil {
		return "", errors.New("没有空闲且满足保留积分规则的账号做识图")
	}
	held = true
	defer func() {
		if held {
			h.cfg.Pool.ReleaseLock(acct.Name)
		}
	}()

	token, ak, sk := acct.Auth.Credentials()
	cred := upstream.SignCredential{AccessKeyID: ak, SecretAccessKey: sk, SecurityToken: token}
	msgs := []upstream.ChatMessage{{
		Role: "user",
		Content: []any{
			map[string]any{"type": "text", "text": policy.Prompt},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}},
		},
	}}
	maxTokens := policy.MaxTokens
	opts := upstream.ChatOptions{MaxTokens: &maxTokens}

	// 每个账号固定一个识图会话 id：上游每个账号只允许 3 个并发会话，
	// 每次新建 id 会把槽位吃满（实测第一次跑就撞 TM.00001041「并发会话数已达上限」）。
	// V2 接口不保留语义上下文（正文每轮完整重放），所以复用同一个会话是安全的。
	chatID := visionChatID(acct.Name)

	for {
		rc, err := acct.Client.ChatStreamWithOptions(ctx, chatID, msgs, "", cred, acct.UserName, policy.Model, opts)
		if err != nil {
			var ae *upstream.ApiError
			if errors.As(err, &ae) && isQueueLimitError(ae) {
				// 并发会话槽位被占满：让出账号槽位等一会儿再试，最多等到 ctx 超时。
				log.Printf("vision bridge 排队等待（%s）account=%s", truncateMsg(ae.Message, 80), acct.Name)
				h.cfg.Pool.ReleaseLock(acct.Name)
				held = false
				select {
				case <-time.After(visionQueueRetryDelay):
					acct = h.acquireAuxiliarySlot(ctx, policy.Timeout, policy.Model)
					if acct == nil {
						return "", errors.New("识图重新获取账号槽位超时")
					}
					held = true
					continue
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			return "", err
		}
		comp, aerr := upstream.AggregateRaw(rc)
		_ = rc.Close()
		if aerr != nil {
			return "", aerr
		}
		if text := strings.TrimSpace(comp.Content); text != "" {
			return text, nil
		}
		return strings.TrimSpace(comp.Reasoning), nil
	}
}

// visionQueueRetryDelay 识图子请求撞上「并发会话数已达上限」后的等待间隔（测试可覆盖）。
var visionQueueRetryDelay = 5 * time.Second

// visionChatID 给某账号派生一个固定的识图会话 id（32 位十六进制，符合上游要求）。
func visionChatID(accountName string) string {
	sum := sha256.Sum256([]byte("codearts-vision-bridge:" + accountName))
	return hex.EncodeToString(sum[:16])
}

// truncateRunes 按字符（rune）截断，避免把多字节字符切坏。
func truncateRunes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s // 字节数没超，字符数一定没超
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
