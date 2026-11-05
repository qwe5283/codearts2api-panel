package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/reqlog"
)

// 重试与冷却的默认值：Default() 与 normalize() 共用同一份，避免两处漂移
// （历史上 README 写「queue_max_attempts 缺省=30」，运行时却按 1 次跑过）。
const (
	defaultSoftRate          = "60s"
	defaultErrThreshold      = 3
	defaultErrCooldown       = "10m"
	defaultQueueRetrySeconds = 10
	defaultQueueMaxAttempts  = 30
)

// Config 顶层配置。
type Config struct {
	Listen          string `json:"listen"`
	APIKey          string `json:"api_key"` // config.json 的 api_key 或 env CA2A_API_KEY（同时存在时 env 优先）
	AdminKey        string `json:"admin_key"`
	AuthDir         string `json:"auth_dir"`
	StateFile       string `json:"state_file"`
	DefaultModel    string `json:"default_model"`
	AccountStrategy string `json:"account_strategy"` // first / credits / round_robin / least_active
	// OAuthCallbackHost 可选：远程授权时把回调指向公网地址，让浏览器回调经反向
	// 代理进入本服务，而不是 127.0.0.1。只有**显式写了端口**才会改写授权链接里的
	// port 参数（如 https://oneapi.example.com:443/codearts）；不写端口则保持本机
	// 端口不变。远端回调不通时会自动回退到 ticket 轮询通道，不影响登录完成。
	OAuthCallbackHost string `json:"oauth_callback_host,omitempty"`

	Cooldown struct {
		SoftRate    string `json:"soft_rate"`
		ErrThresh   int    `json:"err_threshold"`
		ErrCooldown string `json:"err_cooldown"`
	} `json:"cooldown"`

	Watch struct {
		Enabled           bool `json:"enabled"`
		PollMinutes       int  `json:"poll_minutes"`
		RefreshSkewM      int  `json:"refresh_skew_minutes"`
		KeepaliveInterval int  `json:"keepalive_interval_minutes"` // 保活心跳间隔（新增）
	} `json:"watch"`

	MaxConcurrent   int    `json:"max_concurrent"`   // 单账号最大并发数（新增）
	KeepaliveWindow string `json:"keepalive_window"` // 保活窗口（新增）

	// ModelRefreshMinutes 模型目录后台续期巡检间隔（分钟，默认 60）。
	// 账号目录缓存 23h，这里决定「过期前多久巡检一次」；0/负数按默认 60 处理。
	// 调小则目录更早被续上（巡检更频繁），调大则续期最坏滞后更久。
	ModelRefreshMinutes int `json:"model_refresh_minutes"`

	// LoginClientID WebUI 登录使用的 OAuth client_id（留空用默认 codearts-agent）。
	LoginClientID string `json:"login_client_id"`

	// QueueRetrySeconds 上游并发/TPM 排队时的重试间隔（秒，默认 10）。
	QueueRetrySeconds int `json:"queue_retry_seconds"`
	// QueueMaxAttempts 排队重试次数上限（默认 30，约 5 分钟）。
	QueueMaxAttempts int `json:"queue_max_attempts"`

	// CreditsAutoClaim 在 checkin_hours 的时点领取每日签到和新手活动积分（默认 true）。
	CreditsAutoClaim bool `json:"credits_auto_claim"`
	// CheckinHours 每日签到时点（本地小时 0-23，默认 [9, 21]）。空值或全部非法时回落默认。
	// 轮询看护（watch.poll_minutes）与它无关：轮询管 token，时点管签到。
	CheckinHours []int `json:"checkin_hours"`

	// RequestLogRetentionDays data/requests/ 归档保留天数（按文件 mtime 计算，默认 30）。
	// 超过天数的文件由归档后台清理删除；<=0 视为未配置，回落默认值。
	RequestLogRetentionDays int `json:"request_log_retention_days"`

	// MaxBodyMB 聊天请求体上限（MB，默认 32）。
	// 多模态请求的图片是 base64 内联的：一张 2560×1440 截图就有 2~5MB，
	// 旧的硬编码 8MB 会把正常请求拒成 413（客户端只看到"太小"却没法改）。
	MaxBodyMB int `json:"max_body_mb"`

	// ImageShrink 内联图片「按需缩图 / 转 JPEG」。
	// 上游媒体转换对单张图片有硬上限（实测请求体 ≈4.3MB / 图片 ≈3.2MB），超了只能压小再发。
	ImageShrink struct {
		Enabled       bool `json:"enabled"`
		TriggerBodyMB int  `json:"trigger_body_mb"` // 请求体超过它才考虑压缩
		TargetImageMB int  `json:"target_image_mb"` // 单张图片压缩后目标
		TargetTotalMB int  `json:"target_total_mb"` // 本次请求所有图片合计目标
		MaxDimension  int  `json:"max_dimension"`   // 长边像素上限
		Quality       int  `json:"quality"`         // 初始 JPEG 质量
	} `json:"image_shrink"`

	// ImageHandling 带图请求打到纯文本模型时的处理模式：
	//   ""/"bridge"（默认）—— 识图桥接：请求进来先把图转述成文字（见 VisionBridge）；
	//   "analyze"  —— 按需识图：图片换成占位符 + 注入 analyze_image 工具，
	//                  主模型自己决定何时看、看哪张、问什么（复刻官方 AnalyzeImagePlugin）；
	//   "passthrough" —— 都不做，原样发上游（上游会回 001001020 非多模态）。
	ImageHandling string `json:"image_handling"`

	// VisionBridge 让纯文本模型也能吃图：先用 VL 模型把图片转述成文字，再交给原模型。
	// 上游只有 VL 模型支持图片输入，其余模型带图会直接 406。
	VisionBridge struct {
		Enabled        bool   `json:"enabled"`
		Model          string `json:"model"`                 // 辅助识图用的 VL 模型
		MaxImages      int    `json:"max_images"`            // 单请求最多转述几张
		MaxChars       int    `json:"max_chars"`             // 单张转述的最大字符数
		MaxTokens      int    `json:"max_tokens"`            // VL 调用输出上限
		TimeoutSeconds int    `json:"timeout_seconds"`       // 单张识图超时
		TotalSeconds   int    `json:"total_timeout_seconds"` // 整个桥接的总预算
		Prompt         string `json:"prompt,omitempty"`      // 自定义转述提示词
	} `json:"vision_bridge"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"`
	} `json:"upstream"`

	SoftRateDur    time.Duration `json:"-"`
	ErrCooldownDur time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:          ":7866",
		AuthDir:         "./auths",
		StateFile:       "./data/state.json",
		DefaultModel:    "glm-5.2",
		AccountStrategy: "first",
	}
	c.Cooldown.SoftRate = defaultSoftRate
	c.Cooldown.ErrThresh = defaultErrThreshold
	c.Cooldown.ErrCooldown = defaultErrCooldown
	// 排队重试：上游并发/TPM 排队是瞬时的，默认每 10s 重试一次、最多 30 次（约 5 分钟）。
	c.QueueRetrySeconds = defaultQueueRetrySeconds
	c.QueueMaxAttempts = defaultQueueMaxAttempts
	c.Watch.Enabled = true
	c.Watch.PollMinutes = 30
	c.Watch.RefreshSkewM = 30
	c.Watch.KeepaliveInterval = 15 // 新增：保活间隔 15 分钟
	c.MaxBodyMB = 32               // 图片 base64 内联，8MB 会把正常截图拒成 413
	c.ImageShrink.Enabled = true
	c.ImageShrink.TriggerBodyMB = 4   // 4MB 以内不碰（实测 4.29MB 上游还能过）
	c.ImageShrink.TargetImageMB = 2   // 单张压到 2MB，留足余量
	c.ImageShrink.TargetTotalMB = 4   // 多图合计也别超 4MB
	c.ImageShrink.MaxDimension = 2048 // 长边超 2048 一律缩
	c.ImageShrink.Quality = 85
	c.VisionBridge.Enabled = true
	c.VisionBridge.Model = "Qwen3-VL-235B" // 本上游可用的多模态模型之一
	c.VisionBridge.MaxImages = 8
	c.VisionBridge.MaxChars = 6000
	c.VisionBridge.MaxTokens = 1500
	c.VisionBridge.TimeoutSeconds = 120
	c.VisionBridge.TotalSeconds = 90
	c.MaxConcurrent = 2        // 上游单账号最多 3 并发会话且释放慢，留一个槽位余量
	c.KeepaliveWindow = "10m"  // 新增：保活窗口 10 分钟
	c.ModelRefreshMinutes = 60 // 模型目录后台续期巡检间隔
	c.Upstream.TimeoutSeconds = 120
	c.CreditsAutoClaim = true
	c.CheckinHours = []int{9, 21}
	c.RequestLogRetentionDays = reqlog.DefaultRetentionDays
	return c
}

// Load 加载配置 + CA2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		// 目录而非文件：Docker 单文件 bind mount 在宿主机缺文件时会创建同名目录，
		// 且每次启动都会重建，程序没有机会在那里写出配置文件（自动生成只在非挂载
		// 环境下生效）。直接 ReadFile 只会报 "Incorrect function" 之类的系统错误，
		// 这里给出真正可操作的一条路径。
		if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
			return nil, fmt.Errorf("config %s 是目录而非文件——宿主机缺少 config.json 时，"+
				"Docker 会把单文件 bind mount 的挂载点建成同名目录，并且每次启动都重建。"+
				"请在宿主机执行 `cp config.example.json config.json` 再重启容器；"+
				"只删除该目录无效（挂载点会被重新创建），程序也无法在被挂载的路径上自动生成配置", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read config: %w", err)
			}
		} else if err := json.Unmarshal(stripJSONComments(raw), c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	// 没有 API Key 就拒绝启动：旧版本回退到公开的 dummy-key-for-codearts，
	// 等于把 /v1/chat/completions 与 /v1/models 暴露给任何人（本机也可能被反代出去）。
	if c.APIKey == "" {
		return nil, fmt.Errorf("api_key 未设置：请在 config.json 写 api_key，" +
			"或设置环境变量 CA2A_API_KEY 后再启动（生成示例：openssl rand -hex 24）")
	}
	if c.AdminKey == "" {
		c.AdminKey = c.APIKey
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// stripJSONComments 去掉配置里的 // 与 /* */ 注释。
//
// config.example.json 带注释（字段说明只在那一份里），README 又让人直接
// `cp config.example.json config.json`；标准 encoding/json 不接受注释，
// 因此这里先做一次剥离。字符串内的 // 与 /* 本身不动（oauth_callback_host
// 这类值就是 URL）。
func stripJSONComments(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	inString := false
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if inString {
			out = append(out, ch)
			switch ch {
			case '\\':
				if i+1 < len(raw) {
					i++
					out = append(out, raw[i])
				}
			case '"':
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			out = append(out, ch)
			continue
		}
		if ch == '/' && i+1 < len(raw) {
			if raw[i+1] == '/' {
				for i < len(raw) && raw[i] != '\n' {
					i++
				}
				// 保留换行，尽量不改变 offset。
				if i < len(raw) {
					out = append(out, '\n')
				}
				continue
			}
			if raw[i+1] == '*' {
				i += 2
				for i+1 < len(raw) && !(raw[i] == '*' && raw[i+1] == '/') {
					i++
				}
				i++ // 跳过结尾的 '/'
				continue
			}
		}
		out = append(out, ch)
	}
	return out
}

func applyEnv(c *Config) {
	if v := os.Getenv("CA2A_ADMIN_KEY"); v != "" {
		c.AdminKey = v
	}
	if v := os.Getenv("CA2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("CA2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("CA2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("CA2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("CA2A_DEFAULT_MODEL"); v != "" {
		c.DefaultModel = v
	}
	if v := os.Getenv("CA2A_OAUTH_CALLBACK_HOST"); v != "" {
		c.OAuthCallbackHost = v
	}
	if v := os.Getenv("CA2A_WATCH_ENABLED"); v != "" {
		c.Watch.Enabled = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("CA2A_WATCH_POLL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Watch.PollMinutes = n
		}
	}
	if v := os.Getenv("CA2A_WATCH_REFRESH_SKEW"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Watch.RefreshSkewM = n
		}
	}
	if v := os.Getenv("CA2A_WATCH_KEEPALIVE_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Watch.KeepaliveInterval = n
		}
	}
	if v := os.Getenv("CA2A_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxConcurrent = n
		}
	}
	if v := os.Getenv("CA2A_MAX_BODY_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxBodyMB = n
		}
	}
	if v := os.Getenv("CA2A_KEEPALIVE_WINDOW"); v != "" {
		c.KeepaliveWindow = v
	}
	if v := os.Getenv("CA2A_LOGIN_CLIENT_ID"); v != "" {
		c.LoginClientID = v
	}
	if v := os.Getenv("CA2A_QUEUE_RETRY_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.QueueRetrySeconds = n
		}
	}
	if v := os.Getenv("CA2A_QUEUE_MAX_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.QueueMaxAttempts = n
		}
	}
	if v := os.Getenv("CA2A_CREDITS_AUTO_CLAIM"); v != "" {
		c.CreditsAutoClaim = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("CA2A_CHECKIN_HOURS"); v != "" {
		if hours, ok := parseHours(v); ok {
			c.CheckinHours = hours
		}
	}
	if v := os.Getenv("CA2A_REQUEST_LOG_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.RequestLogRetentionDays = n
		}
	}
}

// parseHours 解析 "9,21" 形式的时点列表；没有任何合法值时报 not ok。
func parseHours(raw string) ([]int, bool) {
	nums := make([]int, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			nums = append(nums, n)
		}
	}
	hours := validHours(nums)
	return hours, len(hours) > 0
}

// validHours 过滤 0-23 之外的值、去重并升序（scheduler 侧有同义的规范化，
// 但配置要在面板里就呈现出最终生效的值，不能等调度器再改）。
func validHours(hours []int) []int {
	seen := make(map[int]bool, len(hours))
	out := make([]int, 0, len(hours))
	for _, h := range hours {
		if h < 0 || h > 23 || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	sort.Ints(out)
	return out
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("api_key must not be empty")
	}
	if c.AdminKey == "" {
		c.AdminKey = c.APIKey
	}
	c.normalizeAndLog()
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen must be host:port: %w", err)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("listen port must be between 1 and 65535")
	}
	if strings.TrimSpace(c.AuthDir) == "" || strings.TrimSpace(c.StateFile) == "" {
		return fmt.Errorf("auth_dir and state_file must not be empty")
	}
	if duration, err := time.ParseDuration(c.KeepaliveWindow); err != nil || duration <= 0 {
		return fmt.Errorf("keepalive_window must be a positive duration")
	}
	if c.MaxConcurrent < 1 {
		return fmt.Errorf("max_concurrent must be positive")
	}
	// 重试与冷却的取值不在这里拦：normalize 已把 0/负数/解析不了的值回写成默认值，
	// 拒绝保存只会让「填了 0」的人卡在 400 上，反而看不到真实生效的是什么。
	return nil
}

// normalizeAndLog 归一化并把被回写的字段写进服务日志（启动与保存两条路径共用）。
func (c *Config) normalizeAndLog() {
	for _, change := range c.normalize() {
		log.Printf("config: %s 不是合法取值，已回写默认值", change)
	}
}

// normalize 把运行期不可能生效的配置值回写成默认值，返回被改写的字段说明。
//
// 口径：落盘的 = 真正会生效的。此前 queue_max_attempts 填 0 会被请求路径静默钳成
// 「1 次」、err_threshold 填 0 会被当成 3，而 config.json 里仍留着 0，运维从文件上
// 完全看不出真实行为；现在 0/负数/解析不了的时长一律按「没配置」处理，回写默认值
// （保存时由 settings.update 把结果落盘，面板表单随即显示回写后的值）。
func (c *Config) normalize() []string {
	changes := []string{}
	if dur, err := time.ParseDuration(c.Cooldown.SoftRate); err != nil || dur <= 0 {
		changes = append(changes, fmt.Sprintf("cooldown.soft_rate=%q → %s", c.Cooldown.SoftRate, defaultSoftRate))
		c.Cooldown.SoftRate = defaultSoftRate
	}
	if dur, err := time.ParseDuration(c.Cooldown.ErrCooldown); err != nil || dur <= 0 {
		changes = append(changes, fmt.Sprintf("cooldown.err_cooldown=%q → %s", c.Cooldown.ErrCooldown, defaultErrCooldown))
		c.Cooldown.ErrCooldown = defaultErrCooldown
	}
	// 解析一次即可：上面已把非法值换成了合法默认值。
	c.SoftRateDur, _ = time.ParseDuration(c.Cooldown.SoftRate)
	c.ErrCooldownDur, _ = time.ParseDuration(c.Cooldown.ErrCooldown)
	if c.Cooldown.ErrThresh <= 0 {
		changes = append(changes, fmt.Sprintf("cooldown.err_threshold=%d → %d", c.Cooldown.ErrThresh, defaultErrThreshold))
		c.Cooldown.ErrThresh = defaultErrThreshold
	}
	if c.QueueRetrySeconds <= 0 {
		changes = append(changes, fmt.Sprintf("queue_retry_seconds=%d → %d", c.QueueRetrySeconds, defaultQueueRetrySeconds))
		c.QueueRetrySeconds = defaultQueueRetrySeconds
	}
	if c.QueueMaxAttempts <= 0 {
		changes = append(changes, fmt.Sprintf("queue_max_attempts=%d → %d", c.QueueMaxAttempts, defaultQueueMaxAttempts))
		c.QueueMaxAttempts = defaultQueueMaxAttempts
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "glm-5.2"
	}
	switch c.AccountStrategy {
	case "", "first", "credits", "round_robin", "least_active":
		if c.AccountStrategy == "" {
			c.AccountStrategy = "first"
		}
	default:
		changes = append(changes, fmt.Sprintf("account_strategy=%q → first", c.AccountStrategy))
		c.AccountStrategy = "first"
	}
	if c.Listen == "" {
		c.Listen = ":7866"
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	if c.Watch.PollMinutes <= 0 {
		c.Watch.PollMinutes = 30
	}
	if c.Watch.RefreshSkewM <= 0 {
		c.Watch.RefreshSkewM = 30
	}
	if c.Watch.KeepaliveInterval <= 0 {
		c.Watch.KeepaliveInterval = 15 // 默认保活间隔 15 分钟
	}
	if c.ModelRefreshMinutes <= 0 {
		c.ModelRefreshMinutes = 60 // 默认模型目录续期巡检间隔 60 分钟
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 2 // 上游单账号并发会话上限 3 且释放慢，留一个槽位余量
	}
	// 签到时点：非法值全部滤掉；一个都不剩就回落默认 [9, 21]（与「不配置」同义）。
	if hours := validHours(c.CheckinHours); len(hours) > 0 {
		c.CheckinHours = hours
	} else {
		c.CheckinHours = []int{9, 21}
	}
	// 归档保留天数：0/负数等同于没配置，回落 30 天。不设上限——归档清理按文件
	// mtime 逐轮执行，填很大的值只是不删文件，容量仍由 MaxBytes 兜底。
	if c.RequestLogRetentionDays <= 0 {
		c.RequestLogRetentionDays = reqlog.DefaultRetentionDays
	}
	// 请求体上限：0/负数按没配置处理；上限 512MB 只是防手滑填太大。
	if c.MaxBodyMB <= 0 || c.MaxBodyMB > 512 {
		changes = append(changes, fmt.Sprintf("max_body_mb=%d → 32", c.MaxBodyMB))
		c.MaxBodyMB = 32
	}
	// 缩图参数：非法值分别回写默认，别让一个错值把整段策略废掉。
	if c.ImageShrink.TriggerBodyMB <= 0 {
		c.ImageShrink.TriggerBodyMB = 4
	}
	if c.ImageShrink.TargetImageMB <= 0 {
		c.ImageShrink.TargetImageMB = 2
	}
	if c.ImageShrink.TargetTotalMB <= 0 {
		c.ImageShrink.TargetTotalMB = 4
	}
	if c.ImageShrink.MaxDimension <= 0 {
		c.ImageShrink.MaxDimension = 2048
	}
	if c.ImageShrink.Quality <= 0 || c.ImageShrink.Quality > 100 {
		c.ImageShrink.Quality = 85
	}
	// 图像处理模式：非法值回写默认（bridge），与其它字段同一口径。
	switch c.ImageHandling {
	case "", "bridge", "analyze", "passthrough":
		if c.ImageHandling == "" {
			c.ImageHandling = "bridge"
		}
	default:
		changes = append(changes, fmt.Sprintf("image_handling=%q → bridge", c.ImageHandling))
		c.ImageHandling = "bridge"
	}
	// 识图桥接：空模型名等于没配，回落默认 VL 模型。
	if strings.TrimSpace(c.VisionBridge.Model) == "" {
		c.VisionBridge.Model = "Qwen3-VL-235B"
	}
	if c.VisionBridge.MaxImages <= 0 || c.VisionBridge.MaxImages > 32 {
		c.VisionBridge.MaxImages = 8
	}
	if c.VisionBridge.MaxChars <= 0 || c.VisionBridge.MaxChars > 100000 {
		c.VisionBridge.MaxChars = 6000
	}
	if c.VisionBridge.MaxTokens <= 0 {
		c.VisionBridge.MaxTokens = 1500
	}
	if c.VisionBridge.TimeoutSeconds <= 0 {
		c.VisionBridge.TimeoutSeconds = 120
	}
	if c.VisionBridge.TotalSeconds <= 0 {
		c.VisionBridge.TotalSeconds = 90
	}
	return changes
}

// ToPoolConfig 转换为 pool.Config。
func (c *Config) ToPoolConfig() pool.Config {
	kw := c.KeepaliveWindow
	if kw == "" {
		kw = "10m"
	}
	dur, err := time.ParseDuration(kw)
	if err != nil {
		dur = 10 * time.Minute
	}
	return pool.Config{
		ErrThreshold:    c.Cooldown.ErrThresh,
		ErrCooldown:     c.ErrCooldownDur,
		SoftCooldown:    c.SoftRateDur,
		RefreshSkew:     time.Duration(c.Watch.RefreshSkewM) * time.Minute,
		MaxConcurrent:   c.MaxConcurrent,
		KeepaliveWindow: dur,
	}
}
