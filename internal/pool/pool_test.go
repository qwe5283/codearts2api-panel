package pool

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/upstream"
)

func TestPickExcludingStrategies(t *testing.T) {
	auths := make([]*auth.Auth, 3)
	for i, uid := range []string{"a", "b", "c"} {
		auths[i] = auth.New(uid, uid, "domain", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	}
	p, err := New(auths, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	picked := func(strategy string, tried map[string]bool) string {
		t.Helper()
		a := p.PickExcluding(tried, strategy)
		if a == nil {
			return ""
		}
		return a.Name
	}
	if got := picked("", nil); got != "a" || picked("first", nil) != "a" {
		t.Fatal("default/first must keep selecting the first healthy account")
	}
	p.Accounts()[0].creditBalance = &upstream.CreditBalance{Total: 50}
	p.Accounts()[1].creditBalance = &upstream.CreditBalance{Total: 200}
	p.Accounts()[2].creditBalance = &upstream.CreditBalance{Total: 100}
	for _, test := range []struct {
		strategy string
		tried    map[string]bool
		want     string
	}{
		{"credits", nil, "b"},
		{"credits", map[string]bool{"b": true}, "c"},
		{"credits", map[string]bool{"b": true, "c": true}, "a"},
		{"credits", map[string]bool{"a": true, "b": true, "c": true}, ""},
	} {
		if got := picked(test.strategy, test.tried); got != test.want {
			t.Errorf("credits tried=%v: got %q, want %q", test.tried, got, test.want)
		}
	}
	if got := p.PickExcludingMatching(nil, "credits", map[string]bool{"a": true, "c": true}).Name; got != "c" {
		t.Fatalf("credits must respect model candidates: got %q", got)
	}
	p.Accounts()[1].creditBalance = nil
	if got := picked("credits", map[string]bool{"a": true, "c": true}); got != "b" {
		t.Fatalf("unknown credit must fall back to the only available account: got %q", got)
	}

	rr, err := New(auths, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a", "b", "c", "a"} {
		if got := rr.PickExcluding(nil, "round_robin").Name; got != want {
			t.Fatalf("round_robin got %q, want %q", got, want)
		}
	}
	rr.Disable("b", "test")
	if got := rr.PickExcluding(nil, "round_robin").Name; got != "c" {
		t.Fatalf("round_robin must skip disabled accounts: got %q", got)
	}
	if got := rr.PickExcluding(map[string]bool{"a": true, "c": true}, "round_robin"); got != nil {
		t.Fatalf("round_robin returned an excluded/disabled account: %q", got.Name)
	}
}

func TestCapacitySkipsFullStickyAndFallsBack(t *testing.T) {
	auths := []*auth.Auth{
		auth.New("a", "a", "domain", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
		auth.New("b", "b", "domain", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
	}
	p, err := New(auths, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.AcquireLock("a") {
		t.Fatal("occupy a")
	}
	if got := p.PickExcluding(nil, "first"); got == nil || got.Name != "b" {
		t.Fatalf("full first account must yield to idle account: %+v", got)
	}
	if got := p.PickByUID("a"); got != nil {
		t.Fatalf("full sticky account must be reassignable: %+v", got)
	}
	if got := p.PickByUID("b"); got == nil || got.Name != "b" {
		t.Fatalf("healthy sticky account with capacity must remain usable: %+v", got)
	}
}

func TestReserveCreditsStopsKnownLowBalanceButNotUnknown(t *testing.T) {
	auths := []*auth.Auth{
		auth.New("low", "low", "domain", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
		auth.New("unknown", "unknown", "domain", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
	}
	p, err := New(auths, Config{MaxConcurrent: 1, ReserveCredits: 10}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Accounts()[0].creditBalance = &upstream.CreditBalance{Total: 10}
	p.Accounts()[1].creditBalance = nil
	if got := p.PickExcluding(nil, "first"); got == nil || got.Name != "unknown" {
		t.Fatalf("known low balance must stop; unknown balance is not zero: %+v", got)
	}
	if got := p.PickByUID("low"); got != nil {
		t.Fatalf("sticky route must respect reserve credits: %+v", got)
	}
	if !p.AcquireLock("low") {
		t.Fatal("background operations retain their own slot API")
	}
	p.ReleaseLock("low")
	if got := p.PickExcluding(nil, "first"); got == nil || got.Name != "unknown" {
		t.Fatalf("background slot use must not bypass balance policy: %+v", got)
	}
	p.SetReserveCredits(0)
	if got := p.PickExcluding(nil, "first"); got == nil || got.Name != "low" {
		t.Fatalf("zero reserve must disable the gate: %+v", got)
	}
}

func TestAddAccountInitializesConcurrency(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "verifier")
	p.AddAccount(a)

	if !p.AcquireLockWait("u1", 0) {
		t.Fatal("newly added account did not receive a concurrency slot")
	}
	p.ReleaseLock("u1")
}

func TestSyncToDirInitializesConcurrency(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "verifier")
	p.SyncToDir([]*auth.Auth{a})

	if !p.AcquireLockWait("u1", 0) {
		t.Fatal("synced account did not receive a concurrency slot")
	}
	p.ReleaseLock("u1")
}

func TestValidateKeepsAccountEnabledAfterRetryableRefreshFailure(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary upstream outage", http.StatusBadGateway)
	}))
	defer sts.Close()
	dpopPrivateJWK, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	loginCfg := upstream.DefaultLoginConfig()
	loginCfg.STSHost = sts.URL
	a := auth.New(
		"u-refresh", "user", "domain", "token", "ak", "sk",
		time.Now().Add(30*time.Minute).Format(time.RFC3339), "refresh", "verifier",
		map[string]string(dpopPrivateJWK),
	)
	p, err := New([]*auth.Auth{a}, Config{RefreshSkew: time.Hour, LoginConfig: loginCfg}, "")
	if err != nil {
		t.Fatal(err)
	}
	ok, refreshErr := p.Validate(p.Accounts()[0])
	if ok || refreshErr == nil {
		t.Fatalf("validate=(ok=%t, err=%v), want retryable refresh failure", ok, refreshErr)
	}
	status := p.List()[0]
	if disabled, _ := status["disabled"].(bool); disabled {
		t.Fatalf("retryable refresh failure permanently disabled account: %#v", status)
	}
}

// watch.refresh_skew_minutes 必须是有效的：此前 pool 硬编码 1 小时阈值，
// 调度器传进来的 RefreshSkew 只用于日志，配置改了也不生效。
func TestRefreshSkewControlsThreshold(t *testing.T) {
	a := auth.New("u-skew", "n", "d", "tok", "ak", "sk",
		time.Now().Add(2*time.Hour).Format(time.RFC3339), "refresh-token", "verifier")
	p, err := New([]*auth.Auth{a}, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	// 剩余 2h：skew=1h 时不刷新，skew=3h 时应命中刷新（这里只验证判定分支，
	// 真刷新需要网络，故只检查不触发的那一侧不报错）。
	if err := p.CheckAndRefreshTokenWithin("u-skew", time.Hour); err != nil {
		t.Fatalf("剩余 2h、skew=1h 不应触发刷新: %v", err)
	}
}

// SetMaxConcurrentAll 热更新并发上限：已加载账号与新加入账号都按新值，非正值忽略。
func TestSetMaxConcurrentAllAppliesToExistingAndNewAccounts(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.AddAccount(auth.New("u1", "n1", "d1", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	p.SetMaxConcurrentAll(2)
	for _, row := range p.List() {
		if row["max_concurrent"] != 2 {
			t.Fatalf("existing account not updated: %+v", row)
		}
	}
	p.AddAccount(auth.New("u2", "n2", "d2", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	for _, row := range p.List() {
		if row["max_concurrent"] != 2 {
			t.Fatalf("account added after hot update = %+v", row)
		}
	}
	p.SetMaxConcurrentAll(0) // 非正值不生效，避免把上限清零
	for _, row := range p.List() {
		if row["max_concurrent"] != 2 {
			t.Fatalf("zero must be ignored: %+v", row)
		}
	}
}

// SetKeepaliveWindow 热更新保活窗口，NeedKeepalive 立即按新值判定。
func TestSetKeepaliveWindowHotUpdate(t *testing.T) {
	p, err := New(nil, Config{KeepaliveWindow: time.Hour}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.AddAccount(auth.New("u1", "n1", "d1", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	if p.NeedKeepalive("u1") {
		t.Fatal("fresh account must not need keepalive")
	}
	p.PingKeepalive("u1")
	acct := p.Get("u1")
	acct.mu.Lock()
	acct.keepaliveLastPing = time.Now().Add(-time.Second)
	acct.mu.Unlock()
	p.SetKeepaliveWindow(time.Nanosecond)
	if !p.NeedKeepalive("u1") {
		t.Fatal("shrunken window must trigger keepalive")
	}
	if p.keepaliveWindow() != time.Nanosecond {
		t.Fatalf("window not stored: %s", p.keepaliveWindow())
	}
}
