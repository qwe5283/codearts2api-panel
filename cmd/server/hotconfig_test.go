package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/livecfg"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/reqlog"
	"codearts2api-panel/internal/scheduler"
	"codearts2api-panel/internal/upstream"
)

type fixedBalanceClient float64

func (c fixedBalanceClient) FetchAccountInfo(context.Context, upstream.SignCredential) (*upstream.CreditBalance, error) {
	return &upstream.CreditBalance{Total: float64(c)}, nil
}

// 只有装配期依赖的四个字段需要重启，其余都必须热生效。
func TestRestartRequiredFieldsAreAssemblyTimeOnly(t *testing.T) {
	want := []string{"listen", "auth_dir", "state_file", "upstream.timeout_seconds"}
	got := restartRequiredFields()
	if len(got) != len(want) {
		t.Fatalf("restart fields = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("restart fields = %v, want %v", got, want)
		}
	}
}

// 保存后热应用：快照、并发上限、调度参数一起换新，且不清空密码类字段。
func TestApplyHotConfigUpdatesLiveAndPool(t *testing.T) {
	c := Default()
	c.APIKey = "api-2"
	c.AdminKey = "admin-2"
	c.DefaultModel = "model-hot"
	c.AccountStrategy = "round_robin"
	c.SoftRateDur = 90 * time.Second
	c.ErrCooldownDur = 15 * time.Minute
	c.Cooldown.ErrThresh = 4
	c.QueueRetrySeconds = 3
	c.QueueMaxAttempts = 7
	c.KeepaliveWindow = "5m"
	c.MaxConcurrent = 2
	c.SessionTTL = "15m"
	c.ReserveCredits = 12.5
	c.LoginClientID = "client-hot"
	c.OAuthCallbackHost = "https://example.com/cb"
	c.RequestLogRetentionDays = 7

	p, err := pool.New(nil, pool.Config{MaxConcurrent: 1, BalanceClient: fixedBalanceClient(5)}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.AddAccount(auth.New("u1", "n1", "d1", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	if _, err := p.RefreshBalance(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	sch := scheduler.New(scheduler.Config{Pool: p, Enabled: true, PollInterval: time.Hour})
	live := livecfg.New(livecfg.Snapshot{APIKey: "api-1", DefaultModel: "model-old"})
	logs := reqlog.New(reqlog.Config{Enabled: true, Dir: t.TempDir(), RetentionDays: 30})
	defer logs.Close()

	restart := applyHotConfig(c, live, p, sch, logs)
	if len(restart) != 4 {
		t.Fatalf("hot apply must report four restart fields, got %v", restart)
	}

	snap := live.Load()
	if snap.APIKey != "api-2" || snap.AdminKey != "admin-2" || snap.DefaultModel != "model-hot" {
		t.Fatalf("live snapshot not applied: %+v", snap)
	}
	if snap.AccountStrategy != "round_robin" {
		t.Fatalf("account strategy not applied: %+v", snap)
	}
	if snap.QueueRetryDelay != 3*time.Second || snap.QueueMaxAttempts != 7 {
		t.Fatalf("queue settings not applied: %+v", snap)
	}
	if snap.SessionTTL != 15*time.Minute {
		t.Fatalf("session TTL not applied: %+v", snap)
	}
	if snap.SoftCooldown != 90*time.Second || snap.ErrCooldown != 15*time.Minute || snap.ErrThreshold != 4 {
		t.Fatalf("cooldown settings not applied: %+v", snap)
	}
	if snap.LoginClientID != "client-hot" || snap.OAuthCallbackHost != "https://example.com/cb" {
		t.Fatalf("oauth settings not applied: %+v", snap)
	}
	for _, row := range p.List() {
		if row["max_concurrent"] != 2 {
			t.Fatalf("pool max_concurrent not hot-updated: %+v", row)
		}
	}
	if got := p.PickExcluding(nil, "first"); got != nil {
		t.Fatalf("reserve_credits=12.5 must stop account with cached balance 5: %+v", got)
	}
}

// 归档保留天数必须热生效：面板把 30 天改成 7 天后，后台清理按新口径删掉超期文件。
// 清理每秒一轮，这里留 5 秒窗口等它跑到（不直接调 prune，测的是真实后台路径）。
func TestApplyHotConfigPrunesArchiveWithNewRetention(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "requests-2026-09-01.jsonl")
	if err := os.WriteFile(stale, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tenDaysAgo := time.Now().AddDate(0, 0, -10)
	if err := os.Chtimes(stale, tenDaysAgo, tenDaysAgo); err != nil {
		t.Fatal(err)
	}

	logs := reqlog.New(reqlog.Config{Enabled: true, Dir: dir, RetentionDays: 30})
	defer logs.Close()

	c := Default()
	c.RequestLogRetentionDays = 7
	p, err := pool.New(nil, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	applyHotConfig(c, livecfg.New(livecfg.Snapshot{}), p, scheduler.New(scheduler.Config{Pool: p, PollInterval: time.Hour}), logs)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(stale); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("保留 7 天后，10 天前的归档应在 5 秒内被后台清理删除")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// queue_retry_seconds 为 0（不配置）时沿用 10s 默认，与启动路径同口径。
func TestLiveSnapshotQueueRetryDefaultsToTenSeconds(t *testing.T) {
	c := Default()
	snap := liveSnapshot(c)
	if snap.QueueRetryDelay != 10*time.Second {
		t.Fatalf("queue retry delay = %s, want 10s", snap.QueueRetryDelay)
	}
	c.QueueRetrySeconds = 4
	if got := liveSnapshot(c).QueueRetryDelay; got != 4*time.Second {
		t.Fatalf("queue retry delay = %s, want 4s", got)
	}
}

// 多模态三块必须从配置文件如实带进运行期快照。
// 线上曾经漏了 ImageShrinkEnabled（策略里的 Enabled 没被赋值），
// 结果缩图静默不生效、只有大图才会被发现 —— 这条断言就是钉住那条接线的。
func TestLiveSnapshotCarriesMultimodalPolicy(t *testing.T) {
	c := Default()
	c.ImageShrink.Enabled = true
	c.ImageShrink.TriggerBodyMB = 5
	c.ImageShrink.TargetImageMB = 3
	c.ImageShrink.TargetTotalMB = 6
	c.ImageShrink.MaxDimension = 1600
	c.ImageShrink.Quality = 80
	c.VisionBridge.Enabled = false
	c.MaxBodyMB = 16

	snap := liveSnapshot(c)
	if !snap.ImageShrinkEnabled {
		t.Fatal("image_shrink.enabled 没进快照 → 缩图会静默失效")
	}
	if snap.ImageShrink.TriggerBytes != 5<<20 || snap.ImageShrink.TargetImageBytes != 3<<20 ||
		snap.ImageShrink.TargetTotalBytes != 6<<20 || snap.ImageShrink.MaxDimension != 1600 ||
		snap.ImageShrink.Quality != 80 {
		t.Fatalf("缩图参数没如实进快照：%+v", snap.ImageShrink)
	}
	if snap.VisionBridgeEnabled {
		t.Fatal("vision_bridge.enabled=false 必须如实反映")
	}
	if snap.VisionBridge.Model != c.VisionBridge.Model || snap.VisionBridge.MaxImages != c.VisionBridge.MaxImages {
		t.Fatalf("识图策略没进快照：%+v", snap.VisionBridge)
	}
	if snap.MaxBodyBytes != 16<<20 {
		t.Fatalf("max_body_mb 没进快照：%d", snap.MaxBodyBytes)
	}
}
