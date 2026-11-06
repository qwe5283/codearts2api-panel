package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

const tinyPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="

// extractImageID 从发往上游的请求体里取出占位符中的 imageId。
func extractImageID(body string) string {
	const marker = "[ImageId: "
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	if j := strings.Index(rest, "]"); j > 0 {
		return rest[:j]
	}
	return ""
}

// analyze 模式完整链路：主模型收到的是占位符而非图片；主模型调用 analyze_image
// 后，网关用视觉模型分析并把结果回灌；最终返回主模型的答案。
func TestAnalyzeModeConvertsImagesAndFeedsBackToolResult(t *testing.T) {
	var mainCalls, visionCalls atomic.Int32
	var firstMainBody string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		model, _ := payload["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")

		if strings.Contains(model, "VL") {
			// 视觉模型：应只收到【单张图 + task】，不含主模型的历史
			visionCalls.Add(1)
			if !strings.Contains(string(body), "image_url") {
				t.Errorf("视觉模型请求里应含图片")
			}
			if strings.Contains(string(body), "ImageId") {
				t.Errorf("视觉模型不应收到占位符历史")
			}
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"图片是一个红色方块\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}

		n := mainCalls.Add(1)
		if n == 1 {
			firstMainBody = string(body)
			// 第一次：主模型从占位符里取出真实 imageId 再调用工具。
			id := extractImageID(string(body))
			if id == "" {
				t.Errorf("主模型请求里应含 [ImageId: ...] 占位符")
			}
			args, _ := json.Marshal(map[string]any{"imageId": []string{id}, "task": "这是什么颜色"})
			frame := map[string]any{
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []any{map[string]any{
							"index": 0, "id": "call_1", "type": "function",
							"function": map[string]any{"name": analyzeImageToolName, "arguments": string(args)},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			}
			raw, _ := json.Marshal(frame)
			_, _ = io.WriteString(w, "data: "+string(raw)+"\n\ndata: [DONE]\n\n")
			return
		}
		// 第二次：拿到 tool 结果后给最终答案
		if !strings.Contains(string(body), "红色方块") {
			t.Errorf("第二轮应带上工具结果，body=%s", string(body))
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"这是一个红色方块\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()

	h, p := analyzeTestHandler(t, backend.URL)
	upstream.SetAccountModels("vision-a", []upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", SupportsImages: boolPtr(false)},
		{ID: "Qwen3-VL-235B", SupportsImages: boolPtr(true)},
	})
	if ids := h.visionModelIDs(); len(ids) == 0 {
		t.Fatal("测试前置：目录里应有视觉模型")
	}

	payload := `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":64,
		"messages":[{"role":"user","content":[{"type":"text","text":"这张图什么颜色"},{"type":"image_url","image_url":{"url":"` + tinyPNG + `"}}]}]}`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
	h.ServeHTTP(rec, req)

	if mainCalls.Load() < 2 {
		t.Fatalf("应发生工具回灌（主模型至少 2 次），实际 %d 次；body=%s", mainCalls.Load(), rec.Body.String())
	}
	if visionCalls.Load() != 1 {
		t.Errorf("视觉模型应被调用 1 次，实际 %d", visionCalls.Load())
	}
	// 主模型首轮请求里绝不能出现图片本体
	if strings.Contains(firstMainBody, "image_url") || strings.Contains(firstMainBody, "iVBORw0KGgo") {
		t.Errorf("主模型不应收到图片本体，body=%.400s", firstMainBody)
	}
	if !strings.Contains(firstMainBody, "ImageId") {
		t.Errorf("主模型应收到占位符，body=%.400s", firstMainBody)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "红色方块") {
		t.Errorf("最终答复应包含分析结果，got=%s", rec.Body.String())
	}
	assertVisionPoolHealthy(t, p)
}

// 流式客户端在 analyze 模式下也必须走通：网关先聚合、回灌工具结果，
// 最终以 SSE chunk 格式回放。
func TestAnalyzeModeStreamingClient(t *testing.T) {
	var mainCalls, visionCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		model, _ := payload["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")

		if strings.Contains(model, "VL") {
			visionCalls.Add(1)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"红色\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		if mainCalls.Add(1) == 1 {
			id := extractImageID(string(body))
			args, _ := json.Marshal(map[string]any{"imageId": []string{id}, "task": "颜色"})
			frame := map[string]any{"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "c1", "type": "function",
					"function": map[string]any{"name": analyzeImageToolName, "arguments": string(args)},
				}}},
				"finish_reason": "tool_calls",
			}}}
			raw, _ := json.Marshal(frame)
			_, _ = io.WriteString(w, "data: "+string(raw)+"\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"红色方块\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()

	h, p := analyzeTestHandler(t, backend.URL)
	upstream.SetAccountModels("vision-a", []upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", SupportsImages: boolPtr(false)},
		{ID: "Qwen3-VL-235B", SupportsImages: boolPtr(true)},
	})

	payload := `{"model":"deepseek-v4.1-flash","stream":true,"max_tokens":64,
		"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + tinyPNG + `"}}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))

	if visionCalls.Load() != 1 || mainCalls.Load() < 2 {
		t.Fatalf("流式 analyze 未完成回灌: main=%d vision=%d", mainCalls.Load(), visionCalls.Load())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "chat.completion.chunk") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("应输出 SSE chunk 格式，got=%.300s", body)
	}
	if !strings.Contains(body, "红色方块") {
		t.Errorf("流式响应应含最终答案，got=%.300s", body)
	}
	assertVisionPoolHealthy(t, p)
}

// analyze 模式下**纯文本**流式请求必须保持直通：网关不能在生成完成前聚合，
// 否则客户端首字延迟等于整段生成时间（实测 91s TTFB）。
func TestAnalyzeModeTextStreamStaysDirect(t *testing.T) {
	var mainCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mainCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		// 分片下发：若网关直通，第一片应立即写出
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"second\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()

	h, _ := analyzeTestHandler(t, backend.URL)
	upstream.SetAccountModels("vision-a", []upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", SupportsImages: boolPtr(false)},
		{ID: "Qwen3-VL-235B", SupportsImages: boolPtr(true)},
	})

	payload := `{"model":"deepseek-v4.1-flash","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))

	if rec.Code != http.StatusOK {
		t.Fatalf("纯文本流式应成功，status=%d", rec.Code)
	}
	if mainCalls.Load() != 1 {
		t.Errorf("纯文本请求不应触发回灌，主模型调用 %d 次", mainCalls.Load())
	}
	body := rec.Body.String()
	// 直通会把上游两个 delta 原样转发（各自独立的 data 帧）；
	// 聚合补发只会有一个合并后的 content 帧。数 content delta 帧的个数。
	if n := strings.Count(body, `"content":"first"`); n != 1 {
		t.Errorf("首片应原样直通，got %d 次；body=%.300s", n, body)
	}
	if n := strings.Count(body, `"content":"second"`); n != 1 {
		t.Errorf("次片应原样直通，got %d 次；body=%.300s", n, body)
	}
}

// passthrough（默认）模式下带图请求仍直接 400，行为不变。
func TestPassthroughModeStillRejectsImages(t *testing.T) {
	var attempts atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
	}))
	defer backend.Close()

	h, _ := analyzeTestHandler(t, backend.URL)
	h.cfg.ImageHandling = "passthrough"
	upstream.SetAccountModels("vision-a", []upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", SupportsImages: boolPtr(false)},
	})

	payload := `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":16,
		"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + tinyPNG + `"}}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("passthrough 应回 400，got %d", rec.Code)
	}
	if attempts.Load() != 0 {
		t.Errorf("passthrough 不应联系上游，实际 %d 次", attempts.Load())
	}
}

// 工具参数错误（图片 ID 不存在）应作为 tool 结果回灌，而不是让整个请求失败。
func TestAnalyzeToolUnknownImageReturnsToolError(t *testing.T) {
	h, _ := analyzeTestHandler(t, "http://127.0.0.1:1")
	upstream.SetAccountModels("vision-a", []upstream.ModelInfo{
		{ID: "Qwen3-VL-235B", SupportsImages: boolPtr(true)},
	})
	out, err := h.runAnalyzeTool(context.Background(), `{"imageId":["no-such-id"],"task":"看颜色"}`)
	if err == nil {
		t.Fatalf("不存在的图片应报错，got=%q", out)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("错误信息应提示图片不存在，got=%v", err)
	}
}

func analyzeTestHandler(t *testing.T, endpoint string) (*Handler, *pool.Pool) {
	t.Helper()
	a := auth.New("vision-a", "tester", "domain", "token", "ak", "sk",
		time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 1, ErrThreshold: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range p.Accounts() {
		acct.Client = upstream.NewWithChatEndpoint(time.Second, endpoint)
	}
	return NewHandler(Config{
		Pool: p, DefaultModel: "deepseek-v4.1-flash", MaxRotate: 1,
		ErrThreshold: 1, ImageHandling: "analyze",
	}), p
}

// 识图子请求必须复用固定的会话 id：上游每账号只允许 3 个并发会话，
// 每次工具调用都新建 chat_id 会很快把槽位占满，之后一律 TM.00001041。
func TestAnalyzeImageReusesVisionSessionID(t *testing.T) {
	var mainCalls atomic.Int32
	var chatIDs []string
	var mu sync.Mutex

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		model, _ := payload["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")

		if strings.Contains(model, "VL") {
			chatID, _ := payload["chat_id"].(string)
			mu.Lock()
			chatIDs = append(chatIDs, chatID)
			mu.Unlock()
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"图片是一个红色方块\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		if mainCalls.Add(1) == 1 {
			id := extractImageID(string(body))
			args, _ := json.Marshal(map[string]any{"imageId": []string{id}, "task": "这是什么颜色"})
			frame := map[string]any{"choices": []any{map[string]any{
				"index": 0, "finish_reason": "tool_calls",
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_1", "type": "function",
					"function": map[string]any{"name": analyzeImageToolName, "arguments": string(args)},
				}}},
			}}}
			raw, _ := json.Marshal(frame)
			_, _ = io.WriteString(w, "data: "+string(raw)+"\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"这是一个红色方块\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()

	h, p := analyzeTestHandler(t, backend.URL)
	upstream.SetAccountModels("vision-a", []upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", SupportsImages: boolPtr(false)},
		{ID: "Qwen3-VL-235B", SupportsImages: boolPtr(true)},
	})

	payload := `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":64,
		"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"` + tinyPNG + `"}}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	got := append([]string(nil), chatIDs...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("应调用一次视觉模型，实际 %d", len(got))
	}
	if got[0] == "" {
		t.Fatal("视觉子请求必须带 chat_id")
	}
	if want := visionChatID(p.Accounts()[0].Name); got[0] != want {
		t.Fatalf("识图会话 id 应固定为 visionChatID(账号)，got=%q want=%q", got[0], want)
	}
}

// 撞上「并发会话数已达上限」时必须**让出槽位、等一下**再重试，而不是立刻盲目重发
// （上游的会话槽位不会因为我们重发就变空，盲重试只会连续撞墙，把并发上限误报成
// "图片识别失败"回灌给主模型）。
//
// 判据要判「机制」：只断言"重试后成功了"是假断言 —— 盲目重试也会成功。
// 这里量的是「第一次撞墙」到「第二次调用」之间的间隔：真的让出并等待，就必须 ≥ 等待时长。
func TestAnalyzeImageWaitsAfterConcurrentLimit(t *testing.T) {
	oldDelay := visionQueueRetryDelay
	visionQueueRetryDelay = 300 * time.Millisecond
	defer func() { visionQueueRetryDelay = oldDelay }()

	var mainCalls, visionCalls atomic.Int32
	var mu sync.Mutex
	var visionAt []time.Time

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		model, _ := payload["model"].(string)

		if strings.Contains(model, "VL") {
			mu.Lock()
			visionAt = append(visionAt, time.Now())
			mu.Unlock()
			if visionCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error_code":"TM.00001041","error_msg":"并发会话数已达上限(3个)，请关闭部分会话后重试。"}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"图片是一个红色方块\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if mainCalls.Add(1) == 1 {
			id := extractImageID(string(body))
			args, _ := json.Marshal(map[string]any{"imageId": []string{id}, "task": "这是什么颜色"})
			frame := map[string]any{"choices": []any{map[string]any{
				"index": 0, "finish_reason": "tool_calls",
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_1", "type": "function",
					"function": map[string]any{"name": analyzeImageToolName, "arguments": string(args)},
				}}},
			}}}
			raw, _ := json.Marshal(frame)
			_, _ = io.WriteString(w, "data: "+string(raw)+"\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"这是一个红色方块\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()

	h, p := analyzeTestHandler(t, backend.URL)
	upstream.SetAccountModels("vision-a", []upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", SupportsImages: boolPtr(false)},
		{ID: "Qwen3-VL-235B", SupportsImages: boolPtr(true)},
	})
	payload := `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":64,
		"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"` + tinyPNG + `"}}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "红色方块") {
		t.Fatalf("最终答复应包含分析结果，got=%s", rec.Body.String())
	}
	mu.Lock()
	stamps := append([]time.Time(nil), visionAt...)
	mu.Unlock()
	if len(stamps) != 2 {
		t.Fatalf("视觉调用应为 2 次（1 次撞墙 + 1 次重试），实际 %d", len(stamps))
	}
	if gap := stamps[1].Sub(stamps[0]); gap < 200*time.Millisecond {
		t.Fatalf("撞并发上限后没有等待（间隔 %v）—— 说明是盲目重试，没让出槽位", gap)
	}
	for _, st := range p.List() {
		if st["active_concurrent"] != 0 {
			t.Fatalf("识图结束后槽位没还：%#v", st)
		}
	}
}

// 客户端没带 tools 时，注入 analyze_image 之后 toolsOn 必须重算：否则模型把工具调用
// 写进正文（```tool_call 围栏，部分模型的常见输出）时，聚合路径不会做文本协议兜底，
// 调用原样当成答复返回，图片永远看不到。
func TestAnalyzeModeParsesTextualToolCallWithoutClientTools(t *testing.T) {
	var mainCalls, visionCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		model, _ := payload["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(model, "VL") {
			visionCalls.Add(1)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"红色方块\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		if mainCalls.Add(1) == 1 {
			args, _ := json.Marshal(map[string]any{"imageId": []string{extractImageID(string(body))}, "task": "颜色"})
			call, _ := json.Marshal(map[string]any{"name": analyzeImageToolName, "arguments": json.RawMessage(args)})
			text, _ := json.Marshal("```tool_call\n" + string(call) + "\n```")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":"+string(text)+"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"这是一个红色方块\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()

	h, _ := analyzeTestHandler(t, backend.URL)
	upstream.SetAccountModels("vision-a", []upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", SupportsImages: boolPtr(false)},
		{ID: "Qwen3-VL-235B", SupportsImages: boolPtr(true)},
	})
	payload := `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":64,
		"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"` + tinyPNG + `"}}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))

	if visionCalls.Load() != 1 || mainCalls.Load() != 2 {
		t.Fatalf("文本形态的 analyze_image 调用应被执行并回灌：main=%d vision=%d body=%s",
			mainCalls.Load(), visionCalls.Load(), rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "tool_call") {
		t.Fatalf("工具调用原文不应作为答复泄漏给客户端：%s", rec.Body.String())
	}
}

// 目录里没有任何视觉模型时，analyze 不改写图片（改写只会让主模型调用一个注定失败
// 的工具），而是明确回 400，且不联系上游。
func TestAnalyzeModeRejectsWhenNoVisionModel(t *testing.T) {
	var attempts atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
	}))
	defer backend.Close()

	h, _ := analyzeTestHandler(t, backend.URL)
	upstream.SetAccountModels("vision-a", []upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", SupportsImages: boolPtr(false)},
	})
	payload := `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":16,
		"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + tinyPNG + `"}}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model_not_multimodal") {
		t.Fatalf("应回 400 model_not_multimodal，got %d %s", rec.Code, rec.Body.String())
	}
	if attempts.Load() != 0 {
		t.Errorf("不应联系上游，实际 %d 次", attempts.Load())
	}
}
