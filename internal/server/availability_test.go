package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

// 可用性缓存：过期视为未知；写入后按账号+模型隔离。
func TestAvailabilityCacheScoping(t *testing.T) {
	markUnusable("acct-A", "GLM-5.2", "未注册")
	if st, _ := modelAvailability("acct-A", "GLM-5.2"); st != availUnusable {
		t.Fatal("应记录为不可用")
	}
	if st, _ := modelAvailability("acct-A", "glm-5.2"); st != availUnusable {
		t.Error("大小写不敏感")
	}
	if st, _ := modelAvailability("acct-B", "GLM-5.2"); st != availUnknown {
		t.Error("可用性必须按账号隔离，不能跨账号污染")
	}
	markUsable("acct-A", "GLM-5.2")
	if st, _ := modelAvailability("acct-A", "GLM-5.2"); st != availUsable {
		t.Error("成功调用后应覆盖为可用")
	}
}

// 过期的结论不再生效，避免套餐变化后一直按旧结论办事。
func TestAvailabilityExpiry(t *testing.T) {
	markUsable("acct-exp", "m1")
	availability.Lock()
	e := availability.byKey[availKey("acct-exp", "m1")]
	e.at = time.Now().Add(-availabilityTTL - time.Minute)
	availability.byKey[availKey("acct-exp", "m1")] = e
	availability.Unlock()
	if st, _ := modelAvailability("acct-exp", "m1"); st != availUnknown {
		t.Error("过期结论应视为未知")
	}
}

// 过滤语义：只有「所有健康账号都判定不可用」才下架；只要还有账号没结论，
// 或者有一个账号可用，就必须保留（不能把还没结论的可用模型藏起来）。
func TestModelKnownUnusable(t *testing.T) {
	p, err := pool.New([]*auth.Auth{
		{UserID: "u1", UserName: "u1"}, {UserID: "u2", UserName: "u2"},
	}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(time.Second)})

	if h.modelKnownUnusable("m-keep") {
		t.Error("无结论时不应下架")
	}
	markUnusable("u1", "m-keep", "未注册")
	if h.modelKnownUnusable("m-keep") {
		t.Error("还有账号没结论时不应下架")
	}
	markUnusable("u2", "m-keep", "未注册")
	if !h.modelKnownUnusable("m-keep") {
		t.Error("全部账号都不可用时应下架")
	}
	markUsable("u2", "m-keep")
	if h.modelKnownUnusable("m-keep") {
		t.Error("有账号可用时不应下架")
	}
}

// 端到端：真实请求报 not registered 后，该模型对这个账号被记为不可用；
// 池里唯一的健康账号也调不通时，/v1/models 不再列出它，其他模型照列。
func TestNotRegisteredHidesModelFromList(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error_code":"InferHub.002002009.404","error_msg":"The model is not registered, please request other model"}`)
	}))
	defer backend.Close()
	p := singleAccountPool(t, "u-hide", backend.URL)
	upstream.SetAccountModels("u-hide", []upstream.ModelInfo{{ID: "ghost-model"}, {ID: "real-model"}})
	// 合并展示列表有进程级缓存：进出都清掉，既不读别人的快照，也不给别人留下快照。
	resetModels := func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids, dynamicModelsCache.fetched = nil, time.Time{}
		dynamicModelsCache.Unlock()
	}
	resetModels()
	t.Cleanup(resetModels)
	h := NewHandler(Config{Pool: p, APIKey: "k", MaxRotate: 1, ErrThreshold: 1})

	if rec := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("ghost-model"), "k"); rec.Code != http.StatusNotFound {
		t.Fatalf("未注册模型应回 404，got %d %s", rec.Code, rec.Body.String())
	}
	if !p.Healthy("u-hide") {
		t.Fatal("模型级错误不得冷却账号")
	}
	ids := map[string]bool{}
	for _, e := range h.modelList(false) {
		id, _ := e["id"].(string)
		ids[id] = true
	}
	if ids["ghost-model"] || !ids["real-model"] {
		t.Fatalf("应隐藏 ghost-model、保留 real-model，got %v", ids)
	}
}
