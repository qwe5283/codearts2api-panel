package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

// anthropicTestHandler 组一个单账号 handler，上游返回给定的 SSE 帧。
func anthropicTestHandler(t *testing.T, sseFrames string) *Handler {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseFrames))
	}))
	t.Cleanup(up.Close)

	a := auth.New("anth", "user", "domain", "token", "ak", "sk",
		time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	upstream.SetAccountModels("anth", []upstream.ModelInfo{{ID: "GLM-5.2"}})
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range p.Accounts() {
		acct.Client = upstream.NewWithChatEndpoint(5*time.Second, up.URL)
	}
	return NewHandler(Config{Pool: p, APIKey: "k", DefaultModel: "glm-5.2", MaxRotate: 1})
}

func anthropicRequest(h *Handler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// 非流式：Anthropic 请求 → chat → Anthropic 响应，形状与工具调用都要对。
func TestAnthropicMessagesNonStreaming(t *testing.T) {
	frames := "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	h := anthropicTestHandler(t, frames)

	body := `{"model":"claude-sonnet-4-5","max_tokens":100,"messages":[{"role":"user","content":"你好"}]}`
	w := anthropicRequest(h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v\n%s", err, w.Body.String())
	}
	if resp.Type != "message" || resp.Role != "assistant" {
		t.Errorf("type=%q role=%q，应为 message/assistant", resp.Type, resp.Role)
	}
	if len(resp.Content) == 0 || resp.Content[0].Text != "你好" {
		t.Errorf("content=%+v，应拼出「你好」", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason=%q，stop 应映射为 end_turn", resp.StopReason)
	}
}

// 流式：必须产出 Claude Code 要求的最小事件链，且 message_start 最先、message_stop 最后。
func TestAnthropicMessagesStreamingEventChain(t *testing.T) {
	frames := "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	h := anthropicTestHandler(t, frames)

	body := `{"model":"claude-sonnet-4-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"你好"}]}`
	w := anthropicRequest(h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200: %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	// 正确线格式：Anthropic SSE 用 `event:` + `data:` 两行。
	if !strings.Contains(out, "event: message_start") {
		t.Errorf("缺 message_start 事件\n%s", out)
	}
	for _, want := range []string{
		"event: content_block_start", "event: content_block_delta",
		"event: content_block_stop", "event: message_delta", "event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺事件 %q\n%s", want, out)
		}
	}
	// 事件顺序：message_start 必须最先出现，message_stop 最后。
	iStart := strings.Index(out, "event: message_start")
	iStop := strings.LastIndex(out, "event: message_stop")
	if iStart != 0 {
		t.Errorf("message_start 不是首个事件（位置 %d）", iStart)
	}
	if iStop < iStart || iStop < 0 {
		t.Errorf("message_stop 缺失或位置异常")
	}
	// 文本增量必须出现在流里（拼起来是「你好」）。
	if !strings.Contains(out, "\"text\":\"你\"") || !strings.Contains(out, "\"text\":\"好\"") {
		t.Errorf("文本增量丢失\n%s", out)
	}
}

// 上游错误：Anthropic 客户端要拿到 Anthropic 形状的错误体，而不是先收到 200 SSE。
func TestAnthropicMessagesUpstreamErrorIsAnthropicShaped(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"模型未注册","type":"invalid_request_error"}}`))
	}))
	t.Cleanup(up.Close)
	a := auth.New("anth-err", "user", "domain", "token", "ak", "sk",
		time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	upstream.SetAccountModels("anth-err", []upstream.ModelInfo{{ID: "GLM-5.2"}})
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range p.Accounts() {
		acct.Client = upstream.NewWithChatEndpoint(5*time.Second, up.URL)
	}
	h := NewHandler(Config{Pool: p, APIKey: "k", DefaultModel: "glm-5.2", MaxRotate: 1})

	body := `{"model":"claude-sonnet-4-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	w := anthropicRequest(h, body)
	if w.Header().Get("Content-Type") == "text/event-stream" {
		t.Fatalf("错误不应以 SSE 200 形式发出: %s", w.Body.String())
	}
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("错误体不是 JSON: %v\n%s", err, w.Body.String())
	}
	if e.Type != "error" || e.Error.Type == "" {
		t.Errorf("错误体形状不对: %s", w.Body.String())
	}
}

// 模型选择：claude-* （上游无此名）回落默认模型；其余名字原样透传（含目录未发现的
// 情形——重启后目录为空时，客户端选的 kimi-k3.1-pro 不能被误换成默认模型）。
func TestAnthropicModelResolution(t *testing.T) {
	h := anthropicTestHandler(t, "data: [DONE]\n\n") // 上游不关心内容
	if got := h.resolveAnthropicModel("claude-sonnet-4-5"); got != "glm-5.2" {
		t.Errorf("claude-* 应回落默认模型，got=%q", got)
	}
	if got := h.resolveAnthropicModel("kimi-k3.1-pro"); got != "kimi-k3.1-pro" {
		t.Errorf("真实模型应原样透传，got=%q", got)
	}
	if got := h.resolveAnthropicModel(""); got != "glm-5.2" {
		t.Errorf("空 model 应回落默认，got=%q", got)
	}
}

// 非流式请求打到上游时，若客户端没显式要思考档位，不得注入 reasoning_effort——
// 否则 glm-5.3-flash（拒绝 medium）会被上游 502，Claude Code 里选它必失败。
func TestAnthropicDoesNotInjectReasoningEffort(t *testing.T) {
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(up.Close)
	a := auth.New("anth-effort", "user", "domain", "token", "ak", "sk",
		time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	upstream.SetAccountModels("anth-effort", []upstream.ModelInfo{{ID: "GLM-5.2"}})
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range p.Accounts() {
		acct.Client = upstream.NewWithChatEndpoint(5*time.Second, up.URL)
	}
	h := NewHandler(Config{Pool: p, APIKey: "k", DefaultModel: "glm-5.2", MaxRotate: 1})

	body := `{"model":"claude-sonnet-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`
	if w := anthropicRequest(h, body); w.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	if v, ok := got["reasoning_effort"]; ok {
		t.Errorf("未显式要档位时不得注入 reasoning_effort，实际=%v", v)
	}

	// 显式给了 thinking 时应当透传档位。
	got = nil
	body2 := `{"model":"claude-sonnet-4-5","max_tokens":50,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`
	_ = anthropicRequest(h, body2)
	if got == nil || got["reasoning_effort"] == nil || got["reasoning_effort"] == "" {
		t.Errorf("显式 thinking 时应透传档位，实际 reasoning_effort=%v", got["reasoning_effort"])
	}
}

// count_tokens 必须成功返回（Claude Code 拿它做上下文预算，失败会触发提前压缩）。
func TestAnthropicCountTokens(t *testing.T) {
	p, _ := pool.New(nil, pool.Config{}, "")
	h := NewHandler(Config{Pool: p, APIKey: "k"})
	r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"你好世界"}]}`))
	r.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200: %s", w.Code, w.Body.String())
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.InputTokens < 1 {
		t.Errorf("input_tokens=%d，应为正数", out.InputTokens)
	}
}
