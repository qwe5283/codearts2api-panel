// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：面板保存配置后应当立即生效，但 Handler 的配置字段是被所有请求 goroutine
// 直读的普通结构体，原地改写会 data race。这里用「不可变快照 + atomic 指针」：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁、无竞争。
//
// 只承载**请求期读取**的字段。监听地址、凭据目录、状态文件、上游 HTTP 超时等
// 装配期依赖仍留在启动配置里，改它们必须重启进程。
package livecfg

import (
	"strings"
	"sync/atomic"
	"time"

	"codearts2api-panel/internal/imgshrink"
)

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	APIKey            string
	AdminKey          string
	DefaultModel      string
	AccountStrategy   string        // 无会话绑定时：first / credits / round_robin / least_active
	SoftCooldown      time.Duration // 429 软冷却时长
	ErrThreshold      int           // 连续错误阈值
	ErrCooldown       time.Duration // 错误冷却时长
	QueueRetryDelay   time.Duration // 上游排队重试间隔
	QueueMaxAttempts  int           // 上游排队重试次数上限
	OAuthCallbackHost string        // 授权回调 host 覆盖
	LoginClientID     string        // WebUI 登录 client_id

	// 以下字段供面板 overview 如实展示调度/池参数（本身由 pool/scheduler 应用）。
	WatchEnabled             bool
	CreditsAutoClaim         bool
	PollMinutes              int
	RefreshSkewMinutes       int
	KeepaliveIntervalMinutes int
	MaxConcurrent            int
	KeepaliveWindow          string

	// ModelRefreshMinutes 模型目录后台续期巡检间隔（分钟）。0 表示未配置，用内置默认。
	ModelRefreshMinutes int

	// MaxBodyBytes 聊天请求体上限（0 表示用默认值）。
	MaxBodyBytes int64

	// ImageShrink 内联图片按需缩图/转 JPEG。
	ImageShrinkEnabled bool
	ImageShrink        imgshrink.Policy

	// VisionBridge 纯文本模型「先用 VL 转述图片」的能力补齐。
	VisionBridgeEnabled bool
	VisionBridge        VisionBridge

	// ImageHandling 带图请求打到纯文本模型时的处理模式：
	//   ""/"bridge"（默认）—— 识图桥接：请求进来先把图转述成文字（见 VisionBridge）；
	//   "analyze"  —— 按需识图：图片换成占位符 + 注入 analyze_image 工具，
	//                  主模型自己决定何时看、看哪张、问什么（复刻官方 AnalyzeImagePlugin）；
	//   "passthrough" —— 都不做，原样发上游（上游会回 001001020 非多模态）。
	//
	// analyze 与 bridge 的取舍：bridge 是一次性泛化转述，描述会占上下文且无法追问；
	// analyze 对 Agent 类客户端更友好（可多轮追问、可精确指定看图任务），
	// 但需要客户端支持工具调用。默认 bridge 保持与既有行为一致。
	ImageHandling string
}

// VisionBridge 识图桥接策略。
type VisionBridge struct {
	Enabled      bool
	Model        string        // 辅助识图的 VL 模型
	MaxImages    int           // 单请求最多转述几张
	MaxChars     int           // 单张转述的最大字符数
	MaxTokens    int           // VL 调用输出上限
	Timeout      time.Duration // 单张识图超时
	TotalTimeout time.Duration // 整个桥接过程的总预算
	Prompt       string        // 自定义转述提示词
}

// DefaultVisionBridge 默认策略：Qwen3-VL-235B、最多 8 张、单张 6000 字、总预算 90s。
func DefaultVisionBridge() VisionBridge {
	return VisionBridge{
		Enabled:      true,
		Model:        "Qwen3-VL-235B",
		MaxImages:    8,
		MaxChars:     6000,
		MaxTokens:    1500,
		Timeout:      120 * time.Second,
		TotalTimeout: 90 * time.Second,
	}
}

// DefaultVisionPrompt 默认的图片转述提示词：先概括，再逐行转写文字，最后描述布局。
const DefaultVisionPrompt = `你是「图片转述器」。请把这张图片转成**详尽的中文文字**，供只能读文字的模型使用：
1. 先用一句话概括这是什么（应用界面截图 / 网页截图 / 照片 / 图表 / 报错弹窗 / 代码等）；
2. 然后**逐行转写画面里所有可见文字**（按钮、菜单、标题、正文、报错原文、数字都要原样抄，保留层级和顺序；看不清的写「…」）；
3. 再描述布局结构与关键视觉信息（位置关系、颜色、高亮、图表趋势、异常标记）；
4. 只描述看得见的内容，不要推测；不确定就明确写「不确定」。
直接输出纯文本，不要 Markdown 标题、不要客套话。`

// Normalized 补默认值（整型/时长零值视为未设置）。
func (p VisionBridge) Normalized() VisionBridge {
	d := DefaultVisionBridge()
	if strings.TrimSpace(p.Model) == "" {
		p.Model = d.Model
	}
	if p.MaxImages <= 0 {
		p.MaxImages = d.MaxImages
	}
	if p.MaxChars <= 0 {
		p.MaxChars = d.MaxChars
	}
	if p.MaxTokens <= 0 {
		p.MaxTokens = d.MaxTokens
	}
	if p.Timeout <= 0 {
		p.Timeout = d.Timeout
	}
	if p.TotalTimeout <= 0 {
		p.TotalTimeout = d.TotalTimeout
	}
	if strings.TrimSpace(p.Prompt) == "" {
		p.Prompt = DefaultVisionPrompt
	}
	return p
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }
