package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"codearts2api-panel/internal/reqlog"
)

// maxErrorBodyBytes 只保留足够解析错误消息的响应前缀，失败响应不应长期占用大块内存。
const maxErrorBodyBytes = 64 << 10

type LogEntry struct {
	TS   time.Time `json:"ts"`
	Ch   string    `json:"ch"`
	Text string    `json:"text"`
}

// Ring keeps a bounded log snapshot; HTTP bodies and credentials are never
// added by request recording. Redaction also protects accidental library logs.
type Ring struct {
	mu       sync.Mutex
	entries  []LogEntry
	capacity int
}

var credentialLogPattern = regexp.MustCompile(`(?i)(["']?(?:access_key_id|secret_access_key|security_token|securitytoken|cloud_dragon_token|refresh_token|access_token|code_verifier|dpop_private_key|dpop|authorization|x-auth-token|x-security-token|api_key|admin_key|secret)["']?\s*[:=]\s*)[^\r\n]*`)

func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 500
	}
	return &Ring{capacity: capacity}
}

func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(strings.TrimSpace(string(p)), "\n") {
		if line == "" {
			continue
		}
		line = credentialLogPattern.ReplaceAllString(line, `${1}"[REDACTED]"`)
		if len(line) > 4096 {
			line = line[:4096] + "…"
		}
		channel := "sys"
		if strings.HasPrefix(line, "chat ") {
			channel = "chat"
		}
		if strings.Contains(line, "checkin ") || strings.Contains(line, "credit checkin ") {
			channel = "task"
		}
		r.entries = append(r.entries, LogEntry{TS: time.Now(), Ch: channel, Text: line})
		if len(r.entries) > r.capacity {
			r.entries = r.entries[len(r.entries)-r.capacity:]
		}
	}
	return len(p), nil
}

func (r *Ring) Snapshot() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

func (h *Handler) LogWriter() io.Writer { return h.logs }

func (h *Handler) adminLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": h.logs.Snapshot()})
}

// attemptErrorLimit 面板「尝试次数」列 tooltip 的内存上限：只保留最近 50 条错误。
const attemptErrorLimit = 50

// attemptErrors 按 request_id 记录「重试过的请求」最后一次上游错误。
//
// 只存在内存里、不落盘：归档（data/requests）继续只写脱敏元数据，错误原文随进程
// 重启消失，面板上跨重启的旧记录也就没有 tooltip 可显示。
type attemptErrors struct {
	mu    sync.Mutex
	order []string
	byID  map[string]string
	limit int
}

func newAttemptErrors(limit int) *attemptErrors {
	if limit <= 0 {
		limit = attemptErrorLimit
	}
	return &attemptErrors{byID: map[string]string{}, limit: limit}
}

// note 记下某请求最后一次上游错误；同一个 request_id 重新入队不重复占位。
func (a *attemptErrors) note(requestID, message string) {
	if a == nil || requestID == "" || message == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.byID[requestID]; !ok {
		a.order = append(a.order, requestID)
	}
	a.byID[requestID] = message
	for len(a.order) > a.limit {
		stale := a.order[0]
		a.order = a.order[1:]
		delete(a.byID, stale)
	}
}

// snapshot 返回副本，供 /admin/api/usage 直接序列化。
func (a *attemptErrors) snapshot() map[string]string {
	out := map[string]string{}
	if a == nil {
		return out
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, message := range a.byID {
		out[id] = message
	}
	return out
}

// usageResponse 是 /admin/api/usage 的返回：进程指标与归档回看之外，附上重试请求
// 的上游错误（内存态，见 attemptErrors）。
type usageResponse struct {
	reqlog.Snapshot
	AttemptErrors map[string]string `json:"attempt_errors"`
}

func (h *Handler) adminUsage(w http.ResponseWriter, r *http.Request) {
	// window=1：面板按时间范围要的是聚合，不是逐条记录。30 天的归档逐条传既慢又没人看。
	if r.URL.Query().Get("window") != "" {
		from, to := usageWindowBounds(r)
		stats, err := h.cfg.RequestLog.WindowStats(from, to)
		if err != nil {
			// 归档读不完整：数字仍然可用（坏文件被跳过），把原因告诉面板而不是整页报错。
			fmt.Fprintln(h.logs, "usage window: "+err.Error())
		}
		writeJSON(w, http.StatusOK, stats)
		return
	}
	snap := h.cfg.RequestLog.Snapshot()
	// 进程内指标随重启清零，归档（data/requests）才是能跨重启的那本账：
	// 只要归档里有记录就带上，页面据此一直显示历史 —— 不能只在「本进程还没请求」
	// 时才回看，否则重启后跑一条请求，整页就塌成一条。
	snap.History = h.cfg.RequestLog.History(reqlog.HistoryLimit)
	writeJSON(w, http.StatusOK, usageResponse{Snapshot: snap, AttemptErrors: h.attemptErrs.snapshot()})
}

// usageWindowBounds 解析 from/to：RFC3339 或 Unix 毫秒，缺省/非法即该侧不设界。
func usageWindowBounds(r *http.Request) (from, to time.Time) {
	parse := func(key string) time.Time {
		raw := strings.TrimSpace(r.URL.Query().Get(key))
		if raw == "" {
			return time.Time{}
		}
		if at, err := time.Parse(time.RFC3339, raw); err == nil {
			return at
		}
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return time.UnixMilli(ms)
		}
		return time.Time{}
	}
	return parse("from"), parse("to")
}

type responseObserver struct {
	http.ResponseWriter
	status    int
	wrote     bool
	started   time.Time
	firstByte time.Duration
	errorBody []byte
}

func (w *responseObserver) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote, w.status = true, status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseObserver) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.firstByte == 0 {
		w.firstByte = time.Since(w.started)
	}
	if w.status >= http.StatusBadRequest && len(w.errorBody) < maxErrorBodyBytes {
		body := p
		if remaining := maxErrorBodyBytes - len(w.errorBody); len(body) > remaining {
			body = body[:remaining]
		}
		w.errorBody = append(w.errorBody, body...)
	}
	return w.ResponseWriter.Write(p)
}

func (w *responseObserver) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responseObserver) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func captureUsage(event *reqlog.Event, usage map[string]any) {
	number := func(key string) float64 {
		switch value := usage[key].(type) {
		case float64:
			return value
		case int:
			return float64(value)
		case int64:
			return float64(value)
		default:
			return 0
		}
	}
	event.PromptTokens = int64(number("prompt_tokens"))
	event.CompletionTokens = int64(number("completion_tokens"))
	event.TotalTokens = int64(number("total_tokens"))
	if _, ok := usage["credit"]; ok {
		event.Credit, event.HasCredit = number("credit"), true
	}
	// 上游报过缓存就顺手补齐未命中数：面板的命中率按 hit/(hit+miss) 算，
	// 只留命中数的话「命中 0」与「上游没说」在归档里长得一样。
	sawCache := false
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if hit, ok := details["cached_tokens"].(float64); ok {
			event.CacheHitTokens, sawCache = int64(hit), true
		}
	}
	if hit, ok := usage["prompt_cache_hit_tokens"].(float64); ok {
		event.CacheHitTokens, sawCache = int64(hit), true
	}
	// 前缀缓存只有「命中/未命中」两种 prompt token，未命中数 = prompt - hit。
	// 全命中时 miss 为 0 并被 omitempty 省掉，命中数照旧在，不会变成「没上报」。
	if miss := event.PromptTokens - event.CacheHitTokens; sawCache && miss > 0 {
		event.CacheMissTokens = miss
	}
}

const (
	// logLabelLimit 模型名、账户名这类标签的单行字节上限。
	logLabelLimit = 160
	// errorTextLimit 错误摘要的字节上限：面板「尝试次数」列的 tooltip 直接用这段
	// 文本，所以比标签宽一些，容得下上游错误码 + 原文。
	errorTextLimit = 300
)

// safeLogLabel 把一段外部文本压成单行，并按标签上限截断。
func safeLogLabel(value string) string { return safeLogText(value, logLabelLimit) }

// safeLogText 去掉控制字符（换行、制表符等）后按上限截断，且不切断 UTF-8。
func safeLogText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r < 32 {
			return -1
		}
		return r
	}, value)
	if len(value) > limit {
		value = value[:limit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}

func requestErrorMessage(body []byte) string {
	var response struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &response)
	return response.Error.Message
}

func safeRequestError(value string) string {
	return credentialLogPattern.ReplaceAllString(safeLogText(value, errorTextLimit), `${1}"[REDACTED]"`)
}

func (h *Handler) finishRequest(event reqlog.Event, observed *responseObserver, started time.Time) {
	event.Status = observed.status
	event.DurationMs = time.Since(started).Milliseconds()
	event.TTFBMs = observed.firstByte.Milliseconds()
	if !event.OK {
		event.Error = safeRequestError(event.Error)
		if event.Error == "" {
			event.Error = safeRequestError(requestErrorMessage(observed.errorBody))
		}
		if event.Error == "" {
			event.Error = fmt.Sprintf("request failed with HTTP %d", event.Status)
		}
	} else {
		event.Error = ""
	}
	h.cfg.RequestLog.Record(event)
	// 运行日志带上首字延迟、排队时长、尝试次数与账号：面板「运行日志」页据此一眼
	// 看出慢在排队还是慢在模型、是否换过号（这些字段请求记录里本来就有）。
	line := fmt.Sprintf("chat request=%s model=%s status=%d ok=%t duration_ms=%d ttfb_ms=%d tokens=%d",
		event.RequestID, safeLogLabel(event.Model), event.Status, event.OK, event.DurationMs, event.TTFBMs, event.TotalTokens)
	if event.QueueMs > 0 {
		line += fmt.Sprintf(" queue_ms=%d", event.QueueMs)
	}
	if event.Attempts > 1 {
		line += fmt.Sprintf(" attempts=%d", event.Attempts)
	}
	if event.Account != "" {
		line += " account=" + event.Account
	}
	if event.Error != "" {
		line += fmt.Sprintf(" error=%q", event.Error)
	}
	fmt.Fprintln(h.logs, line)
}
