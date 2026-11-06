package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

const schedulingTestModel = "scheduling-test-model"

func schedulingHandler(t *testing.T, ttl time.Duration, ids ...string) (*Handler, *pool.Pool, map[string]*atomic.Int32) {
	t.Helper()
	calls := map[string]*atomic.Int32{}
	p, err := pool.New(nil, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		counter := &atomic.Int32{}
		calls[id] = counter
		upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}))
		t.Cleanup(upstreamServer.Close)
		a := auth.New(id, id, "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
		p.AddAccount(a)
		p.Get(id).Client = upstream.NewWithChatEndpoint(5*time.Second, upstreamServer.URL)
		upstream.SetAccountModels(id, []upstream.ModelInfo{{ID: schedulingTestModel}})
	}
	return NewHandler(Config{Pool: p, APIKey: "k", MaxRotate: 1, SessionTTL: ttl}), p, calls
}

func schedulingRequest(t *testing.T, h http.Handler, session string, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	if ctx == nil {
		ctx = context.Background()
	}
	body := fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"scheduling probe"}]}`, schedulingTestModel)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("X-Session-Id", session)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestFirstStrategyUsesIdleAccountInsteadOfWaitingForFullAccount(t *testing.T) {
	h, p, calls := schedulingHandler(t, time.Minute, "sched-first-a", "sched-first-b")
	if !p.AcquireLock("sched-first-a") {
		t.Fatal("occupy first account")
	}
	defer p.ReleaseLock("sched-first-a")
	rec := schedulingRequest(t, h, "first-fallback", nil)
	if rec.Code != http.StatusOK || calls["sched-first-a"].Load() != 0 || calls["sched-first-b"].Load() != 1 {
		t.Fatalf("status=%d calls a=%d b=%d body=%s", rec.Code, calls["sched-first-a"].Load(), calls["sched-first-b"].Load(), rec.Body.String())
	}
}

func TestNewSessionsPreferAccountsWithoutBindings(t *testing.T) {
	h, _, calls := schedulingHandler(t, time.Minute, "sched-new-a", "sched-new-b")
	for _, session := range []string{"new-session-a", "new-session-b"} {
		if rec := schedulingRequest(t, h, session, nil); rec.Code != http.StatusOK {
			t.Fatalf("session=%s status=%d body=%s", session, rec.Code, rec.Body.String())
		}
	}
	if calls["sched-new-a"].Load() != 1 || calls["sched-new-b"].Load() != 1 {
		t.Fatalf("two new sessions must spread over two accounts: a=%d b=%d", calls["sched-new-a"].Load(), calls["sched-new-b"].Load())
	}
}

func TestStickyFullFallsBackAndSuccessfulAccountOwnsBinding(t *testing.T) {
	h, p, calls := schedulingHandler(t, time.Minute, "sched-sticky-a", "sched-sticky-b")
	if rec := schedulingRequest(t, h, "sticky-session", nil); rec.Code != http.StatusOK {
		t.Fatalf("initial request: %d %s", rec.Code, rec.Body.String())
	}
	if !p.AcquireLock("sched-sticky-a") {
		t.Fatal("occupy sticky account")
	}
	rec := schedulingRequest(t, h, "sticky-session", nil)
	p.ReleaseLock("sched-sticky-a")
	if rec.Code != http.StatusOK || calls["sched-sticky-b"].Load() != 1 {
		t.Fatalf("full sticky account must fall back: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := schedulingRequest(t, h, "sticky-session", nil); rec.Code != http.StatusOK {
		t.Fatalf("follow-up request: %d %s", rec.Code, rec.Body.String())
	}
	if calls["sched-sticky-a"].Load() != 1 || calls["sched-sticky-b"].Load() != 2 {
		t.Fatalf("successful fallback account must retain binding: a=%d b=%d", calls["sched-sticky-a"].Load(), calls["sched-sticky-b"].Load())
	}
}

func TestSlotWaitStopsWhenDownstreamCancels(t *testing.T) {
	h, p, calls := schedulingHandler(t, time.Minute, "sched-cancel-a")
	if !p.AcquireLock("sched-cancel-a") {
		t.Fatal("occupy only account")
	}
	defer p.ReleaseLock("sched-cancel-a")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- schedulingRequest(t, h, "cancel-session", ctx) }()
	select {
	case rec := <-done:
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "client_cancelled") {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("downstream cancellation did not interrupt full-pool slot wait")
	}
	if calls["sched-cancel-a"].Load() != 0 {
		t.Fatal("canceled waiter reached upstream")
	}
}

func TestFiveSessionsUseFourSlotsAndOnePoolWaiter(t *testing.T) {
	h, p, calls := schedulingHandler(t, time.Minute, "sched-burst-a", "sched-burst-b")
	p.SetMaxConcurrentAll(2)
	started := make(chan struct{})
	var running atomic.Int32
	var startOnce sync.Once
	release := make(chan struct{})
	for _, acct := range p.Accounts() {
		counter := calls[acct.Name]
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			if running.Add(1) == 4 {
				startOnce.Do(func() { close(started) })
			}
			<-release
			running.Add(-1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}))
		defer server.Close()
		acct.Client = upstream.NewWithChatEndpoint(5*time.Second, server.URL)
	}
	// 前四个请求占满两账号，第五个只能在全池等待容量。
	var wg sync.WaitGroup
	results := make(chan int, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := schedulingRequest(t, h, fmt.Sprintf("burst-session-%d", i), nil)
			results <- rec.Code
		}(i)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(release)
		wg.Wait()
		t.Fatal("four requests did not occupy all four slots")
	}
	if got := running.Load(); got != 4 {
		close(release)
		wg.Wait()
		t.Fatalf("upstream running=%d, want 4; fifth must wait", got)
	}
	close(release)
	wg.Wait()
	close(results)
	for status := range results {
		if status != http.StatusOK {
			t.Fatalf("burst request status=%d", status)
		}
	}
	a, b := calls["sched-burst-a"].Load(), calls["sched-burst-b"].Load()
	if !((a == 3 && b == 2) || (a == 2 && b == 3)) {
		t.Fatalf("want two slots per account and one later waiter: a=%d b=%d", a, b)
	}
}

func TestConversationBindingsPersistWithLastSeen(t *testing.T) {
	h, _, _ := schedulingHandler(t, time.Minute, "sched-persist-a")
	h.cfg.ConvStateFile = filepath.Join(t.TempDir(), "chats.json")
	session := conversationIDFor(&chatRequest{Messages: []openAIMessage{{Role: "user", Text: "scheduling probe"}}}, "persist-session")
	if rec := schedulingRequest(t, h, "persist-session", nil); rec.Code != http.StatusOK {
		t.Fatalf("initial request: %d %s", rec.Code, rec.Body.String())
	}
	h2 := NewHandler(Config{Pool: h.cfg.Pool, SessionTTL: time.Minute, ConvStateFile: h.cfg.ConvStateFile})
	if got := h2.convAcct[session]; got != "sched-persist-a" || h2.convSeen[session].IsZero() {
		t.Fatalf("binding/last_seen did not survive restart: binding=%q seen=%s", got, h2.convSeen[session])
	}
}

func TestExpiredStickyBindingIsReassigned(t *testing.T) {
	h, p, calls := schedulingHandler(t, 40*time.Millisecond, "sched-ttl-a")
	if rec := schedulingRequest(t, h, "ttl-session", nil); rec.Code != http.StatusOK {
		t.Fatalf("initial request: %d %s", rec.Code, rec.Body.String())
	}
	time.Sleep(60 * time.Millisecond)
	p.AddAccount(auth.New("sched-ttl-b", "b", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	secondCalls := &atomic.Int32{}
	calls["sched-ttl-b"] = secondCalls
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstreamServer.Close()
	p.Get("sched-ttl-b").Client = upstream.NewWithChatEndpoint(5*time.Second, upstreamServer.URL)
	upstream.SetAccountModels("sched-ttl-b", []upstream.ModelInfo{{ID: schedulingTestModel}})
	if rec := schedulingRequest(t, h, "ttl-session", nil); rec.Code != http.StatusOK {
		t.Fatalf("expired binding request: %d %s", rec.Code, rec.Body.String())
	}
	if calls["sched-ttl-a"].Load() != 1 || calls["sched-ttl-b"] == nil || calls["sched-ttl-b"].Load() != 1 {
		t.Fatalf("expired binding must move to the unbound account: a=%d b=%v", calls["sched-ttl-a"].Load(), calls["sched-ttl-b"])
	}
}
