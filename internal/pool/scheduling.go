package pool

import (
	"context"
	"sync"
	"time"
)

type schedulerState struct {
	mu             sync.Mutex
	reserveCredits float64
	changed        chan struct{}
}

func (s *schedulerState) init() {
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
}

func (p *Pool) signalScheduleChange() {
	p.scheduler.mu.Lock()
	p.scheduler.init()
	close(p.scheduler.changed)
	p.scheduler.changed = make(chan struct{})
	p.scheduler.mu.Unlock()
}

// SetReserveCredits 设置保留积分；0 关闭。余额未知不视为 0。
func (p *Pool) SetReserveCredits(credits float64) {
	if credits < 0 {
		return
	}
	p.scheduler.mu.Lock()
	p.scheduler.reserveCredits = credits
	p.scheduler.mu.Unlock()
	p.signalScheduleChange()
}

// reserveBlocked 调用方须持有 a.mu；只拦接单，不影响签到、保活或余额刷新。
func (p *Pool) reserveBlocked(a *Account, now time.Time) bool {
	p.scheduler.mu.Lock()
	reserve := p.scheduler.reserveCredits
	p.scheduler.mu.Unlock()
	return reserve > 0 && a.creditBalance != nil && a.creditBalance.Total <= reserve
}

// PickByUID 返回健康、余额未被保留且有空槽的指定账号，供软粘性快路径使用。
func (p *Pool) PickByUID(uid string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, a := range p.accounts {
		if a.Name != uid {
			continue
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.disabled || a.removed || now.Before(a.coolUntil) || a.activeConcurrent >= a.maxConcurrent || p.reserveBlocked(a, now) {
			return nil
		}
		return a
	}
	return nil
}

// AcquireScheduled 按策略寻找并原子占有一个空槽；全池满时返回 nil。
// 偏好（会话亲和、未绑定账号）只影响候选顺序，绝不让满槽账号挡住空闲账号。
func (p *Pool) AcquireScheduled(strategy, sticky string, bound map[string]bool, tried map[string]bool, eligible map[string]bool) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	available := func(a *Account) bool {
		if a == nil || tried[a.Name] || eligible != nil && !eligible[a.Name] {
			return false
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.disabled && !a.removed && now.After(a.coolUntil) && !p.reserveBlocked(a, now)
	}
	acquire := func(a *Account) *Account {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.activeConcurrent >= a.maxConcurrent {
			return nil
		}
		a.activeConcurrent++
		a.keepaliveLastPing = now
		return a
	}
	if sticky != "" {
		for _, a := range p.accounts {
			if a.Name == sticky && available(a) {
				if got := acquire(a); got != nil {
					return got
				}
				break
			}
		}
	}
	candidates := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		if available(a) {
			candidates = append(candidates, a)
		}
	}
	if strategy == "round_robin" && len(p.accounts) > 0 {
		// nextRR 是全账号池下标，不能套在过滤后的候选切片上。
		candidates = candidates[:0]
		start := p.nextRR % len(p.accounts)
		for offset := 0; offset < len(p.accounts); offset++ {
			a := p.accounts[(start+offset)%len(p.accounts)]
			if available(a) {
				candidates = append(candidates, a)
			}
		}
	}
	if strategy == "credits" {
		score := func(a *Account) float64 {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.creditBalance == nil {
				return -1 // 未知排后，不当作 0
			}
			return a.creditBalance.Total
		}
		scores := make(map[string]float64, len(candidates))
		for _, a := range candidates {
			scores[a.Name] = score(a)
		}
		for i := 1; i < len(candidates); i++ {
			for j := i; j > 0 && scores[candidates[j-1].Name] < scores[candidates[j].Name]; j-- {
				candidates[j-1], candidates[j] = candidates[j], candidates[j-1]
			}
		}
	}
	// 新会话先占用尚未绑定会话的账号；同会话分配由调用方串行。
	for pass := 0; pass < 2; pass++ {
		for _, a := range candidates {
			// 没有粘性绑定时，优先尚未绑定任何会话的账号。
			if sticky == "" && ((pass == 0) == bound[a.Name]) {
				continue
			}
			if got := acquire(a); got != nil {
				if strategy == "round_robin" {
					for i, original := range p.accounts {
						if original == a {
							p.nextRR = (i + 1) % len(p.accounts)
							break
						}
					}
				}
				return got
			}
		}
	}
	return nil
}

// WaitScheduled 只在全池无可用槽时等待；槽位或配置变化后重新扫描全池。
// 返回 nil 时调用方必须检查 ctx.Err()，区分取消与等待预算耗尽。
func (p *Pool) WaitScheduled(ctx context.Context, timeout time.Duration, strategy, sticky string, bound map[string]bool, tried map[string]bool, eligible map[string]bool) *Account {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		if a := p.AcquireScheduled(strategy, sticky, bound, tried, eligible); a != nil {
			return a
		}
		if timeout <= 0 {
			return nil
		}
		p.scheduler.mu.Lock()
		p.scheduler.init()
		changed := p.scheduler.changed
		p.scheduler.mu.Unlock()
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-changed:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

// AllAccountsFullMatching 区分「本轮候选健康但槽位已满」与其它不可服务原因。
// 只有前者等待本地容量才有意义；已经尝试过的账号不再纳入等待判断。
func (p *Pool) AllAccountsFullMatching(tried map[string]bool, eligible map[string]bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	found := false
	for _, a := range p.accounts {
		if tried[a.Name] || eligible != nil && !eligible[a.Name] {
			continue
		}
		a.mu.Lock()
		if !a.disabled && !a.removed && now.After(a.coolUntil) && !p.reserveBlocked(a, now) {
			found = true
			if a.activeConcurrent < a.maxConcurrent {
				a.mu.Unlock()
				return false
			}
		}
		a.mu.Unlock()
	}
	return found
}
