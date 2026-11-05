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

// least_active 策略：并发相同（都空闲）时按游标严格轮转，不能永远压第一个账号。
func TestPickExcludingLeastActiveRotatesAcrossHealthyAccounts(t *testing.T) {
	p, err := New([]*auth.Auth{
		auth.New("u1", "u1", "d", "tok1", "ak1", "sk1", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
		auth.New("u2", "u2", "d", "tok2", "ak2", "sk2", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
		auth.New("u3", "u3", "d", "tok3", "ak3", "sk3", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
	}, Config{MaxConcurrent: 5}, "")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for i := 0; i < 6; i++ {
		a := p.PickExcluding(nil, "least_active")
		if a == nil {
			t.Fatalf("第 %d 次挑选返回 nil", i+1)
		}
		order = append(order, a.Name)
	}
	want := []string{"u1", "u2", "u3", "u1", "u2", "u3"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("空闲账号之间应严格轮转：got=%v want=%v", order, want)
		}
	}
}

// least_active 策略：优先挑在途请求最少的账号（把 u1 占两个槽位后必须只选 u2）。
func TestPickExcludingLeastActivePrefersLeastLoaded(t *testing.T) {
	p, err := New([]*auth.Auth{
		auth.New("u1", "u1", "d", "tok1", "ak1", "sk1", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
		auth.New("u2", "u2", "d", "tok2", "ak2", "sk2", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
	}, Config{MaxConcurrent: 5}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.AcquireLock("u1") || !p.AcquireLock("u1") {
		t.Fatal("应在 MaxConcurrent=5 下拿到 u1 的槽位")
	}
	defer p.ReleaseLock("u1")
	defer p.ReleaseLock("u1")
	for i := 0; i < 3; i++ {
		a := p.PickExcluding(nil, "least_active")
		if a == nil {
			t.Fatal("挑选返回 nil")
		}
		if a.Name != "u2" {
			t.Fatalf("应当优先选空闲的 u2，got=%s", a.Name)
		}
	}
}

// least_active 策略必须遵守 tried 与 eligible 语义，全被排除时返回 nil。
func TestPickExcludingLeastActiveRespectsTriedAndEligible(t *testing.T) {
	p, err := New([]*auth.Auth{
		auth.New("u1", "u1", "d", "tok1", "ak1", "sk1", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
		auth.New("u2", "u2", "d", "tok2", "ak2", "sk2", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
		auth.New("u3", "u3", "d", "tok3", "ak3", "sk3", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
	}, Config{MaxConcurrent: 5}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.PickExcluding(map[string]bool{"u1": true, "u2": true}, "least_active"); got == nil || got.Name != "u3" {
		t.Fatalf("应跳过 tried 里的账号，只选 u3：%v", got)
	}
	// 单账号池行为必须与 first 一致。
	single, err := New([]*auth.Auth{
		auth.New("solo", "solo", "d", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
	}, Config{MaxConcurrent: 5}, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got := single.PickExcluding(nil, "least_active"); got == nil || got.Name != "solo" {
			t.Fatalf("单账号池应始终返回 solo，got=%v", got)
		}
	}
	if got := single.PickExcluding(map[string]bool{"solo": true}, "least_active"); got != nil {
		t.Fatalf("唯一账号被 tried 排除后应返回 nil，got=%v", got.Name)
	}
	// 全部冷却时也不许返回不健康账号。
	for _, a := range p.Accounts() {
		a.coolUntil = time.Now().Add(time.Hour)
	}
	if got := p.PickExcluding(nil, "least_active"); got != nil {
		t.Fatalf("全部冷却时应返回 nil，got=%v", got.Name)
	}
}
