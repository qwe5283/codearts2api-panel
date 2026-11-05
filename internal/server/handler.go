// Package server 暴露 OpenAI 兼容接口：/v1/chat/completions、/v1/models、/status、WebUI。
package server

import (
	crand "crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/balance"
	"codearts2api-panel/internal/imgshrink"
	"codearts2api-panel/internal/livecfg"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/reqlog"
	"codearts2api-panel/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool            *pool.Pool
	Upstream        *upstream.Client
	AccountStrategy string // 未注入 Live 时的默认选号策略
	APIKey          string
	AdminKey        string
	Settings        SettingsStore
	RequestLog      *reqlog.Recorder
	Balance         *balance.Recorder
	MaxRotate       int
	SoftCooldown    time.Duration
	ErrThreshold    int
	ErrCooldown     time.Duration
	// CodeArts 上游把并发会话/TPM 排队既可能返回 HTTP 400/429，
	// 也可能嵌在 HTTP 200 SSE 内。默认每 10s 重试一次、最多 30 次（约 5 分钟）。
	QueueRetryDelay  time.Duration
	QueueMaxAttempts int
	DefaultModel     string
	ConvStateFile    string
	WatchInfo        map[string]any
	AuthDir          string
	Listen           string
	// OAuthClient 短超时客户端，用于 WebUI 登录换取 token（可注入测试端点）。
	OAuthClient *upstream.Client
	// LoginConfig WebUI 登录配置（client_id 以官方客户端为准）。
	LoginConfig upstream.LoginConfig
	// OAuthCallbackHost 可选：覆盖授权链接回调 host（如 https://oneapi.example.com/codearts）。
	OAuthCallbackHost string
	// Live 运行期配置快照：面板保存后热生效的字段从这里读。
	// 为 nil 时（测试/裸用）回落到上面各字段的启动值。
	Live *livecfg.Holder

	// 以下是多模态相关策略的启动值；请求期一律读 live 快照
	// （live 里没有时回落这里），这样面板改完立即生效。
	MaxBodyBytes int64
	ImageShrink  *imgshrink.Policy
	VisionBridge *VisionBridgePolicy
	// ImageHandling 见 cmd/server/config.go 同名配置项（bridge/analyze/passthrough）。
	ImageHandling string
}

// live 返回请求期配置快照。未注入 holder 时用启动字段拼一个等价快照，
// 保证直接 NewHandler(Config{...}) 的调用方行为不变。
func (h *Handler) live() livecfg.Snapshot {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load()
	}
	return livecfg.Snapshot{
		APIKey:            h.cfg.APIKey,
		AdminKey:          h.cfg.AdminKey,
		DefaultModel:      h.cfg.DefaultModel,
		AccountStrategy:   h.cfg.AccountStrategy,
		SoftCooldown:      h.cfg.SoftCooldown,
		ErrThreshold:      h.cfg.ErrThreshold,
		ErrCooldown:       h.cfg.ErrCooldown,
		QueueRetryDelay:   h.cfg.QueueRetryDelay,
		QueueMaxAttempts:  h.cfg.QueueMaxAttempts,
		OAuthCallbackHost: h.cfg.OAuthCallbackHost,
		LoginClientID:     h.cfg.LoginConfig.ClientID,
	}
}

// 测试/裸用时的回落：把启动值里的多模态策略补进快照。
func (h *Handler) liveMultimodal(s livecfg.Snapshot) livecfg.Snapshot {
	if h.cfg.Live != nil {
		return s // 生产：一律以热配置快照为准，不能拿启动值把用户关掉的开关又打开
	}
	if s.MaxBodyBytes <= 0 {
		s.MaxBodyBytes = h.cfg.MaxBodyBytes
	}
	if h.cfg.ImageShrink != nil && !s.ImageShrinkEnabled {
		s.ImageShrinkEnabled = true
		s.ImageShrink = *h.cfg.ImageShrink
	}
	// 注意：这里要判 Enabled，而不是只看指针非空 —— 测试/裸用里传 &Policy{Enabled:false}
	// 表示"明确关闭"，不能被启动值重新打开。
	if h.cfg.VisionBridge != nil && h.cfg.VisionBridge.Enabled && !s.VisionBridgeEnabled {
		s.VisionBridgeEnabled = true
		s.VisionBridge = *h.cfg.VisionBridge
	}
	if s.ImageHandling == "" {
		s.ImageHandling = h.cfg.ImageHandling
	}
	if s.ImageHandling == "" {
		s.ImageHandling = "bridge"
	}
	return s
}

// noteError 用当前（可热改）阈值记一次失败。
func (h *Handler) noteError(name string) {
	s := h.live()
	h.cfg.Pool.NoteError(name, s.ErrThreshold, s.ErrCooldown)
}

// cooldownErr / cooldownSoft 用当前（可热改）冷却时长处罚账号。
func (h *Handler) cooldownErr(name, reason string) {
	h.cfg.Pool.Cooldown(name, pool.CoolErr, h.live().ErrCooldown, reason)
}

func (h *Handler) cooldownSoft(name, reason string) {
	h.cfg.Pool.Cooldown(name, pool.CoolSoft, h.live().SoftCooldown, reason)
}

// loginConfig 当前登录配置：静态端点来自启动配置，client_id 允许热改。
func (h *Handler) loginConfig() upstream.LoginConfig {
	cfg := h.cfg.LoginConfig
	if id := h.live().LoginClientID; id != "" {
		cfg.ClientID = id
	}
	return cfg
}

// queueReacquireWait 排队重试等完间隔后重新拿回本账号槽位的上限（测试可覆盖）。
var queueReacquireWait = 30 * time.Second

var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

const (
	// dynamicModelsTTL 合并展示列表的缓存时长，与账号目录 CatalogTTL 对齐（23h）。
	// 列表由后台续期维护，不再依赖用户请求触发刷新。
	dynamicModelsTTL        = 23 * time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// defaultMaxBodyBytes 聊天请求体默认上限。旧的硬编码 8MB 太小：一张 2560×1440 的截图
// base64 后约 2~5MB，多图或长会话很容易越界，客户端只会拿到 413 而不是模型回答。
// 实际上限由 config 的 max_body_mb 决定（默认 32MB），可热改。
const defaultMaxBodyBytes = 32 << 20

// maxAuthBytes 凭据导入（auth JSON）的上限，与聊天请求体无关。
const maxAuthBytes = 8 << 20

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	oauth   *oauthStore
	started time.Time // 进程启动时刻，面板顶栏据此显示运行时长

	convMu sync.Mutex
	chats  map[string]string // account → 最近 chat_id
	// 黏性路由：conversation_id → account_name（多轮续接锁定同一账号，减少上游并发会话占用）。
	convAcct map[string]string

	loginMu    sync.Mutex
	logins     map[string]*pendingLogin
	accountsMu sync.Mutex
	logs       *Ring
	// attemptErrs 是「尝试次数」列 tooltip 的数据源：只存内存，滚动保留最近 50 条。
	attemptErrs *attemptErrors
}

// pendingLogin WebUI 登录中间状态。
type pendingLogin struct {
	TicketID string
	Secret   string
	Verifier string
	Port     int
	Done     bool
	Err      string
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.QueueRetryDelay <= 0 {
		cfg.QueueRetryDelay = 10 * time.Second
	}
	if cfg.QueueMaxAttempts <= 0 {
		// 上游并发/TPM 排队是瞬时的，但 180×10s = 30 分钟对交互式调用等于挂死；
		// 取 30 次（约 5 分钟）作为默认，可用 queue_max_attempts 调整。
		cfg.QueueMaxAttempts = 30
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = "glm-5.2"
	}
	if cfg.Upstream == nil {
		cfg.Upstream = upstream.New(120 * time.Second)
	}
	if cfg.OAuthClient == nil {
		cfg.OAuthClient = upstream.New(15 * time.Second)
	}
	if cfg.LoginConfig.ClientID == "" {
		cfg.LoginConfig = upstream.DefaultLoginConfig()
	}
	if cfg.RequestLog == nil {
		cfg.RequestLog = reqlog.New(reqlog.Config{})
	}
	h := &Handler{
		cfg: cfg, mux: http.NewServeMux(), oauth: newOAuthStore(), started: time.Now(),
		chats: map[string]string{}, logins: map[string]*pendingLogin{},
		convAcct: map[string]string{}, logs: NewRing(500),
		attemptErrs: newAttemptErrors(attemptErrorLimit),
	}
	h.loadChats()
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAdminAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// WorkBuddy 风格控制台（页面不鉴权，API 走 Bearer）
	h.mux.HandleFunc("GET /{$}", h.servePanel)
	h.mux.HandleFunc("GET /admin", h.servePanel)
	h.mux.HandleFunc("GET /panel", h.servePanel)
	h.mux.HandleFunc("GET /panel/", h.servePanel)
	h.mux.HandleFunc("GET /panel/assets/", h.servePanelAsset)
	h.mux.HandleFunc("GET /admin/api/overview", h.withAdminAuth(h.adminOverview))
	h.mux.HandleFunc("GET /admin/api/models", h.withAdminAuth(h.models))
	h.mux.HandleFunc("GET /admin/api/usage", h.withAdminAuth(h.adminUsage))
	h.mux.HandleFunc("GET /admin/api/balance_history", h.withAdminAuth(h.adminBalanceHistory))
	h.mux.HandleFunc("GET /admin/api/logs", h.withAdminAuth(h.adminLogs))
	h.mux.HandleFunc("GET /admin/api/config", h.withAdminAuth(h.adminConfigGet))
	h.mux.HandleFunc("PUT /admin/api/config", h.withAdminAuth(h.adminConfigPut))
	h.mux.HandleFunc("POST /admin/api/credits", h.withAdminAuth(h.adminCredits))
	h.mux.HandleFunc("POST /admin/api/checkin", h.withAdminAuth(h.adminCheckin))
	h.mux.HandleFunc("POST /admin/api/keepalive", h.withAdminAuth(h.adminKeepalive))
	h.mux.HandleFunc("POST /admin/api/reload", h.withAdminAuth(h.adminReload))
	h.mux.HandleFunc("POST /admin/api/accounts/import", h.withAdminAuth(h.adminImport))
	h.mux.HandleFunc("POST /admin/api/accounts/delete", h.withAdminAuth(h.adminDelete))
	h.mux.HandleFunc("POST /admin/api/accounts/enable", h.withAdminAuth(h.adminEnable))
	h.mux.HandleFunc("POST /admin/api/accounts/disable", h.withAdminAuth(h.adminDisable))
	h.mux.HandleFunc("POST /admin/api/accounts/clear-cooldown", h.withAdminAuth(h.adminClearCooldown))
	h.mux.HandleFunc("POST /admin/api/accounts/note", h.withAdminAuth(h.adminSetNote))
	h.mux.HandleFunc("POST /admin/api/oauth/start", h.withAdminAuth(h.adminOAuthStart))
	h.mux.HandleFunc("POST /admin/api/oauth/poll", h.withAdminAuth(h.adminOAuthPoll))
	h.mux.HandleFunc("POST /admin/api/oauth/import-callback", h.withAdminAuth(h.adminOAuthImportCallback))
	h.mux.HandleFunc("GET /oauth/callback", h.oauthCallback)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.withKey(h.live().APIKey, next)(w, r)
	}
}

func (h *Handler) withAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		live := h.live()
		key := live.AdminKey
		if key == "" {
			key = live.APIKey
		}
		h.withKey(key, next)(w, r)
	}
}

func (h *Handler) withKey(expected string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if expected != "" {
			authz := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
			key := authz[len(prefix):]
			if subtle.ConstantTimeCompare([]byte(key), []byte(expected)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": h.cfg.Pool.List()})
}

// oauthCallback 本地回调：浏览器同机时由 portal 携带 code 跳到这里。
func (h *Handler) oauthCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	secret := r.URL.Query().Get("secret")
	redirect := r.URL.Query().Get("redirect")
	log.Printf("oauth callback hit: has_code=%t has_secret=%t has_redirect=%t", code != "", secret != "", redirect != "")
	// portal 第一次回调：带 secret + redirect，要求 307 跳转（登录页链路的一部分）。
	if secret != "" && redirect != "" {
		next, nextErr := url.Parse(redirect)
		portal, portalErr := url.Parse(h.cfg.LoginConfig.PortalHost)
		if nextErr != nil || portalErr != nil || next.Scheme != "https" || next.User != nil ||
			!strings.EqualFold(next.Host, portal.Host) || h.oauth.getByTicket(next.Query().Get("ticket_id")) == nil {
			http.Error(w, "OAuth redirect does not match an active Huawei session", http.StatusBadRequest)
			return
		}
		// 用 redirect 里的 ticket_id 定位 pending login，并把华为云下发的 secret 换进去
		// （ticket 轮询必须用 portal 的 secret，而不是本地生成的）。
		if u, err := url.Parse(redirect); err == nil {
			tid := u.Query().Get("ticket_id")
			if tid != "" {
				h.oauth.enableTicketFallbackByTicket(tid, secret)
				h.loginMu.Lock()
				if p, ok := h.logins[tid]; ok {
					p.Secret = secret
					log.Printf("oauth callback: updated secret for ticket=%s", shortID(tid))
				}
				h.loginMu.Unlock()
			}
		}
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if code == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("<h3>登录失败：缺少 code</h3><p>请回到 WebUI 重新发起登录。</p>"))
		return
	}
	candidates := h.oauth.codeSessionCandidates(r.URL.Query().Get("ticket_id"), secret)
	if len(candidates) == 0 {
		// 没有匹配（浏览器在远端时 code 通道不可用），提示用 ticket 通道
		_, _ = w.Write([]byte("<h3>登录已提交，请回到 WebUI 等待结果。</h3>"))
		return
	}
	// portal 的回调可能不带任何配对信息（实测只带 code），因此逐个候选尝试。
	// 授权码与 PKCE verifier 一对一校验：用错只会 STS5.1805，不会消耗授权码。
	var (
		tok     *upstream.TokenResponse
		sess    *oauthSession
		lastErr error
	)
	for _, cand := range candidates {
		t, err := h.cfg.OAuthClient.ExchangeCode(r.Context(), h.loginConfig(), code, cand.Verifier, cand.Port, cand.DPoPPrivateKey)
		if err == nil {
			tok, sess, lastErr = t, cand, nil
			break
		}
		lastErr = err
	}
	if lastErr != nil {
		// STS5.1805 = 授权码与服务端保存的 PKCE verifier 对不上，最常见的成因是
		// 链接生成后服务重启过（会话只在内存里）或重复使用了旧链接。
		hint := ""
		if strings.Contains(lastErr.Error(), "STS5.1805") || strings.Contains(lastErr.Error(), "invalid authorization code") {
			hint = "<p>授权码已失效：请回到 WebUI 重新点「登录」获取新链接，" +
				"并确保从生成到完成授权期间服务没有重启。</p>"
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<h3>换取凭证失败：" + html.EscapeString(lastErr.Error()) + "</h3>" + hint))
		return
	}
	if err := h.saveLoginResult(tok, sess.Verifier, sess.DPoPPrivateKey); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<h3>保存账号失败：" + html.EscapeString(err.Error()) + "</h3>"))
		return
	}
	h.oauth.complete(sess.ID)
	_, _ = w.Write([]byte("<h3>登录成功，可关闭此页面并回到 WebUI。</h3>"))
}

func tokName(t *upstream.TokenResponse) string {
	if t == nil {
		return ""
	}
	return t.UserName
}

func tokToken(t *upstream.TokenResponse) string {
	if t == nil {
		return ""
	}
	return t.Credentials.SecurityToken
}

// saveLoginResult 落盘 auth 并加入账号池。
func (h *Handler) saveLoginResult(tok *upstream.TokenResponse, codeVerifier string, dpopPrivateJWK ...upstream.DPoPPrivateJWK) error {
	h.accountsMu.Lock()
	defer h.accountsMu.Unlock()
	if h.cfg.AuthDir == "" {
		return errors.New("auth_dir not configured")
	}
	cred := tok.Credentials
	var privateJWK map[string]string
	if len(dpopPrivateJWK) > 0 {
		privateJWK = map[string]string(dpopPrivateJWK[0])
	}
	a := auth.New(tok.UserID, tok.UserName, tok.DomainID,
		cred.SecurityToken, cred.AccessKeyID, cred.SecretAccessKey,
		cred.Expiration, tok.RefreshToken, codeVerifier)
	a.SetClientID(h.loginConfig().ClientID)
	a.SetDPoPPrivateKey(privateJWK)

	// portal 回调只回传 code，不带身份；user_id 为空会让 auth.FileName() 退化成
	// codearts-unknown.json，账号池按 UserID 建索引就会加载不到（或与既有账号重复）。
	// 因此落盘前用 STS 凭证把身份补全，补全失败也要保证 user_id 有值。
	if a.UserID == "" || a.UserName == "" {
		uid, uname, domain := lookupIdentity(a)
		if uid == "" {
			// 兜底：refresh_token 里的 sub 是唯一且稳定的，避免写出 unknown 文件
			uid = identityFromRefreshToken(tok.RefreshToken)
			log.Printf("webui login: identity lookup failed, fallback user_id=%q", uid)
		}
		if uid != "" {
			a.UserID, a.UserName = uid, firstNonEmpty(uname, a.UserName)
			if domain != "" {
				a.DomainID = domain
			}
		}
	}
	if a.UserID == "" {
		return errors.New("无法确定华为云账号身份，请重试授权或通过凭据导入添加账号")
	}
	if err := os.MkdirAll(h.cfg.AuthDir, 0o700); err != nil {
		return err
	}
	if err := a.SetPathAndSave(filepath.Join(h.cfg.AuthDir, a.FileName())); err != nil {
		return err
	}
	if p := h.cfg.Pool.AddAccount(a); p != nil && p.Auth != nil {
		log.Printf("webui login success user_id=%s name=%s", a.UserID, a.UserName)
	} else {
		log.Printf("webui login saved user_id=%s name=%s (pool add failed)", a.UserID, a.UserName)
	}
	return nil
}

// lookupIdentity 用刚拿到的 STS 凭证查身份（先 caller-identity，再 current/user）。
func lookupIdentity(a *auth.Auth) (uid, name, domain string) {
	cred := upstream.SignCredential{
		AccessKeyID:     a.AccessKeyID,
		SecretAccessKey: a.SecretAccessKey,
		SecurityToken:   a.CloudDragonTok,
	}
	if uid, name, domain, err := upstream.CallerIdentity(cred); err == nil && uid != "" {
		return uid, name, domain
	} else if err != nil {
		log.Printf("webui login: caller-identity: %v", err)
	}
	if uid, name, domain, err := upstream.CurrentUser(cred); err == nil && uid != "" {
		return uid, name, domain
	} else if err != nil {
		log.Printf("webui login: current/user: %v", err)
	}
	return "", "", ""
}

// identityFromRefreshToken 从 JWT 形式的 refresh_token 里取 sub 作为兜底 user_id。
func identityFromRefreshToken(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return claims.Sub
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// models
// ---------------------------------------------------------------------------

// models 返回账号实际上游目录中的模型列表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(r.URL.Query().Get("refresh") == "1"),
	})
}

// modelList 动态获取模型列表并包装成 OpenAI 格式。
func (h *Handler) modelList(force bool) []map[string]any {
	return modelEntries(h.fetchDynamicModels(force))
}

// modelEntries 把账号模型目录包装成 OpenAI /v1/models 条目。
//
// 对外只暴露小写 ID：上游注册名区分大小写（GLM-5.2 用小写发会被回
// InferHub.002002009.404，2026-11-02 实测），但同一个模型不该因为大小写列出两条。
// 目录里也不会存在「只差大小写、其余属性相同」的两条 —— MergeModels 已经按小写 ID
// 合并过 —— 所以这里逐条小写化、按小写去重即可；聊天时再由 CanonicalModel /
// CatalogModelID 还原成该账号注册的那个写法。
//
// 目录为空就返回空列表：没有账号时客户端应该看到「这里没有模型」，
// 而不是一张硬编码的、没经过任何上游确认的模型表。
func modelEntries(infos []upstream.ModelInfo) []map[string]any {
	out := make([]map[string]any, 0, len(infos))
	seen := map[string]bool{}
	for _, mi := range infos {
		id := strings.ToLower(mi.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		// 模态声明：上游只有 supports_images 这一个图像能力开关，没表态的按纯文本处理；
		// 输出端没有非文本模型，固定 text。客户端（如带视觉选择器的）靠这个字段判断能不能贴图。
		inputModalities := []string{"text"}
		if mi.SupportsImages != nil && *mi.SupportsImages {
			inputModalities = append(inputModalities, "image")
		}
		entry := map[string]any{
			"id":                id,
			"object":            "model",
			"created":           1753600000,
			"owned_by":          "codearts",
			"input_modalities":  inputModalities,
			"output_modalities": []string{"text"},
		}
		if mi.ContextWindow > 0 {
			entry["context_length"] = mi.ContextWindow
		}
		if mi.MaxTokens > 0 {
			entry["max_output_tokens"] = mi.MaxTokens
		}
		if mi.RatioDisplay != "" {
			entry["ratio_display"] = mi.RatioDisplay
		}
		if mi.Name != "" {
			entry["model_name"] = mi.Name
		}
		// 模型描述与上游提示：面板在模型 ID 列的原生 tooltip 里展示（desc，必要时追加 notice 一行）。
		if mi.Desc != "" {
			entry["model_desc"] = mi.Desc
		}
		if mi.Notice != "" {
			entry["notice"] = mi.Notice
		}
		// 徽标数据：标签原文，加上两个只在上游明确表态时才出现的布尔值。
		if mi.Tag != "" {
			entry["model_tag"] = mi.Tag
		}
		if mi.DisplayEnabled != nil && !*mi.DisplayEnabled {
			entry["display_enabled"] = false
		}
		if mi.SupportsImages != nil && *mi.SupportsImages {
			entry["supports_images"] = true
		}
		out = append(out, entry)
	}
	return out
}

// fetchDynamicModels 刷新各账号模型目录并返回合并后的展示列表，缓存 1h。
// force=true 用于面板显式重新获取：跳过全局缓存与账号目录缓存。
//
// 目录按账号存放，因此这里逐个健康账号发现并写入其目录；
// 发现失败的账号沿用旧目录或静态兜底。
func (h *Handler) fetchDynamicModels(force bool) []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if !force && len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !force && !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	var sets [][]upstream.ModelInfo
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct.Auth == nil || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		if !force && !upstream.AccountCatalogStale(acct.UID) {
			if infos, ok := upstream.AccountModels(acct.UID); ok {
				sets = append(sets, infos)
				continue
			}
		}
		_, ak, sk := acct.Auth.Credentials()
		if ak == "" || sk == "" {
			continue
		}
		infos, err := h.cfg.Upstream.FetchModels(acct.Auth) // 成功即写入该账号目录
		if err != nil {
			log.Printf("model discovery failed for account %s: %v", acct.Name, err)
			continue
		}
		if len(infos) == 0 {
			log.Printf("model discovery returned no models for account %s", acct.Name)
			continue
		}
		sets = append(sets, infos)
	}
	infos := upstream.MergeModels(sets...)
	if len(infos) == 0 {
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// pickAccount 为一次调用挑账号：优先按策略选择目录里登记过该模型的
// 健康账号，都挑不到再退回普通选号。图像分析路径（describeImage）用它，
// 以免把视觉请求发给不支持视觉的账号白耗一次。
func (h *Handler) pickAccount(tried map[string]bool, model string) *pool.Account {
	want := strings.ToLower(upstream.CanonicalModel(model))
	if acct := h.pickByCatalog(tried, want); acct != nil {
		return acct
	}
	// 目录里没人登记这个模型。两种可能：① 谁都没有；② 目录还没发现
	// （客户端只调 /v1/chat/completions、从没调过 /v1/models 就属于这种）。
	// 直接退回轮转的话，混合池里没有该模型的账号会先吃一次 400，白耗 MaxRotate
	// （默认 3 次，账号多于 3 个时可能永远轮不到有能力的那台），还会给健康账号
	// 记错误、到阈值冷却 10 分钟。所以先补齐缺失的目录再挑一次。
	h.discoverMissingCatalogs(tried)
	if acct := h.pickByCatalog(tried, want); acct != nil {
		return acct
	}
	return h.cfg.Pool.PickExcluding(tried, h.live().AccountStrategy)
}

// pickByCatalog 在支持该模型的健康账号中应用当前选号策略。
func (h *Handler) pickByCatalog(tried map[string]bool, wantLower string) *pool.Account {
	candidates := map[string]bool{}
	for _, acct := range h.cfg.Pool.Accounts() {
		if models, known := upstream.AccountModels(acct.UID); known && catalogHas(models, wantLower) {
			candidates[acct.Name] = true
		}
	}
	return h.cfg.Pool.PickExcludingMatching(tried, h.live().AccountStrategy, candidates)
}

// discoverMissingCatalogs 补拉缺失的账号模型目录（懒发现）。
func (h *Handler) discoverMissingCatalogs(tried map[string]bool) {
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil || acct.Auth == nil || tried[acct.Name] || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		if !upstream.AccountCatalogStale(acct.UID) {
			continue
		}
		_, ak, sk := acct.Auth.Credentials()
		if ak == "" || sk == "" {
			continue
		}
		if _, err := h.cfg.Upstream.FetchModels(acct.Auth); err != nil {
			log.Printf("lazy model discovery failed for account %s: %v", acct.Name, err)
		}
	}
}

// ensureCatalog 在「所有健康账号的目录都还没发现」时补拉一次目录。
//
// 只在目录完全未发现时才触发（进程刚启动、或从未查过 /v1/models），且只补一次：
// 一旦有任一账号目录已发现，后续判断就有依据，不再补。这样：
//   - 冷启动后第一个带图请求也能正确判定模型图片能力（否则 analyze/bridge 都不
//     触发，图片原样发上游吃 001001020.406）；
//   - 目录已就绪时零额外上游调用（这是热路径，不能被每次请求的目录请求拖慢）。
func (h *Handler) ensureCatalog(tried map[string]bool) {
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		if _, ok := upstream.AccountModels(acct.UID); ok {
			return // 已有账号目录可用：判断有依据，不再补
		}
	}
	// 所有健康账号都还没目录：补一次。
	h.discoverMissingCatalogs(tried)
}

// accountServesModel 报告该账号的目录是否登记了这个模型（目录未知时放行）。
func (h *Handler) accountServesModel(acct *pool.Account, model string) bool {
	if acct == nil {
		return false
	}
	models, known := upstream.AccountModels(acct.UID)
	if !known {
		return true // 目录未发现：无依据拒绝，交给上游判定
	}
	return catalogHas(models, strings.ToLower(upstream.CanonicalModel(model)))
}

// catalogDeclaresNoImages 报告该模型是否被上游**明确声明**为不支持图片输入
// （supports_images 存在且为 false）。
//
// 三态严格区分：「未声明」（nil）不算，因为上游没表态时网关没有依据拒绝请求——
// 那会把「上游只是没写这个字段」误判成「不能收图」，挡住本来可用的模型。
func (h *Handler) catalogDeclaresNoImages(model string) bool {
	want := strings.ToLower(upstream.CanonicalModel(model))
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil {
			continue
		}
		models, ok := upstream.AccountModels(acct.UID)
		if !ok {
			continue
		}
		for _, mi := range models {
			if strings.ToLower(mi.ID) == want {
				return mi.SupportsImages != nil && !*mi.SupportsImages
			}
		}
	}
	return false
}

// visionModelIDs 返回所有账号目录里声明支持图片的模型 ID（去重、排序）。
// 用于错误信息里告诉调用方「可以改用哪些模型」。
func (h *Handler) visionModelIDs() []string {
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil {
			continue
		}
		models, ok := upstream.AccountModels(acct.UID)
		if !ok {
			continue
		}
		for _, mi := range models {
			if mi.ID == "" || mi.SupportsImages == nil || !*mi.SupportsImages {
				continue
			}
			if !seen[mi.ID] {
				seen[mi.ID] = true
				out = append(out, mi.ID)
			}
		}
	}
	sort.Strings(out)
	return out
}

// catalogHas 按小写 ID 在模型目录里查模型。
func catalogHas(models []upstream.ModelInfo, wantLower string) bool {
	for _, mi := range models {
		if strings.ToLower(mi.ID) == wantLower {
			return true
		}
	}
	return false
}

// catalogHasModel 报告该模型是否出现在某个账号已发现的目录里。
//
// known=false 表示所有账号的目录都还没发现过：这时没有任何证据判定「目录以外」，
// 调用方应放行，交给上游回答 —— 不能凭缺失的目录拒请求。
// 判定与 /v1/models 同源：用 CanonicalModel 归一大小写/旧别名后比对账号目录。
//
// 这里刻意**不**在未命中时去上游补目录：那是每个未知请求一次的额外往返，
// 目录本身由 /v1/models 首次惰性发现，之后由后台续期维护（CatalogTTL 23h）。
func (h *Handler) catalogHasModel(model string) (known, found bool) {
	want := strings.ToLower(upstream.CanonicalModel(model))
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil {
			continue
		}
		models, ok := upstream.AccountModels(acct.UID)
		if !ok {
			continue
		}
		known = true
		if catalogHas(models, want) {
			return true, true
		}
	}
	return known, false
}

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	observed := &responseObserver{ResponseWriter: w, status: http.StatusOK, started: started}
	w = observed
	event := reqlog.Event{Time: started, RequestID: reqlog.NewRequestID(), Path: r.URL.Path}
	// lastUpstreamErr 记本次请求最后一次上游错误：只有重试过（attempts>1）的请求
	// 才写进面板「尝试次数」列的 tooltip，普通请求不占额外内存。
	var lastUpstreamErr error
	h.cfg.RequestLog.Begin()
	defer func() {
		if event.Attempts > 1 && lastUpstreamErr != nil {
			h.attemptErrs.note(event.RequestID, safeRequestError(lastUpstreamErr.Error()))
		}
		h.finishRequest(event, observed, started)
	}()
	// 池里一个账号都没有：直接给出明确错误，别走完选号流程再报成「账号都不可用」。
	// 客户端拿到 no_accounts 才知道是运维侧没配置账号，而不是账号被禁用/冷却。
	if h.cfg.Pool == nil || len(h.cfg.Pool.Accounts()) == 0 {
		event.Error = "no accounts configured"
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_accounts", "no accounts configured; add one in the panel")
		return
	}
	limit := h.liveMultimodal(h.live()).MaxBodyBytes
	if limit <= 0 {
		limit = defaultMaxBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			"request body exceeds "+humanBytes(limit)+" limit (config max_body_mb)")
		return
	}
	req, err := parseChatRequest(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	tools, err := nativeTools(req.Tools, req.ToolChoice)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	affinity := r.Header.Get("X-Codearts-Chat-Id")
	if affinity == "" {
		affinity = r.Header.Get("X-Session-Affinity")
	}
	if affinity == "" {
		affinity = r.Header.Get("X-Session-Id")
	}
	req.ConversationID = conversationIDFor(req, affinity)

	toolsOn := toolsActive(req)
	model := h.live().DefaultModel
	if req.Model != "" && req.Model != "auto" {
		model = req.Model
	}
	event.Model = safeLogLabel(model)

	// analyzeRouted 表示本次请求改写了图片、注入了 analyze_image 工具。
	// 只有它为真时才需要「先聚合再判断回灌」：纯文本请求必须原样直通流式，
	// 否则客户端要等模型生成完才收到首字。
	analyzeRouted := false

	// 冷启动兜底：目录是内存态、重启即空，而下面三步都依赖目录里的图片能力声明。
	// 补齐目录的 discoverMissingCatalogs 此前只在 pickAccount（视觉子请求路径）里
	// 调过，chat 主路径从不调 —— 于是重启后第一个带图请求会扫到空目录，
	// catalogDeclaresNoImages/modelSupportsImages 把「不知道能力」当成「能收图」，
	// analyze 不路由、bridge 不触发，图片原样发上游吃 InferHub.001001020.406。
	// 这里在判定前补一次目录；已就绪时 ensureCatalog 立即返回，热路径零额外调用。
	if hasImageBlocks(req) {
		h.ensureCatalog(nil)
	}

	// 多模态前置处理，三步都在「模型已确定、还没发给上游」时做：
	//   ① 缩图：上游媒体转换对单图有硬上限（实测请求体 ≈4.3MB / 图片 ≈3.2MB），
	//      超了只能压小再发，否则拿到的是一句 ModelArts.81029；
	//   ② analyze 模式（image_handling=analyze）：图片换成占位符 + 注入工具，
	//      主模型按需调用，视觉模型只承担单图分析（复刻官方 AnalyzeImagePlugin）；
	//   ③ 识图桥接（默认）：目标模型在本上游是纯文本时，先用 VL 模型把图转述成
	//      文字，否则上游直接回 InferHub.001001020.406（非多模态）。
	multi := h.liveMultimodal(h.live())
	if multi.ImageShrinkEnabled {
		shrinkMessageImages(req, multi.ImageShrink, int64(len(body)))
	}
	// passthrough：不替调用方做任何决策，明确回 400 并列出可用的视觉模型。
	// 刻意不做自动改派——视觉模型上下文普遍更小（实测 deepseek 1M vs qwen3-vl 131k），
	// 拿大请求去撞只会得到 ModelArts.81001；而且视觉模型没有推理输出，
	// 静默换掉会同时毁掉能力与语义。
	if multi.ImageHandling == "passthrough" && hasImageBlocks(req) && h.catalogDeclaresNoImages(model) {
		vision := strings.Join(h.visionModelIDs(), ", ")
		if vision == "" {
			vision = "(当前账号目录里没有声明支持图片的模型)"
		}
		writeOpenAIError(w, http.StatusBadRequest, "model_not_multimodal",
			"model "+model+" does not accept image input; retry with a vision model such as: "+vision)
		return
	}
	// analyze 模式：图片换成占位符 + 注入 analyze_image 工具，主模型按需调用。
	// 与 bridge 互斥——analyze 后请求里已无图片，bridge 分支自然不触发。
	// 仅对「目录明确声明不支持图片」的模型生效；未声明时无依据，照原样放行。
	if multi.ImageHandling == "analyze" && !analyzeRouted && hasImageBlocks(req) && h.catalogDeclaresNoImages(model) {
		if injected := rewriteImagesForAnalyze(req); len(injected) > 0 {
			req.Tools = append(req.Tools, injected...)
			analyzeRouted = true
			// 工具列表在图片改写之前就已构建，注入 analyze_image 后必须重建，
			// 否则主模型根本看不到这个工具。
			if tools, err = nativeTools(req.Tools, req.ToolChoice); err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			log.Printf("analyze mode model=%s: images rewritten, analyze_image tool injected", model)
		}
	}
	// bridge 只在默认模式生效；analyze 已自行处理图片，passthrough 表示
	// 调用方要原样发上游（由上游回 001001020），都不该被 bridge 改写。
	// 归一化保证 ImageHandling 非空（见 liveMultimodal 的回落）。
	if multi.ImageHandling == "bridge" && multi.VisionBridgeEnabled && len(imagePartsOf(req)) > 0 {
		policy := multi.VisionBridge.Normalized()
		if supports, known := h.modelSupportsImages(model); known && !supports &&
			!strings.EqualFold(upstream.CanonicalModel(policy.Model), upstream.CanonicalModel(model)) {
			stats, bridgeErr := h.bridgeImages(r.Context(), req, policy)
			switch {
			case bridgeErr != nil && clientGone(r):
				writeOpenAIError(w, http.StatusServiceUnavailable, "client_cancelled", "client disconnected")
				return
			case bridgeErr != nil:
				// 保持原样：让上游按 406 明确报错，别把图悄悄吞掉。
				log.Printf("vision bridge failed model=%s: %v（原图保留）", model, bridgeErr)
			case stats.Images > 0:
				log.Printf("vision bridge model=%s vl=%s images=%d chars=%d cache_hits=%d",
					model, stats.VLModel, stats.Images, stats.Chars, stats.CacheHits)
			}
		}
	}

	// 目录之外的模型 ID 不再往上游打：上游只会回 InferHub.002002009.404，
	// 还白占一次并发会话与轮转名额。目录还没发现过时没有依据，放行交给上游。
	if known, found := h.catalogHasModel(model); known && !found {
		event.Error = "model " + safeLogLabel(model) + " is not in the model catalog"
		writeOpenAIError(w, http.StatusNotFound, "model_not_available",
			"model "+model+" is not in this service's model catalog; call GET /v1/models for the available ids")
		return
	}

	// OpenAI 线格式本身不携带会话 ID；conversationIDFor 用首条 user 消息
	// 为整条 Agent 工具链固定 CodeArts 内置 chat_id 与账号亲和。V2 接口
	// 不保留语义上下文，所以每轮仍重放完整 OpenAI 历史。
	explicitChat := validChatID(req.ConversationID)
	stickyAcct := ""
	if explicitChat {
		h.convMu.Lock()
		stickyAcct = h.convAcct[req.ConversationID]
		h.convMu.Unlock()
	}
	msgs := buildUpstreamMessages(req, toolsOn)

	tried := map[string]bool{}
	var lastErr error
	// analyze 模式的工具回灌轮数：主模型调用 analyze_image 时，网关内部执行后
	// 把结果作为 tool 消息回灌、再请求一次；超过上限就停止，避免死循环。
	analyzeRounds := 0
	for {
		answered := false
		for i := 0; i < h.cfg.MaxRotate; i++ {
			var acct *pool.Account
			if stickyAcct != "" {
				// 续接会话：锁定原账号（若仍健康）。
				acct = h.cfg.Pool.Get(stickyAcct)
				if acct == nil || !h.cfg.Pool.Healthy(stickyAcct) {
					stickyAcct = ""
					acct = h.cfg.Pool.PickExcluding(tried, h.live().AccountStrategy)
				} else {
					tried[acct.Name] = true
				}
			}
			if acct == nil {
				acct = h.cfg.Pool.PickExcluding(tried, h.live().AccountStrategy)
			}
			if acct == nil {
				break
			}
			tried[acct.Name] = true
			event.Account = shortID(acct.UID)

			// 阻塞等待并发槽位（上游单账号并发会话释放慢，串行最稳）。
			// 超时 180s 排队（5 个请求 × ~30s 上限），避免高并发立即失败跳号。
			if !h.cfg.Pool.AcquireLockWait(acct.Name, 180*time.Second) {
				log.Printf("account %s concurrent limit reached after wait, trying next", acct.Name)
				continue
			}

			ok, verr := h.cfg.Pool.Validate(acct)
			if verr == nil && !ok {
				h.cfg.Pool.ReleaseLock(acct.Name)
				lastErr = errors.New("token invalid")
				continue
			}
			if verr != nil {
				h.cfg.Pool.ReleaseLock(acct.Name)
				lastErr = verr
				continue
			}

			// 检查是否需要保活
			if h.cfg.Pool.NeedKeepalive(acct.Name) {
				h.cfg.Pool.PingKeepalive(acct.Name)
			}

			chatID := req.ConversationID
			// 上游要求 chat_id 为 32 位十六进制（UUID 去连字符）；无显式 id 时每次新建。
			if !validChatID(chatID) {
				chatID = randHex(32)
			}

			token, accessKeyID, secretAccessKey := acct.Auth.Credentials()
			cred := upstream.SignCredential{
				AccessKeyID:     accessKeyID,
				SecretAccessKey: secretAccessKey,
				SecurityToken:   token,
			}
			// 上游按注册名精确匹配，而 CanonicalModel 的大小写索引是跨账号的（同一模型
			// 在不同账号可以注册成不同大小写）。发送前按当前账号自己的目录定大小写：
			// catalogHasModel 放行的 ID 才一定调得通，也不用再白打一次注定 002002009.404
			// 的上游往返。
			chatOpts := upstream.ChatOptions{
				ReasoningEffort: req.ReasoningEffort,
				MaxTokens:       req.MaxTokens,
				Temperature:     req.Temperature,
				TopP:            req.TopP,
				Tools:           tools,
				ToolChoice:      nativeToolChoice(req.ToolChoice),
			}
			if exact, ok := upstream.CatalogModelID(acct.UID, model); ok {
				chatOpts.ExactModelID = exact
			}
			// 上游并发会话/TPM 排队是瞬时的。对齐 CodeArts Agent IDE
			// 参考实现：默认每 10s 重试同一账号，最多 180 次（30min）。
			// 等待期间释放并发锁，让排队的请求也能尝试（避免死锁式串行等待）。
			var rc io.ReadCloser
			var serr error
			authRetried := false
			// lockHeld 跟踪本请求是否仍持有并发槽位：排队重试会「释放→等待→重获」，
			// 重获失败时不能再走下面的统一释放（计数器是账号级的、不分持有者，
			// 多减一次就是把别的在跑请求的槽位还掉，账号随即被超发）。
			lockHeld := true
			queueAttempts, queueDelay := h.live().QueueMaxAttempts, h.live().QueueRetryDelay
			if queueAttempts < 1 {
				queueAttempts = 1
			}
			for retry := 0; retry < queueAttempts; retry++ {
				event.Attempts++
				rc, serr = acct.Client.ChatStreamWithOptions(r.Context(), chatID, msgs, "", cred, acct.UserName, model, chatOpts)
				if serr == nil {
					break
				}
				lastUpstreamErr = serr
				var ae *upstream.ApiError
				if errors.As(serr, &ae) && (ae.Status == 401 || ae.Code == 401) && !authRetried && acct.Auth.Refresh() != "" {
					if rerr := h.cfg.Pool.RefreshToken(acct.Name); rerr == nil {
						token, accessKeyID, secretAccessKey = acct.Auth.Credentials()
						cred = upstream.SignCredential{
							AccessKeyID:     accessKeyID,
							SecretAccessKey: secretAccessKey,
							SecurityToken:   token,
						}
						authRetried = true
						log.Printf("upstream 401 account=%s: token refreshed, retrying request once", acct.Name)
						continue
					} else {
						log.Printf("upstream 401 account=%s: refresh before retry failed: %v", acct.Name, rerr)
					}
				}
				if errors.As(serr, &ae) && isQueueLimitError(ae) {
					// 已经是最后一次尝试：等完也没有下一次了，等待只会把客户端看到
					// 错误的时间往后推（实测每次白等 10s，attempts=3 时整条请求 30s）。
					// 直接带着这次的上游错误收尾，交给外层换号或返回。
					if retry+1 >= queueAttempts {
						break
					}
					log.Printf("upstream queue limit retry=%d/%d account=%s, waiting %s", retry+1, queueAttempts, acct.Name, queueDelay)
					// 释放锁让其他请求有机会，等待后重新获取
					h.cfg.Pool.ReleaseLock(acct.Name)
					lockHeld = false
					queueStarted := time.Now()
					select {
					case <-time.After(queueDelay):
					case <-r.Context().Done():
						event.QueueMs += time.Since(queueStarted).Milliseconds()
						writeOpenAIError(w, http.StatusServiceUnavailable, "client_cancelled", "client disconnected")
						return
					}
					event.QueueMs += time.Since(queueStarted).Milliseconds()
					if !h.cfg.Pool.AcquireLockWait(acct.Name, queueReacquireWait) {
						lastErr = errors.New("concurrent limit: could not reacquire lock after wait")
						break
					}
					lockHeld = true
					continue
				}
				break // 非并发错误，跳出重试
			}
			if serr != nil {
				if lockHeld {
					h.cfg.Pool.ReleaseLock(acct.Name) // 释放槽位再换号
				}
				if clientGone(r) {
					// 调用方自己断开了：上游必然报 context canceled / broken pipe。
					log.Printf("client disconnected before upstream reply account=%s: %v", acct.Name, serr)
					writeOpenAIError(w, http.StatusServiceUnavailable, "client_cancelled", "client disconnected")
					return
				}
				if isParamInvalidError(serr) {
					// Client request-param problem: rotating accounts yields the same 400,
					// and cooling accounts takes down the whole pool for minutes.
					writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
						"upstream rejected request params: "+truncateMsg(serr.Error(), errorTextLimit))
					return
				}
				lastErr = serr
				if h.handleUpstreamError(acct, model, serr) {
					// 模型级错误：换账号也是同样结果，直接给客户端明确答复。
					writeOpenAIError(w, http.StatusNotFound, "model_not_available",
						"model "+model+" is not available: "+truncateMsg(serr.Error(), errorTextLimit))
					return
				}
				continue
			}
			w.Header().Set("X-Codearts-Chat-Id", chatID)

			storeChat := func() {
				// 仅缓存显式会话，便于同 conversation_id 续聊；不把一次性测连写进账号默认会话。
				if !explicitChat {
					return
				}
				h.convMu.Lock()
				h.chats[acct.Name] = chatID
				h.convAcct[req.ConversationID] = acct.Name // 黏性路由：续接锁定同账号
				h.convMu.Unlock()
				h.saveChats()
			}
			// analyze 模式必须走聚合路径：要先拿到主模型的完整回复才能判断它是否
			// 调用了 analyze_image、是否需要回灌结果。纯文本请求保持原样直通流式，
			// 否则首字要等到模型生成完才返回（实测 91s 的 TTFB）。
			if req.Stream && !analyzeRouted {
				var usage func(*upstream.RawCompletion) map[string]any
				if req.IncludeUsage {
					usage = func(comp *upstream.RawCompletion) map[string]any { return usageEstimate(msgs, comp) }
				}
				werr := upstream.StreamCaptureWithUsage(w, rc, model, func(comp *upstream.RawCompletion) {
					captureUsage(&event, comp.Usage)
					storeChat()
					// 流式传输中定期保活
					h.cfg.Pool.PingKeepalive(acct.Name)
				}, usage)
				rc.Close()
				h.cfg.Pool.ReleaseLock(acct.Name) // 释放锁
				if werr != nil {
					lastUpstreamErr = werr
					event.Outcome = reqlog.OutcomeStreamError
					event.Error = werr.Error()
					log.Printf("chat stream account=%s error: %v", acct.Name, werr)
					if !clientGone(r) && !isParamInvalidError(werr) {
						h.noteError(acct.Name)
					}
				} else {
					event.OK, event.Outcome = true, reqlog.OutcomeSuccess
					h.cfg.Pool.NoteSuccess(acct.Name)
				}
				return
			}

			comp, aerr := upstream.AggregateRaw(rc)
			rc.Close()
			h.cfg.Pool.ReleaseLock(acct.Name) // 释放锁
			if aerr != nil {
				lastUpstreamErr = aerr
				if clientGone(r) {
					log.Printf("client disconnected during aggregate account=%s: %v", acct.Name, aerr)
					writeOpenAIError(w, http.StatusServiceUnavailable, "client_cancelled", "client disconnected")
					return
				}
				if isParamInvalidError(aerr) {
					writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
						"upstream rejected request params: "+truncateMsg(aerr.Error(), errorTextLimit))
					return
				}
				lastErr = aerr
				h.cooldownErr(acct.Name, aerr.Error())
				continue
			}

			content, finish := comp.Content, comp.Finish
			calls := fromUpstreamToolCalls(comp.ToolCalls)
			// 原生 tool_calls 是 GLM-5.2 的主路径。仅保留文本协议解析作为
			// 旧模型/异常输出的兼容兜底，并同时检查 reasoning_content，
			// 避免模型把工具 JSON 放到思考通道时错误以 stop 结束。
			if toolsOn && len(calls) == 0 {
				if c, cleanContent, found := toolResponse(comp.Content + "\n" + comp.Reasoning); found {
					calls = c
					assignCallIDs(calls)
					content = cleanContent
					finish = "tool_calls"
				}
			}
			if len(calls) > 0 {
				finish = "tool_calls"
			}
			// analyze 模式：主模型调用了 analyze_image，在网关内部执行掉，
			// 把结果回灌成 tool 消息后重新请求。其余工具调用原样交给客户端。
			if analyzeRouted && len(calls) > 0 && analyzeRounds < analyzeMaxRounds {
				if results, handled := h.handleAnalyzeToolCalls(r.Context(), calls); handled {
					analyzeRounds++
					msgs = append(msgs, upstream.ChatMessage{
						Role:    "assistant",
						Content: content,
						ToolCalls: []upstream.ChatToolCall{
							{ID: calls[0].ID, Type: "function", Function: upstream.ChatToolFunction{Name: calls[0].Name, Arguments: calls[0].Arguments}},
						},
					})
					msgs = append(msgs, openAIMessagesToUpstream(results)...)
					storeChat()
					h.cfg.Pool.NoteSuccess(acct.Name)
					captureUsage(&event, comp.Usage)
					// 工具结果已回灌：跳出内层轮转，外层 for 重新发起一次完整请求。
					tried = map[string]bool{}
					answered = true
					break
				}
			}
			storeChat()
			h.cfg.Pool.NoteSuccess(acct.Name)
			captureUsage(&event, comp.Usage)
			event.OK, event.Outcome = true, reqlog.OutcomeSuccess

			// analyze 改写过的请求响应已被聚合：客户端要流式时按 SSE chunk 补发。
			if req.Stream {
				writeAggregatedAsStream(w, model, comp, content, calls, finish, chatID, usageEstimate(msgs, comp))
				return
			}
			writeJSON(w, http.StatusOK, buildCompletion(model, comp.Reasoning, content, calls, finish, chatID, usageEstimate(msgs, comp)))
			return
		} // 内层轮转
		// 工具结果已回灌：外层 for 重新发起一次完整请求。
		if answered {
			continue
		}
		// 落到这里有两种完全不同的原因，不能再共用一句话：
		// 一种是轮转时连一个健康账号都挑不出来（确实禁用/冷却中）；
		// 另一种是挑到了账号、也真的打到了上游，只是重试预算用完了——账号多半是健康的，
		// 把上游错误说成「账号都不可用」会让人去查根本不存在的账号故障。
		if lastErr != nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "upstream_retries_exhausted",
				fmt.Sprintf("upstream attempts exhausted after %d attempt(s) on %d account(s): %s",
					event.Attempts, len(tried), truncateMsg(lastErr.Error(), errorTextLimit)))
			return
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", "all accounts unavailable (disabled/cooldown)")
	}
}

// writeAggregatedAsStream 把已聚合的完整响应按 OpenAI 流式格式补发。
//
// analyze 模式必须先聚合才能判断是否要回灌工具结果，所以不能直接直通流式。
// 这里在拿到最终答复后按 chunk 格式回放：先发 reasoning（若有），再发正文或
// tool_calls，最后 finish_reason + [DONE]。调用方仍按 SSE 消费。
func writeAggregatedAsStream(w http.ResponseWriter, model string, comp *upstream.RawCompletion,
	content string, calls []openAIToolCall, finish, chatID string, usage map[string]any) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	if chatID != "" {
		h.Set("X-Codearts-Chat-Id", chatID)
	}
	fl, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

	write := func(v any) {
		raw, _ := json.Marshal(v)
		_, _ = io.WriteString(w, "data: "+string(raw)+"\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
	chunk := func(delta map[string]any, fin string) {
		choice := map[string]any{"index": 0, "delta": delta}
		if fin != "" {
			choice["finish_reason"] = fin
		}
		write(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"model": model, "choices": []any{choice},
		})
	}

	if comp.Reasoning != "" {
		chunk(map[string]any{"reasoning_content": comp.Reasoning}, "")
	}
	if len(calls) > 0 {
		chunk(map[string]any{"tool_calls": toOpenAIToolCalls(calls)}, "")
	} else if content != "" {
		chunk(map[string]any{"content": content}, "")
	}
	chunk(map[string]any{}, finish)
	if usage != nil {
		write(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"model": model, "choices": []any{}, "usage": usage,
		})
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// hasImageBlocks 报告请求里是否含图片分片。
// 上游对纯文本模型收到带图请求会回 InferHub.001001020（且被判为 5xx 冷却账号），
// 因此这是决定是否改派视觉模型的依据。
func hasImageBlocks(req *chatRequest) bool {
	for _, m := range req.Messages {
		if m.HasImages {
			return true
		}
	}
	return false
}

// buildUpstreamMessages 把 OpenAI 消息原生映射为 CodeArts V2 chat-completions。
// Chat-Id/Session-Id 只做亲和与缓存；多轮语义仍由完整 messages 承载。
func buildUpstreamMessages(req *chatRequest, toolsOn bool) []upstream.ChatMessage {
	_ = toolsOn // 保留参数以稳定内部调用面；工具 schema 由 ChatOptions 原生传递。
	keepCalls, keepResults := pairedToolMessages(req.Messages)
	out := make([]upstream.ChatMessage, 0, len(req.Messages))
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "user":
			content := m.Content
			if content == nil {
				content = m.Text
			}
			out = append(out, upstream.ChatMessage{Role: m.Role, Content: content})
		case "assistant":
			reasoning := m.ReasoningContent
			wire := upstream.ChatMessage{Role: "assistant", Content: m.Text, ReasoningContent: &reasoning}
			if keepCalls[i] {
				for _, c := range m.ToolCalls {
					wire.ToolCalls = append(wire.ToolCalls, upstream.ChatToolCall{
						ID: c.ID, Type: "function",
						Function: upstream.ChatToolFunction{Name: c.Name, Arguments: c.Arguments},
					})
				}
				if m.Text == "" {
					wire.Content = nil
				}
			}
			out = append(out, wire)
		case "tool":
			if keepResults[i] {
				content := m.Text
				if content == "" {
					content = "(no output)"
				}
				out = append(out, upstream.ChatMessage{
					Role: "tool", Content: content, ToolCallID: m.ToolCallID, Name: m.Name,
				})
			}
		}
	}
	return out
}

// pairedToolMessages 只保留完整成对的 assistant.tool_calls + tool results。
// CodeArts 会对孤儿或部分成对的工具历史返回 400，并使后续每轮持续失败。
func pairedToolMessages(messages []openAIMessage) (map[int]bool, map[int]bool) {
	resultIndex := make(map[string]int)
	for i, m := range messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			resultIndex[m.ToolCallID] = i
		}
	}
	keepCalls := make(map[int]bool)
	keepResults := make(map[int]bool)
	for i, m := range messages {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		complete := true
		for _, c := range m.ToolCalls {
			if c.ID == "" {
				complete = false
				break
			}
			if _, ok := resultIndex[c.ID]; !ok {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		keepCalls[i] = true
		for _, c := range m.ToolCalls {
			keepResults[resultIndex[c.ID]] = true
		}
	}
	return keepCalls, keepResults
}

// nativeTools preserves a named function choice by exposing only that function
// to the upstream, whose tool_choice accepts strings but not a named object.
func nativeTools(tools []map[string]any, choice toolChoiceOpenAI) ([]map[string]any, error) {
	if choice.Mode != "function" {
		return tools, nil
	}
	var selected map[string]any
	for _, tool := range tools {
		fn, ok := tool["function"].(map[string]any)
		if !ok || fn["name"] != choice.Function {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("tool_choice function %q is defined more than once", choice.Function)
		}
		selected = tool
	}
	if selected == nil {
		return nil, fmt.Errorf("tool_choice function %q is not defined in tools", choice.Function)
	}
	return []map[string]any{selected}, nil
}

func nativeToolChoice(choice toolChoiceOpenAI) any {
	switch choice.Mode {
	case "none", "required":
		return choice.Mode
	case "function":
		// CodeArts InferHub only accepts string-form tool_choice; the object form
		// is rejected with InferHub.001001005.400 param invalid (measured 2026-10-06).
		// nativeTools limits the tools to the named function, so "required"
		// preserves the client's forced-function constraint.
		return "required"
	default:
		return "auto"
	}
}

func fromUpstreamToolCalls(calls []upstream.ChatToolCall) []openAIToolCall {
	out := make([]openAIToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, openAIToolCall{ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	assignCallIDs(out)
	return out
}

func usageEstimate(msgs []upstream.ChatMessage, comp *upstream.RawCompletion) map[string]any {
	if comp.Usage != nil {
		return comp.Usage
	}
	var pt int
	for _, m := range msgs {
		switch content := m.Content.(type) {
		case string:
			pt += len([]rune(content))/4 + 1
		default:
			raw, _ := json.Marshal(content)
			pt += len([]rune(string(raw)))/4 + 1
		}
	}
	ct := (len([]rune(comp.Content))+len([]rune(comp.Reasoning)))/4 + 1
	return map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}
}

// buildCompletion 组装非流式响应。
func buildCompletion(model, reasoning, content string, calls []openAIToolCall, finish, chatID string, usage map[string]any) map[string]any {
	message := map[string]any{"role": "assistant"}
	if len(calls) > 0 {
		message["tool_calls"] = toOpenAIToolCalls(calls)
		if content == "" {
			message["content"] = nil
		} else {
			message["content"] = content
		}
	} else {
		message["content"] = content
	}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"chat_id": chatID,
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp
}

// upstreamUnavailableReason 把上游错误归类为「该模型对这个账号不可用」，否则返回空串。
func upstreamUnavailableReason(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "002002009") || strings.Contains(msg, "not registered") {
		return "模型未在当前账号注册"
	}
	return ""
}

// handleUpstreamError 记录错误并决定是否处罚账号。
// 返回 true 表示这是「模型对这个账号不可用」的模型级错误：调用方应立即结束
// 轮转并向客户端返回明确错误（换账号也是同样的结果）。
func (h *Handler) handleUpstreamError(acct *pool.Account, model string, err error) bool {
	if isParamInvalidError(err) {
		return false
	}
	var ae *upstream.ApiError
	if errors.As(err, &ae) {
		// 「模型对这个账号不可用」是账号能力问题，不是账号故障，必须在状态码分支
		// **之前**判断：not registered 会被嵌入错误包装成
		// 5xx，落到下面的 `ae.Status >= 500` 就把健康账号冷却 10 分钟——实测
		// 调用一个未注册模型，整个账号被冷却，后续正常请求全部失败。
		if reason := upstreamUnavailableReason(err); reason != "" {
			log.Printf("model unusable account=%s model=%s: %s", acct.Name, model, reason)
			return true
		}
		switch {
		case ae.Status == 401 || ae.Code == 401:
			h.cfg.Pool.Disable(acct.Name, "401 "+ae.Message)
		case ae.Status == 429:
			h.cooldownSoft(acct.Name, ae.Error())
		case ae.Status == 400 && isConcurrentLimitError(ae.Message):
			// 并发会话上限（TM.00001041）是瞬时错误：上游会话槽位会被其他请求释放，
			// 不冷却账号——池的并发锁已防止过载，冷却反而误伤后续请求。
			log.Printf("upstream concurrent limit (transient) account=%s msg=%s", acct.Name, truncateMsg(ae.Message, 80))
		case ae.Status >= 500:
			h.cooldownErr(acct.Name, ae.Error())
		default:
			h.noteError(acct.Name)
		}
		return false
	}
	h.noteError(acct.Name)
	return false
}

// clientGone 报告「调用方自己走了」（断连/取消）。这类错误必须不记在账号头上：
// 上游收到的是 context canceled / broken pipe，冷却整池 10 分钟纯属误伤
// （2026-11-03 线上实测：客户端超时/回压交替把两个账号互相打进冷却）。
func clientGone(r *http.Request) bool {
	return r != nil && r.Context().Err() != nil
}

// isParamInvalidError 判断是否为上游「请求参数无效/模型能力不匹配」类错误。
// 这类错误由请求内容决定：换账号结果相同，绝不能冷却账号（曾因此打停整池）。
func isParamInvalidError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *upstream.ApiError
	var streamErr *upstream.UpstreamError
	var message string
	switch {
	case errors.As(err, &apiErr):
		message = apiErr.Message
	case errors.As(err, &streamErr):
		message = streamErr.Code + " " + streamErr.Msg
	default:
		return false
	}
	low := strings.ToLower(message)
	// 这一类错误的共同点：由请求内容决定，换账号/重试结果相同，而冷却账号会把整池
	// 打停 10 分钟（上游常把它们包成 HTTP 502，落进 handleUpstreamError 的
	// Status>=500 分支）。逐条：
	//   InferHub.001001005 / request param is invalid —— 参数形状错；
	//   InferHub.001001020 / request model is not multimodal —— 带图打到纯文本模型；
	//   InferHub.001001046 —— 多模态内容格式不正确；
	//   ModelArts.81029 / media conversion failed —— 图片超出媒体转换上限
	//     （downstream returned 413）；
	//   ModelArts.81001 / prompt length exceeds —— 请求超出该模型的上下文：
	//     同一个超长请求打 1M 上下文的 deepseek 成功、打 131k 的 qwen3-vl 报 81001，
	//     是「这个模型装不下」，不是账号坏了。
	return strings.Contains(low, "inferhub.001001005") || strings.Contains(low, "request param is invalid") ||
		strings.Contains(low, "inferhub.001001020") || strings.Contains(low, "request model is not multimodal") ||
		strings.Contains(low, "inferhub.001001046") ||
		strings.Contains(low, "modelarts.81029") || strings.Contains(low, "media conversion failed") ||
		strings.Contains(low, "modelarts.81001") || strings.Contains(low, "prompt length exceeds")
}

// isConcurrentLimitError 判断是否为上游并发会话上限错误（瞬时、可重试）。
func isConcurrentLimitError(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "tm.00001041") || strings.Contains(low, "并发会话")
}

func isQueueLimitError(err *upstream.ApiError) bool {
	if err == nil {
		return false
	}
	if err.Status == http.StatusTooManyRequests {
		return true
	}
	low := strings.ToLower(err.Message)
	return (err.Status == http.StatusBadRequest && isConcurrentLimitError(low)) ||
		strings.Contains(low, "inferhub.modelarts.81111.429") ||
		strings.Contains(low, "tpm limit")
}

// truncateMsg 截断到 n 字节，并回退到合法 UTF-8 边界——截断点常常落在汉字中间，
// 半个字进了 JSON 会被替换成 U+FFFD。
func truncateMsg(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error", "code": code},
	})
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := crand.Read(b); err != nil {
		seed := fmt.Sprintf("%x", time.Now().UnixNano())
		for len(seed) < n {
			seed += seed
		}
		return seed[:n]
	}
	return hex.EncodeToString(b)[:n]
}

func validChatID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func (h *Handler) loadChats() {
	if h.cfg.ConvStateFile == "" {
		return
	}
	raw, err := os.ReadFile(h.cfg.ConvStateFile)
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, &h.chats)
}

func (h *Handler) saveChats() {
	if h.cfg.ConvStateFile == "" {
		return
	}
	raw, _ := json.MarshalIndent(h.chats, "", "  ")
	if err := os.WriteFile(h.cfg.ConvStateFile+".tmp", raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(h.cfg.ConvStateFile+".tmp", h.cfg.ConvStateFile)
}
