package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/imgshrink"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

func shrinkHandler(t *testing.T, endpoint string, policy *imgshrink.Policy) *Handler {
	t.Helper()
	auths := []*auth.Auth{
		auth.New(t.Name()+"-a", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
	}
	p, err := pool.New(auths, pool.Config{MaxConcurrent: 1, ErrThreshold: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range p.Accounts() {
		acct.Client = upstream.NewWithChatEndpoint(time.Second, endpoint)
	}
	return NewHandler(Config{Pool: p, DefaultModel: "glm-5.2", MaxRotate: 2, ErrThreshold: 1, ImageShrink: policy})
}

// captureBackend 记录上游真实收到的请求体，并回一个最小可用的 SSE 响应。
func captureBackend(t *testing.T) (*httptest.Server, *[]byte) {
	t.Helper()
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	return srv, &got
}

func noisePNGBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(5))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func imagePayload(b64 string) string {
	return fmt.Sprintf(`{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}],"stream":false}`, b64)
}

// 上游实际收到的必须是**压缩后**的图片：JPEG 且明显变小。
func TestChatShrinksOversizeImageBeforeUpstream(t *testing.T) {
	raw := noisePNGBytes(t, 700, 700) // ≈1.2MB，噪点
	origB64 := base64.StdEncoding.EncodeToString(raw)
	backend, got := captureBackend(t)
	defer backend.Close()

	h := shrinkHandler(t, backend.URL, &imgshrink.Policy{
		TriggerBytes:     100 << 10,
		TargetImageBytes: 200 << 10, TargetTotalBytes: 200 << 10,
		MaxDimension: 2048, Quality: 85,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(imagePayload(origB64))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var sent map[string]any
	if err := json.Unmarshal(*got, &sent); err != nil {
		t.Fatalf("上游请求体解析失败: %v", err)
	}
	url := firstImageURL(t, sent)
	if !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Fatalf("上游收到的不是 JPEG：%.48s", url)
	}
	_, payload, _ := imgshrink.IsDataURL(url)
	shrunk, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	if len(shrunk) > 200<<10 {
		t.Fatalf("压缩后仍有 %d 字节，超过目标", len(shrunk))
	}
	if len(shrunk) >= len(raw) {
		t.Fatalf("没有变小：before=%d after=%d", len(raw), len(shrunk))
	}
}

// 小图原样透传：一个字节都不能变。
func TestChatKeepsSmallImageUntouched(t *testing.T) {
	raw := noisePNGBytes(t, 64, 64)
	origB64 := base64.StdEncoding.EncodeToString(raw)
	backend, got := captureBackend(t)
	defer backend.Close()

	h := shrinkHandler(t, backend.URL, &imgshrink.Policy{
		TriggerBytes:     4 << 20,
		TargetImageBytes: 2 << 20, TargetTotalBytes: 4 << 20,
		MaxDimension: 2048, Quality: 85,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(imagePayload(origB64))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(*got, &sent); err != nil {
		t.Fatalf("上游请求体解析失败: %v", err)
	}
	if url := firstImageURL(t, sent); url != "data:image/png;base64,"+origB64 {
		t.Fatalf("小图被改动了：%.60s", url)
	}
}

// 关掉开关就必须完全照原样转发（回滚路径）。
func TestChatImageShrinkDisabledPassesThrough(t *testing.T) {
	raw := noisePNGBytes(t, 700, 700)
	origB64 := base64.StdEncoding.EncodeToString(raw)
	backend, got := captureBackend(t)
	defer backend.Close()

	h := shrinkHandler(t, backend.URL, nil) // nil = 未配置/关闭
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(imagePayload(origB64))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(*got, &sent); err != nil {
		t.Fatalf("上游请求体解析失败: %v", err)
	}
	if url := firstImageURL(t, sent); url != "data:image/png;base64,"+origB64 {
		t.Fatalf("关闭后仍被压缩了：%.60s", url)
	}
}

func firstImageURL(t *testing.T, body map[string]any) string {
	t.Helper()
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		parts, _ := msg["content"].([]any)
		for _, p := range parts {
			part, _ := p.(map[string]any)
			if kind, _ := part["type"].(string); kind != "image_url" {
				continue
			}
			img, _ := part["image_url"].(map[string]any)
			if url, ok := img["url"].(string); ok {
				return url
			}
		}
	}
	t.Fatal("上游请求里找不到 image_url")
	return ""
}

// 同一张图在后续轮次里会重复出现（客户端每轮重发整段历史）。
// 第二次必须命中缓存：结果逐字节一致，且缓存里有这个键。
func TestShrinkResultIsCached(t *testing.T) {
	raw := noisePNGBytes(t, 600, 600)
	origB64 := base64.StdEncoding.EncodeToString(raw)
	policy := &imgshrink.Policy{
		TriggerBytes:     100 << 10,
		TargetImageBytes: 200 << 10, TargetTotalBytes: 200 << 10,
		MaxDimension: 2048, Quality: 85,
	}

	run := func() string {
		backend, got := captureBackend(t)
		defer backend.Close()
		h := shrinkHandler(t, backend.URL, policy)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(imagePayload(origB64))))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var sent map[string]any
		if err := json.Unmarshal(*got, &sent); err != nil {
			t.Fatalf("上游请求体解析失败: %v", err)
		}
		return firstImageURL(t, sent)
	}

	first := run()
	key := cacheKey("data:image/png;base64,"+origB64, imgshrink.Options{
		TargetBytes: 200 << 10, MaxDimension: 2048, Quality: 85,
	})
	if _, ok := imageCache.get(key); !ok {
		t.Fatal("压完的结果没有进缓存，下一轮会白烧一次解码")
	}
	second := run()
	if first != second {
		t.Fatal("同一张图两次压缩结果不一致（缓存键漏了参数或压缩不确定）")
	}

	// 关键判据：把缓存值换成哨兵，第二次必须原样把它转给上游 —— 这才证明「真走了缓存」，
	// 而不是每次都重算（重算也能得到相同结果，那种断言等于没写）。
	sentinel := "data:image/jpeg;base64,U0VOVElORUw="
	imageCache.mu.Lock()
	imageCache.items[key] = sentinel
	imageCache.mu.Unlock()
	if third := run(); third != sentinel {
		t.Fatalf("第二次没有走缓存，实际发出的是：%.60s", third)
	}
}
