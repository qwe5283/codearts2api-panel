package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

// notMultimodalErr 复刻线上真实形态：上游对纯文本模型收到带图请求回 502，
// 正文是 InferHub.001001020.406 The request model is not multimodal。
func notMultimodalErr() *upstream.ApiError {
	return &upstream.ApiError{
		Code: 502, Status: 502, Path: "/api/v2/chat/completions",
		Message: "InferHub.001001020.406 The request model is not multimodal",
	}
}

// 误用图片模型曾把健康账号冷却 10 分钟，导致后续所有正常请求 503。
// 这个错误必须归为请求形状问题：不冷却、不轮转。
func TestNotMultimodalDoesNotCoolAccount(t *testing.T) {
	a := &auth.Auth{UserID: "u-mm", UserName: "u-mm", AccessKeyID: "AK", SecretAccessKey: "SK", CloudDragonTok: "ST"}
	p, err := pool.New([]*auth.Auth{a}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(time.Second), ErrCooldown: time.Minute, ErrThreshold: 1})

	err402 := notMultimodalErr()
	if !isRequestContentError(err402) {
		t.Error("not multimodal 应归为请求内容问题")
	}
	h.handleUpstreamError(p.Get("u-mm"), "deepseek-v4.1-flash", err402)
	if !p.Healthy("u-mm") {
		t.Error("多模态错误不得冷却账号（旧版本会瘫整池 10 分钟）")
	}
}

// prompt 超过模型上下文（ModelArts.81001）同样是请求问题，绝不能冷却账号：
// 实测同一个 279k token 请求打 1M 上下文的 deepseek 成功、打 131k 的 qwen3-vl 报 81001。
func TestPromptTooLongDoesNotCoolAccount(t *testing.T) {
	a := &auth.Auth{UserID: "u-pl", UserName: "u-pl", AccessKeyID: "AK", SecretAccessKey: "SK", CloudDragonTok: "ST"}
	p, err := pool.New([]*auth.Auth{a}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(time.Second), ErrCooldown: time.Minute, ErrThreshold: 1})

	errLong := &upstream.ApiError{
		Code: 502, Status: 502, Path: "/api/v2/chat/completions",
		Message: "InferHub.ModelArts.81001.400 Bad request, Prompt length exceeds the limit",
	}
	if !isRequestContentError(errLong) {
		t.Error("prompt 超长应归为请求内容问题")
	}
	h.handleUpstreamError(p.Get("u-pl"), "qwen3-vl-235b", errLong)
	if !p.Healthy("u-pl") {
		t.Error("prompt 超长不得冷却账号")
	}
}

// 带图请求打到「目录明确声明不支持图片」的模型：预检直接回 400，列出可用的
// 视觉模型，且绝不联系上游（不静默改派、不浪费并发槽位、不冒 81001 的风险）。
func TestImageRequestToTextModelIsRejectedWithoutUpstream(t *testing.T) {
	var attempts atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()

	h, p := analyzeVisionTestHandler(t, backend.URL)
	// passthrough 模式：不替调用方做决策，明确的 400 + 可用视觉模型列表。
	h.cfg.ImageHandling = "passthrough"
	upstream.SetAccountModels("vision-a", []upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", SupportsImages: boolPtr(false)},
		{ID: "Qwen3-VL-235B", SupportsImages: boolPtr(true)},
	})

	payload := `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":16,
		"messages":[{"role":"user","content":[{"type":"text","text":"color?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))

	if attempts.Load() != 0 {
		t.Errorf("应在上游之前拦下，实际联系了上游 %d 次", attempts.Load())
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应回 400，got status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Qwen3-VL-235B") {
		t.Errorf("错误信息应列出可用的视觉模型，got=%s", body)
	}
	if !strings.Contains(body, "deepseek-v4.1-flash") {
		t.Errorf("错误信息应指出是哪个模型不支持图片，got=%s", body)
	}
	assertVisionPoolHealthy(t, p)
}

// 图片能力「未声明」（上游没给 supports_images）时不拦：无依据拒绝会挡住
// 本来可用的模型，交给上游回答。
func TestImageRequestWithUndeclaredCapabilityIsPassedThrough(t *testing.T) {
	seen := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)
		seen <- model
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()

	h, _ := analyzeVisionTestHandler(t, backend.URL)
	upstream.SetAccountModels("vision-a", []upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash"}, // SupportsImages 未声明
		{ID: "Qwen3-VL-235B", SupportsImages: boolPtr(true)},
	})

	payload := `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":16,
		"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("未声明能力时应放行，status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := <-seen; got != "deepseek-v4.1-flash" {
		t.Errorf("不得替换用户指定的模型，实际上游收到 %q", got)
	}
	if hdr := rec.Header().Get("X-Codearts-Routed-Model"); hdr != "" {
		t.Errorf("不存在改派，不应写路由头，got=%q", hdr)
	}
}

// 纯文本请求打到明确不支持图片的模型：正常放行，不得因该模型「不支持图片」被拒。
func TestTextRequestToTextModelPassesThrough(t *testing.T) {
	seen := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)
		seen <- model
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()

	h, _ := analyzeVisionTestHandler(t, backend.URL)
	upstream.SetAccountModels("vision-a", []upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", SupportsImages: boolPtr(false)},
	})

	payload := `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("纯文本请求应成功，status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := <-seen; got != "deepseek-v4.1-flash" {
		t.Errorf("纯文本请求不得被替换，实际上游收到 %q", got)
	}
}

// /v1/models 必须三态暴露图片能力：true/false 都透传，未声明则不带该字段。
func TestModelEntriesExposeImageCapability(t *testing.T) {
	entries := modelEntries([]upstream.ModelInfo{
		{ID: "vision-model", SupportsImages: boolPtr(true)},
		{ID: "text-model", SupportsImages: boolPtr(false)},
		{ID: "unknown-model"},
	})
	byID := map[string]map[string]any{}
	for _, e := range entries {
		id, _ := e["id"].(string)
		byID[id] = e
	}
	// 声明支持图片的模型：supports_images=true 且 input_modalities 含 image。
	if v, ok := byID["vision-model"]["supports_images"]; !ok || v != true {
		t.Errorf("视觉模型应透传 supports_images=true，got=%#v", byID["vision-model"]["supports_images"])
	}
	if mods, _ := byID["vision-model"]["input_modalities"].([]string); len(mods) != 2 || mods[1] != "image" {
		t.Errorf("视觉模型的 input_modalities 应含 image，got=%#v", byID["vision-model"]["input_modalities"])
	}
	// 显式声明不支持 / 未声明的模型：都不写 supports_images（客户端按「非 true 即纯文本」判断）。
	if _, ok := byID["text-model"]["supports_images"]; ok {
		t.Error("未声明支持图片时不应写 supports_images")
	}
	if _, ok := byID["unknown-model"]["supports_images"]; ok {
		t.Error("上游未声明时不应凭空写入 supports_images")
	}
	if mods, _ := byID["text-model"]["input_modalities"].([]string); len(mods) != 1 || mods[0] != "text" {
		t.Errorf("纯文本模型只声明 text 模态，got=%#v", byID["text-model"]["input_modalities"])
	}
}

// 非流式路径曾被任何聚合错误无条件冷却账号——正是它让一个超长 prompt
// 把健康账号打进 10 分钟冷却。这里验证 81001 走 400 且账号不受罚。
func TestNonStreamPromptTooLongReturns400AndKeepsPoolHealthy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "InferHub.ModelArts.81001.400 Bad request, Prompt length exceeds the limit")
	}))
	defer backend.Close()

	h, p := analyzeVisionTestHandler(t, backend.URL)
	upstream.SetAccountModels("vision-a", []upstream.ModelInfo{{ID: "deepseek-v4.1-flash", SupportsImages: boolPtr(false)}})

	payload := `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("prompt 超长应回 400，got status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertVisionPoolHealthy(t, p)
}

func boolPtr(b bool) *bool { return &b }

func analyzeVisionTestHandler(t *testing.T, endpoint string) (*Handler, *pool.Pool) {
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
	return NewHandler(Config{Pool: p, DefaultModel: "deepseek-v4.1-flash", MaxRotate: 1, ErrThreshold: 1}), p
}

func assertVisionPoolHealthy(t *testing.T, p *pool.Pool) {
	t.Helper()
	total, healthy, disabled, cooling, _ := p.Stats()
	if total != 1 || healthy != 1 || disabled != 0 || cooling != 0 {
		t.Fatalf("请求改变了账号池状态: total=%d healthy=%d disabled=%d cooling=%d", total, healthy, disabled, cooling)
	}
	for _, state := range p.List() {
		if state["err_count"] != 0 || state["active_concurrent"] != 0 {
			t.Fatalf("请求惩罚了账号或未释放并发锁: %#v", state)
		}
	}
}

// 间歇性 5xx（实测 ModelArts.81104 Conversion failed，重发即成功）不得一次就冷却：
// 阈值未到只累计，账号保持可用；连续失败到阈值才冷却。
func TestTransientUpstream5xxCoolsOnlyAtThreshold(t *testing.T) {
	a := auth.New("u-5xx", "u-5xx", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, ErrCooldown: time.Minute, ErrThreshold: 3})
	jitter := &upstream.ApiError{Code: 502, Status: 502, Path: upstream.EpChatV2, Message: "InferHub.ModelArts.81104.503 Conversion failed"}

	h.handleUpstreamError(p.Get("u-5xx"), "qwen3-vl-235b", jitter)
	if !p.Healthy("u-5xx") {
		t.Fatal("单次间歇性 5xx 不得立刻冷却账号")
	}
	for i := 0; i < 2; i++ {
		h.handleUpstreamError(p.Get("u-5xx"), "qwen3-vl-235b", jitter)
	}
	if p.Healthy("u-5xx") {
		t.Fatal("连续 3 次 5xx 应累计到阈值并冷却")
	}
}
