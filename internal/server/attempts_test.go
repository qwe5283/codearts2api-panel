package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

// 面板 tooltip 用的错误文本放宽到 300 字节（标签仍是 160），且不切断 UTF-8。
func TestSafeRequestErrorKeeps300Bytes(t *testing.T) {
	if got := safeRequestError(strings.Repeat("x", 1000)); len(got) != errorTextLimit {
		t.Errorf("ASCII 错误应截到 %d 字节，got=%d", errorTextLimit, len(got))
	}
	han := safeRequestError(strings.Repeat("错", 200)) // 600 字节
	if !utf8.ValidString(han) || len(han) != errorTextLimit || utf8.RuneCountInString(han) != 100 {
		t.Errorf("300 字节应恰好是 100 个汉字：len=%d runes=%d valid=%v",
			len(han), utf8.RuneCountInString(han), utf8.ValidString(han))
	}
	if got := safeLogLabel(strings.Repeat("m", 1000)); len(got) != logLabelLimit {
		t.Errorf("标签仍应按 %d 字节截断，got=%d", logLabelLimit, len(got))
	}
}

// 面板「尝试次数」列只留最近 50 条错误，超出按写入顺序滚动淘汰。
func TestAttemptErrorsKeepsLastEntries(t *testing.T) {
	ring := newAttemptErrors(attemptErrorLimit)
	for i := 0; i < attemptErrorLimit+7; i++ {
		ring.note(fmt.Sprintf("req-%02d", i), fmt.Sprintf("upstream %d", i))
	}
	snap := ring.snapshot()
	if len(snap) != attemptErrorLimit {
		t.Fatalf("只应保留 %d 条，got=%d", attemptErrorLimit, len(snap))
	}
	if _, ok := snap["req-00"]; ok {
		t.Error("最早的记录应被挤出")
	}
	newest := fmt.Sprintf("req-%02d", attemptErrorLimit+6)
	if snap[newest] != fmt.Sprintf("upstream %d", attemptErrorLimit+6) {
		t.Errorf("最新一条应保留，got=%q", snap[newest])
	}
}

// 同一个 request_id 重试多次只留最后一条；空 id 或空消息不占位。
func TestAttemptErrorsOverwritesSameRequest(t *testing.T) {
	ring := newAttemptErrors(2)
	ring.note("req-a", "第一次失败")
	ring.note("req-a", "第二次失败")
	ring.note("", "no id")
	ring.note("req-b", "")
	if snap := ring.snapshot(); len(snap) != 1 || snap["req-a"] != "第二次失败" {
		t.Fatalf("同一 request_id 应覆盖、且空值不占位：%v", snap)
	}
}

// 客户端错误报文按同一上限截断，且不会把汉字切成半个（否则 JSON 里会出现 U+FFFD）。
func TestTruncateMsgStopsOnUTF8Boundary(t *testing.T) {
	got := truncateMsg(strings.Repeat("错", 200), errorTextLimit) // 600 字节
	if len(got) != errorTextLimit || !utf8.ValidString(got) {
		t.Fatalf("应截到 %d 字节且保持合法 UTF-8：len=%d valid=%v", errorTextLimit, len(got), utf8.ValidString(got))
	}
	if short := truncateMsg("短消息", errorTextLimit); short != "短消息" {
		t.Errorf("未超限时不应改动，got=%q", short)
	}
	if edge := truncateMsg(strings.Repeat("错", 200), errorTextLimit+1); !utf8.ValidString(edge) || len(edge) > errorTextLimit+1 {
		t.Errorf("截断点落在汉字中间时应回退，len=%d valid=%v", len(edge), utf8.ValidString(edge))
	}
}

// 重试过的请求：/admin/api/usage 同时给出 attempts 与最后一次上游错误；
// 一次就成功的请求不写错误，也就不会在面板上弹 tooltip。
func TestUsageExposesLastUpstreamErrorForRetriedRequest(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			_, _ = io.WriteString(w, "data: {\"error_code\":\"InferHub.ModelArts.81111.429\",\"error_msg\":\"TPM limit reached\"}\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()

	a := auth.New("u-attempt", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, up.URL)
	// 排队上限 2、重试间隔 1ms：第一次吃 429 后立刻重试并成功。
	h := NewHandler(Config{Pool: p, APIKey: "k", MaxRotate: 1, QueueMaxAttempts: 2, QueueRetryDelay: time.Millisecond})

	if w := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("glm-5.2"), "k"); w.Code != http.StatusOK {
		t.Fatalf("重试后应成功，status=%d body=%s", w.Code, w.Body.String())
	}
	// 第二次请求一次就成功：不该产生新的错误记录。
	if w := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("glm-5.2"), "k"); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	rec := panelRequest(h, http.MethodGet, "/admin/api/usage", "", "k")
	if rec.Code != http.StatusOK {
		t.Fatalf("usage status=%d", rec.Code)
	}
	var payload struct {
		Recent []struct {
			RequestID string `json:"request_id"`
			Attempts  int    `json:"attempts"`
			QueueMs   int64  `json:"queue_ms"`
		} `json:"recent"`
		AttemptErrors map[string]string `json:"attempt_errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("parse usage: %v body=%s", err, rec.Body.String())
	}

	var retriedID, firstTryID string
	var retriedQueueMs, firstTryQueueMs int64
	for _, row := range payload.Recent {
		switch row.Attempts {
		case 2:
			retriedID = row.RequestID
			retriedQueueMs = row.QueueMs
		case 1:
			firstTryID = row.RequestID
			firstTryQueueMs = row.QueueMs
		}
	}
	if retriedID == "" || firstTryID == "" {
		t.Fatalf("应各有一条 attempts=2 与 attempts=1 的记录，got=%+v", payload.Recent)
	}
	if retriedQueueMs <= 0 {
		t.Errorf("重试请求应记录排队时长，got=%d", retriedQueueMs)
	}
	if firstTryQueueMs != 0 {
		t.Errorf("一次就成功的请求不应有排队时长，got=%d", firstTryQueueMs)
	}
	if msg := payload.AttemptErrors[retriedID]; !strings.Contains(msg, "81111.429") {
		t.Errorf("重试请求应带上最后一次上游错误，got=%q", msg)
	}
	if _, ok := payload.AttemptErrors[firstTryID]; ok {
		t.Error("一次就成功的请求不应记录错误")
	}
	if len(payload.AttemptErrors) != 1 {
		t.Errorf("只应有一条错误记录，got=%v", payload.AttemptErrors)
	}
}

// singleAccountPool 用给定端点建一个只有一个账号的池（测试里最贴近线上单账号形态）。
func singleAccountPool(t *testing.T, name, endpoint string) *pool.Pool {
	t.Helper()
	a := auth.New(name, "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, endpoint)
	return p
}

// concurrentLimitUpstream 模拟上游并发会话上限（TM.00001041）：HTTP 400 + 业务错误体。
func concurrentLimitUpstream(t *testing.T, calls *int32) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error_code":"TM.00001041","error_msg":"并发会话数已达上限(3个)，请关闭部分会话后重试。"}`)
	}))
	t.Cleanup(up.Close)
	return up
}

// 最后一次排队尝试之后已经没有下一次了：原实现在最后一轮仍然 sleep 一个重试间隔
// （默认 10s）才把错误交给客户端。这里用 2s 间隔，断言请求远早于它返回。
func TestQueueLimitDoesNotWaitAfterLastAttempt(t *testing.T) {
	var calls int32
	up := concurrentLimitUpstream(t, &calls)
	p := singleAccountPool(t, "u-nowait", up.URL)
	h := NewHandler(Config{Pool: p, APIKey: "k", MaxRotate: 1, QueueMaxAttempts: 1, QueueRetryDelay: 2 * time.Second})

	start := time.Now()
	rec := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("glm-5.2"), "k")
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("排队上限 1 时应只打一次上游，got=%d", got)
	}
	if elapsed >= time.Second {
		t.Fatalf("最后一次尝试后不该再等重试间隔，耗时 %s", elapsed)
	}
}

// 打到了上游、只是重试预算用完：错误要说明「试了几次、几个账号、上游原话」，
// 不能再谎报「账号都不可用（disabled/cooldown）」——账号其实是健康的。
func TestExhaustedRetriesReportUpstreamNotDisabledAccounts(t *testing.T) {
	var calls int32
	up := concurrentLimitUpstream(t, &calls)
	p := singleAccountPool(t, "u-exhausted", up.URL)
	h := NewHandler(Config{Pool: p, APIKey: "k", MaxRotate: 1, QueueMaxAttempts: 1})

	rec := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("glm-5.2"), "k")
	body := rec.Body.String()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, body)
	}
	if !strings.Contains(body, `"upstream_retries_exhausted"`) {
		t.Errorf("应报 upstream_retries_exhausted，got=%s", body)
	}
	if strings.Contains(body, "disabled/cooldown") {
		t.Errorf("账号是健康的，不该报 disabled/cooldown：%s", body)
	}
	if !strings.Contains(body, "1 attempt(s) on 1 account(s)") || !strings.Contains(body, "TM.00001041") {
		t.Errorf("应带上尝试次数、账号数与上游原始错误，got=%s", body)
	}
	if !p.Healthy("u-exhausted") {
		t.Error("并发会话上限是瞬时错误，不该冷却账号")
	}
}

// 排队重试不能无限等：默认值应远小于原来的 180×10s=30min。
func TestQueueRetryDefaultBounded(t *testing.T) {
	h := NewHandler(Config{})
	total := time.Duration(h.cfg.QueueMaxAttempts) * h.cfg.QueueRetryDelay
	if total > 10*time.Minute {
		t.Fatalf("排队重试上限 %s 过长，交互式请求会像挂死", total)
	}
	if total < time.Minute {
		t.Fatalf("排队重试上限 %s 过短，瞬时的上游排队会直接失败", total)
	}
}

// 排队重试「释放 → 等待 → 重获」时，若重获失败（槽位被别的请求拿走），本请求已不持有
// 槽位，收尾不能再释放一次：计数器是账号级的、不分持有者，多减一次就是把别人的
// 槽位还掉，账号随即被超发。
func TestQueueRetryReacquireFailureDoesNotReleaseOthersSlot(t *testing.T) {
	oldWait := queueReacquireWait
	queueReacquireWait = 200 * time.Millisecond
	defer func() { queueReacquireWait = oldWait }()

	firstCall := make(chan struct{})
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(firstCall)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error_code":"TM.00001041","error_msg":"并发会话数已达上限(3个)，请关闭部分会话后重试。"}`)
	}))
	defer up.Close()
	p := singleAccountPool(t, "u-reacquire", up.URL)
	h := NewHandler(Config{Pool: p, APIKey: "k", MaxRotate: 1, QueueMaxAttempts: 3, QueueRetryDelay: 100 * time.Millisecond})

	// 另一个请求在本请求让出槽位的间隙占住它，并一直持有到断言之后。
	occupied := make(chan bool, 1)
	go func() {
		<-firstCall
		occupied <- p.AcquireLockWait("u-reacquire", time.Second)
	}()
	rec := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("glm-5.2"), "k")
	if !<-occupied {
		t.Fatal("测试前置：没能在让出的间隙占住槽位")
	}
	defer p.ReleaseLock("u-reacquire")

	if rec.Code == http.StatusOK {
		t.Fatalf("重获失败时请求不应成功，body=%s", rec.Body.String())
	}
	if active := p.List()[0]["active_concurrent"]; active != 1 {
		t.Fatalf("另一个请求仍持有槽位，active_concurrent 应为 1，got %v（被多释放了一次）", active)
	}
}
