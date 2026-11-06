package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 归一化把「运行期不可能生效」的值回写成默认值，并回报改了哪些字段：
// queue_* 填 0 过去会被请求路径静默钳成 1 次，文件里却留着 0，看不出真实行为。
func TestNormalizeRewritesInvalidRetryAndCooldownValues(t *testing.T) {
	c := Default()
	c.Cooldown.SoftRate = "nope"
	c.Cooldown.ErrCooldown = "0s"
	c.Cooldown.ErrThresh = 0
	c.QueueRetrySeconds = 0
	c.QueueMaxAttempts = -2

	changes := c.normalize()
	if len(changes) != 5 {
		t.Fatalf("应回报 5 处回写，got=%v", changes)
	}
	if c.Cooldown.SoftRate != "60s" || c.SoftRateDur != time.Minute {
		t.Errorf("soft_rate 未回写: %q / %s", c.Cooldown.SoftRate, c.SoftRateDur)
	}
	if c.Cooldown.ErrCooldown != "10m" || c.ErrCooldownDur != 10*time.Minute {
		t.Errorf("err_cooldown 未回写: %q / %s", c.Cooldown.ErrCooldown, c.ErrCooldownDur)
	}
	if c.Cooldown.ErrThresh != 3 || c.QueueRetrySeconds != 10 || c.QueueMaxAttempts != 30 {
		t.Errorf("回写后的值不符: thresh=%d retry=%d attempts=%d", c.Cooldown.ErrThresh, c.QueueRetrySeconds, c.QueueMaxAttempts)
	}
	// 默认值本身不能被当成非法值反复改写（否则每次保存都会变一次）。
	if got := Default().normalize(); len(got) != 0 {
		t.Fatalf("默认配置不该产生回写: %v", got)
	}
}

func TestSessionTTLAndReserveCreditsConfig(t *testing.T) {
	t.Setenv("CA2A_API_KEY", "test-key")
	t.Setenv("CA2A_SESSION_TTL", "")
	t.Setenv("CA2A_RESERVE_CREDITS", "")
	cfg, err := Load("")
	if err != nil || cfg.SessionTTL != "30m" || cfg.ReserveCredits != 100 {
		t.Fatalf("defaults: cfg=%+v err=%v", cfg, err)
	}
	if got := liveSnapshot(cfg).SessionTTL; got != 30*time.Minute {
		t.Fatalf("default live TTL=%s", got)
	}
	t.Setenv("CA2A_SESSION_TTL", "45m")
	t.Setenv("CA2A_RESERVE_CREDITS", "12.5")
	cfg, err = Load("")
	if err != nil || cfg.SessionTTL != "45m" || cfg.ReserveCredits != 12.5 || cfg.ToPoolConfig().ReserveCredits != 12.5 {
		t.Fatalf("environment/config wiring failed: cfg=%+v err=%v", cfg, err)
	}
	if got := liveSnapshot(cfg).SessionTTL; got != 45*time.Minute {
		t.Fatalf("live TTL=%s", got)
	}
	t.Setenv("CA2A_RESERVE_CREDITS", "0")
	cfg, err = Load("")
	if err != nil || cfg.ReserveCredits != 0 {
		t.Fatalf("explicit zero must disable reserve credits: cfg=%+v err=%v", cfg, err)
	}
}

func TestAccountStrategyConfigAndLiveSnapshot(t *testing.T) {
	for _, strategy := range []string{"first", "credits", "round_robin"} {
		c := Default()
		c.AccountStrategy = strategy
		if changes := c.normalize(); len(changes) != 0 || liveSnapshot(c).AccountStrategy != strategy {
			t.Errorf("strategy %q not preserved: %v / %+v", strategy, changes, liveSnapshot(c))
		}
	}
	c := Default()
	c.AccountStrategy = "random"
	changes := c.normalize()
	if c.AccountStrategy != "first" || len(changes) != 1 {
		t.Fatalf("invalid strategy must normalize to first: %q / %v", c.AccountStrategy, changes)
	}
}

func TestCreditsAutoClaimConfigAndEnv(t *testing.T) {
	t.Setenv("CA2A_API_KEY", "test-key")
	t.Setenv("CA2A_CREDITS_AUTO_CLAIM", "")
	if cfg, err := Load(""); err != nil || !cfg.CreditsAutoClaim {
		t.Fatalf("auto claim default disabled: err=%v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"credits_auto_claim":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(path); err != nil || cfg.CreditsAutoClaim {
		t.Fatalf("config opt-out ignored: err=%v", err)
	}
	t.Setenv("CA2A_CREDITS_AUTO_CLAIM", "1")
	if cfg, err := Load(path); err != nil || !cfg.CreditsAutoClaim {
		t.Fatalf("env override ignored: err=%v", err)
	}
	t.Setenv("CA2A_CREDITS_AUTO_CLAIM", "0")
	if cfg, err := Load(""); err != nil || cfg.CreditsAutoClaim {
		t.Fatalf("env opt-out ignored: err=%v", err)
	}
}

// 仓库自带的 config.example.json 含 // 注释，README 让人直接 cp 成 config.json；
// 带注释的配置必须能加载，否则快起步就断在第一步。
func TestLoadToleratesExampleConfigComments(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("read config.example.json: %v", err)
	}
	// 没有 api_key 现在会拒绝启动，示例配置的用例显式给一个（env 优先）。
	t.Setenv("CA2A_API_KEY", "test-key")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("带注释的示例配置应能加载: %v", err)
	}
	if cfg.Listen != ":7866" || cfg.AuthDir != "./auths" || cfg.DefaultModel != "glm-5.2" {
		t.Fatalf("示例配置字段未生效: %+v", cfg)
	}
	if cfg.Cooldown.SoftRate != "60s" || cfg.MaxConcurrent != 2 || cfg.Watch.PollMinutes != 30 {
		t.Fatalf("示例配置嵌套字段未生效: %+v", cfg)
	}
	if cfg.KeepaliveWindow != "10m" {
		t.Errorf("keepalive_window 未生效: %q", cfg.KeepaliveWindow)
	}
}

// 宿主机缺 ./config.json 时，单文件 bind mount 会把挂载点建成同名目录；
// 直接 ReadFile 只会报系统错误，这里要求给出可操作的提示。
func TestLoadRejectsConfigPathThatIsDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CA2A_API_KEY", "test-key")

	_, err := Load(path)
	if err == nil {
		t.Fatal("配置路径是目录时应拒绝启动")
	}
	for _, want := range []string{"是目录而非文件", "cp config.example.json config.json", "只删除该目录无效"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("提示缺少关键信息 %q: %v", want, err)
		}
	}
	// 挂载点是目录时程序写不进配置：不能再承诺「删掉目录就会自动生成」。
	if strings.Contains(err.Error(), "会自动生成配置") {
		t.Errorf("提示不应承诺自动生成配置: %v", err)
	}
}

func TestStripJSONCommentsKeepsStrings(t *testing.T) {
	raw := []byte("{\n" +
		"  // 行注释\n" +
		"  \"oauth_callback_host\": \"https://oneapi.example.com/codearts\", /* 块注释 */\n" +
		"  \"note\": \"含 // 与 /* 的字符串\",\n" +
		"  \"n\": 1\n" +
		"}")
	var v map[string]any
	if err := json.Unmarshal(stripJSONComments(raw), &v); err != nil {
		t.Fatalf("剥离注释后应是合法 JSON: %v", err)
	}
	if v["oauth_callback_host"] != "https://oneapi.example.com/codearts" {
		t.Errorf("URL 值被破坏: %v", v["oauth_callback_host"])
	}
	if v["note"] != "含 // 与 /* 的字符串" {
		t.Errorf("字符串内容被破坏: %v", v["note"])
	}
	if v["n"] != float64(1) {
		t.Errorf("n=%v", v["n"])
	}

	// 无注释的配置保持原样。
	plain := []byte(`{"listen":":7866"}`)
	if got := string(stripJSONComments(plain)); got != string(plain) {
		t.Errorf("无注释配置被改动: %q", got)
	}
}

// 剥离器的边界：CRLF、文件末尾无换行的注释、字符串里的转义引号、
// 跨行块注释、以及孤立的 '/'。这些走错一个就会静默吃掉配置。
func TestStripJSONCommentsEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]any
	}{
		{
			name: "CRLF 与行尾注释",
			in:   "{\r\n  \"listen\": \":7866\", // 端口\r\n  \"default_model\": \"glm-5.2\"\r\n}",
			want: map[string]any{"listen": ":7866", "default_model": "glm-5.2"},
		},
		{
			name: "文件末尾注释无换行",
			in:   "{\"listen\":\":7866\"} // 结尾注释",
			want: map[string]any{"listen": ":7866"},
		},
		{
			name: "字符串内转义引号后的 //",
			in:   `{"note":"a\"//b","listen":":7866"}`,
			want: map[string]any{"note": `a"//b`, "listen": ":7866"},
		},
		{
			name: "跨行块注释",
			in:   "{\n/*\n 多行\n 说明\n*/\n\"listen\": \":7866\"\n}",
			want: map[string]any{"listen": ":7866"},
		},
		{
			name: "值后紧跟块注释",
			in:   `{"listen":"/*:7866*/","n":1/* 尾随 */}`,
			want: map[string]any{"listen": "/*:7866*/", "n": float64(1)},
		},
		{
			name: "注释里的引号不改变字符串状态",
			in:   "{\"listen\": \":7866\" // 这里的 \" 不配对\n}",
			want: map[string]any{"listen": ":7866"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			if err := json.Unmarshal(stripJSONComments([]byte(tc.in)), &got); err != nil {
				t.Fatalf("剥离注释后应是合法 JSON: %v\n输入: %s", err, tc.in)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("字段数不符: got=%v want=%v", got, tc.want)
			}
			for k, w := range tc.want {
				if got[k] != w {
					t.Errorf("%s: got=%v want=%v", k, got[k], w)
				}
			}
		})
	}

	// 孤立的 '/' 不能 panic 或死循环（结果是否合法 JSON 无所谓）。
	for _, in := range []string{"{", "/", `{"a":1}/`, `{"a":1}/*`} {
		_ = stripJSONComments([]byte(in))
	}
}

// 归档保留天数：默认 30 天；配置可改；0/负数等同于没配置（回落 30，而不是删光）；
// env 用整数覆盖。
func TestRequestLogRetentionDaysConfigAndEnv(t *testing.T) {
	t.Setenv("CA2A_API_KEY", "test-key")
	t.Setenv("CA2A_REQUEST_LOG_RETENTION_DAYS", "")
	if cfg, err := Load(""); err != nil || cfg.RequestLogRetentionDays != 30 {
		t.Fatalf("default retention = %d err=%v, want 30", cfg.RequestLogRetentionDays, err)
	}

	write := func(body string) string {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if cfg, err := Load(write(`{"request_log_retention_days":7}`)); err != nil || cfg.RequestLogRetentionDays != 7 {
		t.Fatalf("configured retention = %d err=%v, want 7", cfg.RequestLogRetentionDays, err)
	}
	for _, body := range []string{`{"request_log_retention_days":0}`, `{"request_log_retention_days":-5}`} {
		if cfg, err := Load(write(body)); err != nil || cfg.RequestLogRetentionDays != 30 {
			t.Fatalf("%s: retention = %d err=%v, want default 30", body, cfg.RequestLogRetentionDays, err)
		}
	}

	t.Setenv("CA2A_REQUEST_LOG_RETENTION_DAYS", "90")
	if cfg, err := Load(write(`{"request_log_retention_days":7}`)); err != nil || cfg.RequestLogRetentionDays != 90 {
		t.Fatalf("env override ignored: retention=%d err=%v", cfg.RequestLogRetentionDays, err)
	}
	// 非法 env 不覆盖配置文件里的值。
	t.Setenv("CA2A_REQUEST_LOG_RETENTION_DAYS", "forever")
	if cfg, err := Load(write(`{"request_log_retention_days":7}`)); err != nil || cfg.RequestLogRetentionDays != 7 {
		t.Fatalf("invalid env must keep config value: retention=%d err=%v", cfg.RequestLogRetentionDays, err)
	}
}

// 签到时点：默认 9/21，配置里允许乱序/重复/非法值，env 用 "9,21" 形式覆盖。
func TestCheckinHoursConfigAndEnv(t *testing.T) {
	t.Setenv("CA2A_API_KEY", "test-key")
	t.Setenv("CA2A_CHECKIN_HOURS", "")
	if cfg, err := Load(""); err != nil || !reflect.DeepEqual(cfg.CheckinHours, []int{9, 21}) {
		t.Fatalf("default checkin hours = %v err=%v", cfg.CheckinHours, err)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"checkin_hours":[21,9,9,25]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(path); err != nil || !reflect.DeepEqual(cfg.CheckinHours, []int{9, 21}) {
		t.Fatalf("normalized checkin hours = %v err=%v", cfg.CheckinHours, err)
	}

	// 全部非法等同于没配置：回落默认，而不是「一个时点都不跑」。
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"checkin_hours":[42,-1]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(bad); err != nil || !reflect.DeepEqual(cfg.CheckinHours, []int{9, 21}) {
		t.Fatalf("invalid hours must fall back to default: %v err=%v", cfg.CheckinHours, err)
	}

	t.Setenv("CA2A_CHECKIN_HOURS", "7,22")
	if cfg, err := Load(path); err != nil || !reflect.DeepEqual(cfg.CheckinHours, []int{7, 22}) {
		t.Fatalf("env override ignored: %v err=%v", cfg.CheckinHours, err)
	}
	// 非法 env 不覆盖配置文件里的值。
	t.Setenv("CA2A_CHECKIN_HOURS", "nine")
	if cfg, err := Load(path); err != nil || !reflect.DeepEqual(cfg.CheckinHours, []int{9, 21}) {
		t.Fatalf("invalid env must keep config value: %v err=%v", cfg.CheckinHours, err)
	}
}
