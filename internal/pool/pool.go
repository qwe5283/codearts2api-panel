// Package pool 管理 CodeArts 账号池：token 校验/自动刷新、冷却、禁用与轮转。
package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/upstream"
)

// CoolKind 冷却原因。
type CoolKind int

const (
	CoolSoft CoolKind = iota
	CoolErr
	CoolPlan
)

// Account 一个上游账号。
type Account struct {
	Name         string `json:"name"`
	UID          string `json:"uid"`
	UserName     string `json:"user_name"`
	DefaultModel string `json:"default_model"`

	Auth   *auth.Auth       `json:"-"`
	Client *upstream.Client `json:"-"`

	mu                  sync.Mutex
	refreshMu           sync.Mutex
	checkinMu           sync.Mutex
	checkin             *CheckinStatus
	lastCheckinDay      string // 每日签到已了结的本地日期（"2006-01-02"），跨零点自然失效
	checkinClaimed      bool   // 该日真的签到成功/已领；只有它为真才显示「已签」
	newUserSettled      bool   // 新手注册活动已了结，单账号只领一次
	balanceMu           sync.Mutex
	balanceRefreshing   bool
	creditBalance       *upstream.CreditBalance
	balanceUpdatedAt    time.Time
	creditBalanceError  string
	removed             bool
	lastValidated       time.Time
	errCount            int
	coolUntil           time.Time
	coolKind            CoolKind
	disabled            bool
	disabledReason      string
	lastErr             string
	activeConcurrent    int       // 当前活跃并发请求数
	maxConcurrent       int       // 最大允许并发数
	keepaliveLastPing   time.Time // 最后保活心跳时间
}

// Config 池配置。
type Config struct {
	ErrThreshold     int
	ErrCooldown      time.Duration
	SoftCooldown     time.Duration
	RefreshSkew      time.Duration        // token 到期前多久自动刷新
	MaxConcurrent    int                  // 单账号最大并发数（默认 5）
	KeepaliveWindow  time.Duration        // 保活心跳窗口，超过此时间无活动则发送心跳
	LoginConfig      upstream.LoginConfig // OAuth/refresh 端点；零值使用生产默认
	CheckinClient    CheckinClient        // 可选活动客户端；nil 使用账号 Client
	BalanceClient    BalanceClient
	BalanceObserver BalanceObserver // 可选余额趋势采样；仅接收成功且非 nil 的结果
}

// Pool 账号池。
type Pool struct {
	cfg      Config
	state    string
	mu       sync.Mutex
	stateMu  sync.Mutex
	accounts []*Account
	nextRR   int
	// pickLeast 是 least_active 策略的轮转游标：并发相同（含都空闲）时按它决定
	// 选中谁，并移到选中账号的下一个 —— 空闲账号之间严格轮转，避免退化回 first-fit。
	pickLeast int
}

// New 构建池。
func New(auths []*auth.Auth, cfg Config, stateFile string) (*Pool, error) {
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 30 * time.Minute
	}
	if cfg.MaxConcurrent <= 0 {
		// 上游单账号最多 3 个并发会话（TM.00001041），但会话槽位释放慢，
		// 串行（1）最稳，避免撞限。可按多账号情况调高。
		cfg.MaxConcurrent = 1
	}
	if cfg.KeepaliveWindow <= 0 {
		cfg.KeepaliveWindow = 10 * time.Minute // 10 分钟无活动则保活
	}
	if cfg.LoginConfig.ClientID == "" {
		cfg.LoginConfig = upstream.DefaultLoginConfig()
	}
	p := &Pool{cfg: cfg, state: stateFile}
	for _, a := range auths {
		acct := &Account{
			Name:              a.UserID,
			UID:               a.UserID,
			UserName:          a.UserName,
			Auth:              a,
			Client:            upstream.New(120 * time.Second),
			maxConcurrent:     cfg.MaxConcurrent,
			keepaliveLastPing: time.Now(),
		}
			p.accounts = append(p.accounts, acct)
	}
	p.loadState()
	return p, nil
}

// Accounts 返回全部账号。
func (p *Pool) Accounts() []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*Account(nil), p.accounts...)
}

// Get 按名字取账号。
func (p *Pool) Get(name string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// AddAccount 动态添加账号（WebUI 登录成功后调用）。
func (p *Pool) AddAccount(a *auth.Auth) *Account {
	acct := &Account{
		Name:              a.UserID,
		UID:               a.UserID,
		UserName:          a.UserName,
		Auth:              a,
		Client:            upstream.New(120 * time.Second),
		maxConcurrent:     p.cfg.MaxConcurrent,
		keepaliveLastPing: time.Now(),
	}
	p.mu.Lock()
	for i, existing := range p.accounts {
		if existing.Name == acct.Name {
			existing.mu.Lock()
			existing.Auth.ReplaceFrom(a)
			existing.maxConcurrent = p.cfg.MaxConcurrent
			existing.lastValidated = time.Time{}
			existing.disabled = false
			existing.disabledReason = ""
			existing.lastErr = ""
			existing.removed = false
			existing.mu.Unlock()
			p.mu.Unlock()
			p.saveState()
			return existing
		}
		p.accounts[i] = existing
	}
	p.accounts = append(p.accounts, acct)
	p.mu.Unlock()
	p.saveState()
	log.Printf("pool account added name=%s uid=%s", acct.Name, a.UserID)
	return acct
}

// List 状态快照（脱敏；字段对齐 WorkBuddy 面板）。
func (p *Pool) List() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.accounts))
	now := time.Now()
	today := checkinDay(now)
	for _, a := range p.accounts {
		a.mu.Lock()
		cooling := !a.disabled && now.Before(a.coolUntil)
		until := ""
		if cooling {
			until = a.coolUntil.Format(time.RFC3339)
		}
		reason := a.disabledReason
		if reason == "" {
			reason = a.lastErr
		}
		var credit any
		if a.creditBalance != nil {
			credit = a.creditBalance.Total
		}
		expires := ""
		remaining := a.Auth.Remaining().Round(time.Minute).String()
		credentialType := "permanent"
		if t := a.Auth.ExpiresAt(); !t.IsZero() {
			expires = t.Format(time.RFC3339)
			credentialType = "temporary"
		} else {
			remaining = "permanent"
		}
		if a.Auth.Refreshable() {
			credentialType = "oauth"
		}
		out = append(out, map[string]any{
			"name":                  a.Name,
			"uid":                   a.UID,
			"nickname":              a.UserName,
			"user_name":             a.UserName,
			"note":                  a.Auth.Note,
			"default_model":         a.DefaultModel,
			"credits":               credit,
			"credit_balance":        a.creditBalance,
			"balance_updated_at":    optionalTime(a.balanceUpdatedAt),
			"credit_balance_error":  a.creditBalanceError,
			"refreshable":           a.Auth.Refreshable(),
			"credential_type":       credentialType,
			"checkin":               a.checkin,
			"checkin_done":          a.lastCheckinDay == today && a.checkinClaimed,
			"last_checkin_day":      a.lastCheckinDay,
			"new_user_settled":      a.newUserSettled,
			"disabled":              a.disabled,
			"cooling":               cooling,
			"until":                 until,
			"err_count":             a.errCount,
			"reason":                reason,
			"last_error":            a.lastErr,
			"active_concurrent":     a.activeConcurrent,
			"max_concurrent":        a.maxConcurrent,
			"token_remaining":       remaining,
			"expires_at":            expires,
		})
		a.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["uid"].(string) < out[j]["uid"].(string) })
	return out
}

// Stats 汇总。
func (p *Pool) Stats() (total, healthy, disabled, cooling int, credits float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, a := range p.accounts {
		total++
		a.mu.Lock()
		if a.creditBalance != nil {
			credits += a.creditBalance.Total
		}
		if a.disabled {
			disabled++
		} else if now.Before(a.coolUntil) {
			cooling++
		} else {
			healthy++
		}
		a.mu.Unlock()
	}
	return
}

// Enable 解禁账号。
func (p *Pool) Enable(name string) bool {
	defer p.saveState()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name || a.UID == name {
			a.mu.Lock()
			a.disabled = false
			a.disabledReason = ""
			a.lastErr = ""
			a.mu.Unlock()
			return true
		}
	}
	return false
}

// ClearCooldown 清冷却。
func (p *Pool) ClearCooldown(name string) bool {
	defer p.saveState()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name || a.UID == name {
			a.mu.Lock()
			a.coolUntil = time.Time{}
			a.coolKind = CoolSoft
			a.mu.Unlock()
			return true
		}
	}
	return false
}

// SetNote 设置账号备注（写盘）。备注是纯本地字段，不参与任何上游请求。
// 空字符串表示清除备注。返回 false 表示账号不存在或写盘失败。
func (p *Pool) SetNote(name, note string) bool {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name || a.UID == name {
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil || target.Auth == nil {
		return false
	}
	target.mu.Lock()
	target.Auth.Note = strings.TrimSpace(note)
	target.mu.Unlock()
	if err := target.Auth.Save(); err != nil {
		log.Printf("pool save note account=%s: %v", name, err)
		return false
	}
	return true
}

// SyncToDir 用磁盘 auths 全量对齐。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	byUID := map[string]*Account{}
	for _, a := range p.accounts {
		byUID[a.UID] = a
	}
	next := make([]*Account, 0, len(auths))
	for _, a := range auths {
		if existing, ok := byUID[a.UserID]; ok {
			existing.mu.Lock()
			existing.Auth.ReplaceFrom(a)
			existing.maxConcurrent = p.cfg.MaxConcurrent
			existing.mu.Unlock()
			next = append(next, existing)
			delete(byUID, a.UserID)
		} else {
			acct := &Account{
				Name:              a.UserID,
				UID:               a.UserID,
				UserName:          a.UserName,
				Auth:              a,
				Client:            upstream.New(120 * time.Second),
				maxConcurrent:     p.cfg.MaxConcurrent,
				keepaliveLastPing: time.Now(),
			}
			next = append(next, acct)
		}
	}
	p.accounts = next
}

// PickExcluding 按策略挑一个健康账号，不并发查询积分余额。
// strategy 取值：first（默认）/ credits / round_robin / least_active。
func (p *Pool) PickExcluding(tried map[string]bool, strategy string) *Account {
	return p.PickExcludingMatching(tried, strategy, nil)
}

// PickExcludingMatching 在 eligible 非 nil 时只在其中的账号里选号。
func (p *Pool) PickExcludingMatching(tried map[string]bool, strategy string, eligible map[string]bool) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	available := func(a *Account) bool {
		if a == nil || eligible != nil && !eligible[a.Name] || tried != nil && tried[a.Name] {
			return false
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.disabled && now.After(a.coolUntil)
	}
	if strategy == "credits" {
		var selected *Account
		highest := 0.0
		for _, a := range p.accounts {
			if !available(a) {
				continue
			}
			if a.creditBalance != nil && (selected == nil || a.creditBalance.Total > highest) {
				selected, highest = a, a.creditBalance.Total
			}
		}
		if selected != nil {
			return selected
		}
	}
	if strategy == "round_robin" && len(p.accounts) > 0 {
		start := p.nextRR % len(p.accounts)
		for offset := 0; offset < len(p.accounts); offset++ {
			i := (start + offset) % len(p.accounts)
			if available(p.accounts[i]) {
				p.nextRR = (i + 1) % len(p.accounts)
				return p.accounts[i]
			}
		}
		return nil
	}
	if strategy == "least_active" && len(p.accounts) > 0 {
		// 在未排除的健康账号里挑「活跃并发最少」的那个；并发相同时由 pickLeast 游标
		// 打破平局并前移 —— 空闲账号之间是严格轮转的。
		//
		// first-fit 的问题：多账号池会把流量全压给列表里第一个账号，它的上游会话
		// （每账号 3 个，按 chat_id 计）先打满、开始回 TM.00001041，其它账号空转，
		// 用量与积分也随之严重倾斜。实测（2 账号、max_concurrent=2、4 并发 4 会话）：
		// first-fit 墙钟 66~67s、排队重试 17 次全落同一账号；least_active 2.8~3.1s、
		// 0 重试，账号分布 11:9。
		var best *Account
		bestActive := 0
		n := len(p.accounts)
		for i := 0; i < n; i++ {
			a := p.accounts[(p.pickLeast+i)%n]
			if !available(a) {
				continue
			}
			a.mu.Lock()
			active := a.activeConcurrent
			a.mu.Unlock()
			if best == nil || active < bestActive {
				best, bestActive = a, active
			}
		}
		if best != nil {
			for i := 0; i < n; i++ {
				if p.accounts[i] == best {
					p.pickLeast = (i + 1) % n
					break
				}
			}
			return best
		}
		return nil
	}
	for _, a := range p.accounts {
		if available(a) {
			return a
		}
	}
	return nil
}

// Validate 校验并（必要时）自动刷新 token。带 5 分钟缓存。
func (p *Pool) Validate(a *Account) (bool, error) {
	a.mu.Lock()
	if a.removed {
		a.mu.Unlock()
		return false, fmt.Errorf("account removed")
	}
	if time.Since(a.lastValidated) < 5*time.Minute && !a.disabled {
		ok := true
		a.mu.Unlock()
		return ok, nil
	}
	a.mu.Unlock()

	authz := a.Auth
	if authz.Expired() && !authz.Refreshable() {
		p.Disable(a.Name, "token expired; re-login required")
		return false, nil
	}
	if authz.ExpiringSoon(p.cfg.RefreshSkew) || authz.Expired() {
		if !authz.Refreshable() {
			// 没有 refresh_token（华为云 ticket 通道不返回），不立即 disable，
			// 未过期时只打告警；到期后由上面的分支明确标记为需重新登录。
			log.Printf("pool token account=%s expiring soon (remaining=%s) but no refresh_token, re-login required",
				a.Name, authz.Remaining().Round(time.Minute))
			a.mu.Lock()
			a.lastValidated = time.Now()
			a.mu.Unlock()
			return true, nil
		}
		if err := p.RefreshToken(a.Name); err != nil {
			var expired *upstream.RefreshTokenExpiredError
			if errors.As(err, &expired) {
				p.Disable(a.Name, "refresh_token expired; re-login required")
				return false, nil
			}
			// 网络/5xx 属于可重试错误：保持账号可调度，由后续
			// watchdog tick 重试，不得把短暂故障固化为永久禁用。
			return false, err
		}
	}
	a.mu.Lock()
	a.lastValidated = time.Now()
	a.mu.Unlock()
	return true, nil
}

// Cooldown 冷却账号。
func (p *Pool) Cooldown(name string, kind CoolKind, dur time.Duration, reason string) {
	defer p.saveState()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		a.coolUntil = time.Now().Add(dur)
		a.coolKind = kind
		a.lastErr = reason
		a.mu.Unlock()
		log.Printf("pool cooldown account=%s kind=%d dur=%s reason=%s", name, kind, dur, reason)
	}
}

// NoteError 累计错误。
func (p *Pool) NoteError(name string, threshold int, cooldown time.Duration) {
	defer p.saveState()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		a.errCount++
		if a.errCount >= threshold {
			a.coolUntil = time.Now().Add(cooldown)
			a.lastErr = "consecutive errors"
		}
		a.mu.Unlock()
	}
}

// NoteSuccess 清零错误计数，并在活跃使用期间按需刷新积分缓存。
func (p *Pool) NoteSuccess(name string) {
	p.RefreshBalanceAfterSuccess(name)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			a.errCount = 0
			a.mu.Unlock()
		}
	}
}

// Disable 禁用账号。
func (p *Pool) Disable(name, reason string) {
	defer p.saveState()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			a.disabled = true
			a.disabledReason = reason
			a.lastErr = reason
			a.mu.Unlock()
			log.Printf("pool disable account=%s reason=%s", name, reason)
		}
	}
}

// Healthy 查询账号可用。
func (p *Pool) Healthy(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			h := !a.disabled && time.Now().After(a.coolUntil)
			a.mu.Unlock()
			return h
		}
	}
	return false
}

// Enabled 报告账号是否启用；冷却期间仍需续期。
func (p *Pool) Enabled(name string) bool {
	a := p.Get(name)
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.disabled
}

// AcquireLock 尝试获取账号的并发请求锁。返回 true 表示成功，false 表示已达上限。
func (p *Pool) AcquireLock(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.activeConcurrent >= a.maxConcurrent {
			log.Printf("pool concurrent limit reached account=%s current=%d max=%d",
				name, a.activeConcurrent, a.maxConcurrent)
			return false
		}
		a.activeConcurrent++
		return true
	}
	return false
}

// ReleaseLock 释放账号的并发请求锁，并唤醒等待槽位的请求。
func (p *Pool) ReleaseLock(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			if a.activeConcurrent > 0 {
				a.activeConcurrent--
			}
			a.mu.Unlock()
			break
		}
	}
	// 唤醒等待槽位的 AcquireLockWait（轮询方式无需 broadcast，此处保留语义占位）。
}

// AcquireLockWait 阻塞等待账号并发槽位，超时返回 false。
//
// 上游单账号并发会话上限 3，本池 maxConcurrent 默认 2 留余量。
// 高并发请求在此排队等待槽位释放，而非立即失败跳号。
// timeout=0 时等价于非阻塞 AcquireLock。
func (p *Pool) AcquireLockWait(name string, timeout time.Duration) bool {
	return p.AcquireLockWaitContext(context.Background(), name, timeout)
}

// AcquireLockWaitContext 等待并发槽位，取消或超时后不再获取槽位。
func (p *Pool) AcquireLockWaitContext(ctx context.Context, name string, timeout time.Duration) bool {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	for {
		if ctx.Err() != nil {
			return false
		}
		p.mu.Lock()
		acquired := false
		for _, a := range p.accounts {
			if a.Name != name {
				continue
			}
			a.mu.Lock()
			if a.activeConcurrent < a.maxConcurrent {
				a.activeConcurrent++
				acquired = true
			}
			a.mu.Unlock()
			break
		}
		p.mu.Unlock()
		if acquired {
			return true
		}
		if timeout <= 0 {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// NeedKeepalive 检查账号是否需要保活心跳。
func (p *Pool) NeedKeepalive(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		// 超过 KeepaliveWindow 无活动则需保活
		return time.Since(a.keepaliveLastPing) > p.cfg.KeepaliveWindow
	}
	return false
}

// PingKeepalive 更新账号的保活心跳时间。
func (p *Pool) PingKeepalive(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			a.keepaliveLastPing = time.Now()
			a.mu.Unlock()
			break
		}
	}
}

// CheckAndRefreshTokenWithin 在剩余有效期低于 skew 时主动刷新 token。
//
// skew 来自 scheduler 的 refresh_skew（配置项 watch.refresh_skew_minutes）。
// 此前该配置从未生效：原实现硬编码 1 小时阈值，调度器传进来的 RefreshSkew
// 只出现在日志里，README 写的「提前刷新时间」实际不起作用。
func (p *Pool) CheckAndRefreshTokenWithin(name string, skew time.Duration) error {
	if skew <= 0 {
		skew = time.Hour
	}
	p.mu.Lock()
	var acct *Account
	for _, a := range p.accounts {
		if a.Name == name {
			acct = a
			break
		}
	}
	p.mu.Unlock()
	if acct == nil {
		return fmt.Errorf("account not found: %s", name)
	}

	acct.mu.Lock()
	remaining := acct.Auth.Remaining()
	hasRefresh := acct.Auth.Refreshable()
	acct.mu.Unlock()

	if remaining > skew {
		return nil
	}
	if !hasRefresh {
		// ticket 登录不返回 refresh_token，跳过无意义的周期刷新。
		return nil
	}
	log.Printf("pool proactive refresh account=%s remaining=%s skew=%s",
		name, remaining.Round(time.Minute), skew)
	return p.RefreshToken(name)
}

// CheckAndRefreshToken 检查 token 是否快过期并主动刷新（比 scheduler 更积极）。
func (p *Pool) CheckAndRefreshToken(name string) error {
	return p.CheckAndRefreshTokenWithin(name, time.Hour)
}

// RefreshToken 手动刷新指定账号的 token。
func (p *Pool) RefreshToken(name string) error {
	p.mu.Lock()
	var acct *Account
	for _, a := range p.accounts {
		if a.Name == name {
			acct = a
			break
		}
	}
	p.mu.Unlock()
	if acct == nil {
		return fmt.Errorf("account not found: %s", name)
	}
	requestedToken := acct.Auth.Refresh()
	acct.refreshMu.Lock()
	defer acct.refreshMu.Unlock()

	authz := acct.Auth
	refreshToken := authz.Refresh()
	if refreshToken != requestedToken {
		return nil
	}
	acct.mu.Lock()
	removed := acct.removed
	acct.mu.Unlock()
	if removed {
		return fmt.Errorf("account removed")
	}
	if refreshToken == "" {
		return fmt.Errorf("no refresh_token available")
	}
	if !authz.Refreshable() {
		return fmt.Errorf("incomplete refresh credentials; re-login required")
	}
	dpopPrivateJWK := authz.DPoPPrivateJWK()
	if dpopPrivateJWK["d"] == "" {
		return fmt.Errorf("no DPoP private key available; re-login required")
	}

	cfg := p.loginConfig()
	cfg.ClientID = authz.ClientIDOr(cfg.ClientID)
	resp, err := acct.Client.RefreshToken(context.Background(), cfg, refreshToken, authz.Verifier(), upstream.DPoPPrivateJWK(dpopPrivateJWK))
	if err != nil {
		if authz.Refresh() != refreshToken {
			return nil
		}
		return fmt.Errorf("refresh failed: %w", err)
	}
	if authz.Refresh() != refreshToken {
		return nil
	}

	if _, err := authz.UpdateCredentialsIfCurrent(refreshToken,
		resp.Credentials.SecurityToken,
		resp.Credentials.AccessKeyID,
		resp.Credentials.SecretAccessKey,
		resp.Credentials.Expiration,
		resp.RefreshToken,
	); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	acct.mu.Lock()
	acct.disabled = false
	acct.disabledReason = ""
	acct.lastErr = ""
	acct.lastValidated = time.Now()
	acct.mu.Unlock()
	log.Printf("pool token refreshed manually account=%s", name)
	return nil
}

// RefreshAllTokens 批量刷新所有临期 token（用于 cron 或脚本）。
func (p *Pool) RefreshAllTokens() int {
	count := 0
	for _, acct := range p.Accounts() {
		if err := p.CheckAndRefreshToken(acct.Name); err == nil {
			count++
		}
	}
	return count
}

// Acquire 尝试获取账号并发 slot（返回 true 表示成功，调用方需随后 Release）。
func (p *Pool) Acquire(name string) bool {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return false
	}
	target.mu.Lock()
	defer target.mu.Unlock()

	// 检查是否超过最大并发限制
	if target.activeConcurrent >= target.maxConcurrent {
		return false
	}
	// 检查是否正在冷却
	if !time.Now().After(target.coolUntil) {
		return false
	}
	// 增加活跃计数
	target.activeConcurrent++
	target.keepaliveLastPing = time.Now()
	return true
}

// Release 释放账号并发 slot。
func (p *Pool) Release(name string) {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if target.activeConcurrent > 0 {
		target.activeConcurrent--
	}
}

// CheckKeepalive 检查是否需要保活心跳。
func (p *Pool) CheckKeepalive(name string) bool {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return false
	}
	// 先取窗口再加锁：与 NeedKeepalive（p.mu → a.mu）保持同一把锁顺序。
	window := p.keepaliveWindow()
	target.mu.Lock()
	defer target.mu.Unlock()

	// 如果超过保活窗口无活动，需要保活
	needsKeepalive := time.Since(target.keepaliveLastPing) > window
	if needsKeepalive {
		target.keepaliveLastPing = time.Now()
	}
	return needsKeepalive
}

// SetKeepalive 手动更新保活时间。
func (p *Pool) SetKeepalive(name string) {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	target.keepaliveLastPing = time.Now()
}

// GetConcurrentStats 获取账号并发统计信息。
func (p *Pool) GetConcurrentStats() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]map[string]any, 0, len(p.accounts))
	now := time.Now()
	for _, a := range p.accounts {
		a.mu.Lock()
		cooling := !a.disabled && now.Before(a.coolUntil)
		concurrent := a.activeConcurrent
		maxConc := a.maxConcurrent
		lastPing := a.keepaliveLastPing
		a.mu.Unlock()

		out = append(out, map[string]any{
			"name":              a.Name,
			"uid":               a.UID,
			"user_name":         a.UserName,
			"active_concurrent": concurrent,
			"max_concurrent":    maxConc,
			"utilization":       float64(concurrent) / float64(maxConc),
			"last_keepalive":    lastPing.Format(time.RFC3339),
			"cooling":           cooling,
			"disabled":          a.disabled,
		})
	}
	return out
}

// TryAcquire 尝试获取账号的并发槽位，成功返回 true 并增加计数。
func (p *Pool) TryAcquire(name string) bool {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()

	if target == nil {
		return false
	}

	target.mu.Lock()
	defer target.mu.Unlock()

	if target.disabled || time.Now().Before(target.coolUntil) {
		return false
	}

	if target.activeConcurrent >= target.maxConcurrent {
		log.Printf("pool concurrent limit reached account=%s active=%d max=%d",
			name, target.activeConcurrent, target.maxConcurrent)
		return false
	}

	target.activeConcurrent++
	target.keepaliveLastPing = time.Now()
	return true
}

// NeedsKeepalive 检查账号是否需要保活心跳。
func (p *Pool) NeedsKeepalive(name string) bool {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()

	if target == nil {
		return false
	}

	// 先取窗口再加锁：与 NeedKeepalive（p.mu → a.mu）保持同一把锁顺序。
	window := p.keepaliveWindow()
	target.mu.Lock()
	defer target.mu.Unlock()

	// 超过窗口期无活动则需保活
	return time.Since(target.keepaliveLastPing) > window
}

// UpdateKeepalive 更新账号的最后保活时间。
func (p *Pool) UpdateKeepalive(name string) {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()

	if target != nil {
		target.mu.Lock()
		target.keepaliveLastPing = time.Now()
		target.mu.Unlock()
	}
}

// SetMaxConcurrentAll 热更新单账号并发上限：同步池配置与所有已加载账号，
// 之后新加入/同步进来的账号也沿用该值（面板保存 max_concurrent 后立即生效）。
func (p *Pool) SetMaxConcurrentAll(max int) {
	if max <= 0 {
		return
	}
	p.mu.Lock()
	p.cfg.MaxConcurrent = max
	targets := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()
	for _, a := range targets {
		a.mu.Lock()
		a.maxConcurrent = max
		a.mu.Unlock()
	}
}

// SetKeepaliveWindow 热更新保活窗口（保活判定依据）。
func (p *Pool) SetKeepaliveWindow(window time.Duration) {
	if window <= 0 {
		return
	}
	p.mu.Lock()
	p.cfg.KeepaliveWindow = window
	p.mu.Unlock()
}

// SetLoginClientID 热更新 OAuth client_id（token 续期路径使用）。
// 空值表示"沿用现有值"，与密钥留空的语义一致。
func (p *Pool) SetLoginClientID(id string) {
	if id == "" {
		return
	}
	p.mu.Lock()
	p.cfg.LoginConfig.ClientID = id
	p.mu.Unlock()
}

// keepaliveWindow 读当前保活窗口。读方不加锁会与面板热更新竞争，统一走这里。
func (p *Pool) keepaliveWindow() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.KeepaliveWindow
}

// loginConfig 当前 OAuth 配置快照（client_id 可热改）。
func (p *Pool) loginConfig() upstream.LoginConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.LoginConfig
}

// SetMaxConcurrent 设置账号的最大并发数。
func (p *Pool) SetMaxConcurrent(name string, max int) {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()

	if target != nil {
		target.mu.Lock()
		target.maxConcurrent = max
		target.mu.Unlock()
	}
}

func (p *Pool) loadState() {
	if p.state == "" {
		return
	}
	raw, err := os.ReadFile(p.state)
	if err != nil {
		return
	}
	var data []struct {
		Name      string                   `json:"name"`
		ErrCount  int                      `json:"err_count"`
		CoolUntil time.Time                `json:"cool_until"`
		Disabled  bool                     `json:"disabled"`
		Reason    string                   `json:"reason"`
		LastErr   string                   `json:"last_error"`
		Credit    *upstream.CreditBalance  `json:"credit_balance"`
		BalanceAt time.Time                `json:"balance_updated_at"`
		// 签到账本：重启后仍要知道「今天已签」「新手已领」，否则重启即重复打上游。
		// checkin_claimed 用指针读：更早的账本只有日期（写入即代表已签），不能因缺字段
		// 把已签的账号在升级当天显示成未签。
		LastCheckinDay *string `json:"last_checkin_day"`
		CheckinClaimed *bool   `json:"checkin_claimed"`
		NewUserSettled bool    `json:"new_user_settled"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	for _, a := range p.accounts {
		for _, s := range data {
			if s.Name == a.Name {
				a.mu.Lock()
				a.errCount = s.ErrCount
				a.coolUntil = s.CoolUntil
				a.disabled = s.Disabled
				a.disabledReason = s.Reason
				a.lastErr = s.LastErr
				a.creditBalance, a.balanceUpdatedAt = s.Credit, s.BalanceAt
				a.lastCheckinDay, a.checkinClaimed = "", false
				if s.LastCheckinDay != nil {
					a.lastCheckinDay = *s.LastCheckinDay
					a.checkinClaimed = s.CheckinClaimed == nil || *s.CheckinClaimed
				}
				a.newUserSettled = s.NewUserSettled
				a.mu.Unlock()
			}
		}
	}
}

func (p *Pool) saveState() {
	if err := p.saveStateErr(); err != nil {
		log.Printf("save pool state: %v", err)
	}
}

func (p *Pool) saveStateErr() error {
	if p.state == "" {
		return nil
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	type entry struct {
		Name      string                   `json:"name"`
		ErrCount  int                      `json:"err_count"`
		CoolUntil time.Time                `json:"cool_until"`
		Disabled  bool                     `json:"disabled"`
		Reason    string                   `json:"reason"`
		LastErr   string                   `json:"last_error"`
		Credit    *upstream.CreditBalance  `json:"credit_balance"`
		BalanceAt time.Time                `json:"balance_updated_at"`
		// 签到账本：持久化「今日已签」「新手已领」，重启后不重复请求上游。
		LastCheckinDay string `json:"last_checkin_day,omitempty"`
		CheckinClaimed bool   `json:"checkin_claimed,omitempty"`
		NewUserSettled bool   `json:"new_user_settled,omitempty"`
	}
	accounts := p.Accounts()
	out := make([]entry, 0, len(accounts))
	for _, a := range accounts {
		a.mu.Lock()
		out = append(out, entry{
			Name:      a.Name,
			ErrCount:  a.errCount,
			CoolUntil: a.coolUntil,
			Disabled:  a.disabled,
			Reason:    a.disabledReason,
			LastErr:   a.lastErr,
			Credit:    a.creditBalance, BalanceAt: a.balanceUpdatedAt,
			LastCheckinDay: a.lastCheckinDay, CheckinClaimed: a.checkinClaimed, NewUserSettled: a.newUserSettled,
		})
		a.mu.Unlock()
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.state), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(p.state), ".pool-*.tmp")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p.state)
}
