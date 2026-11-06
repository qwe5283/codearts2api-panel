package server

import (
	"context"
	"strings"
	"time"

	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

const localSlotWait = 10 * time.Second

// acquireConversationSlot 把软粘性解析、容量分配和原子占槽放在一起。
// 同会话的分配与重绑串行；不同会话可并行，不预留永久并发名额。
func (h *Handler) acquireConversationSlot(ctx context.Context, key string, persistent bool, wait time.Duration, tried map[string]bool) *pool.Account {
	if !persistent || key == "" {
		return h.cfg.Pool.WaitScheduled(ctx, wait, h.live().AccountStrategy, "", h.bindingSnapshot(), tried, nil)
	}
	select {
	case h.convAssign <- struct{}{}:
	case <-ctx.Done():
		return nil
	}
	defer func() { <-h.convAssign }()

	now := time.Now()
	ttl := h.live().SessionTTL
	if ttl <= 0 {
		ttl = h.cfg.SessionTTL
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	h.convMu.Lock()
	previous := h.convAcct[key]
	h.convMu.Unlock()
	bound := h.bindingSnapshotBefore(now.Add(-ttl))
	// 刚因 TTL 清理的旧账号不是“从未绑定的账号”，不能又被优先选回。
	if previous != "" {
		bound[previous] = true
	}
	h.convMu.Lock()
	sticky := h.convAcct[key]
	h.convMu.Unlock()
	acct := h.cfg.Pool.WaitScheduled(ctx, wait, h.live().AccountStrategy, sticky, bound, tried, nil)
	if acct == nil {
		if sticky != "" {
			h.clearConversationBinding(key, sticky)
		}
		return nil
	}
	if wait == 0 {
		h.convMu.Lock()
		h.convAcct[key] = acct.Name // 暂定；成功响应时由 storeChat 持久化
		h.convSeen[key] = time.Now()
		h.convMu.Unlock()
	}
	return acct
}

// bindingSnapshot 返回未过期的绑定集合，同时惰性清理 TTL 到期项。
func (h *Handler) bindingSnapshotBefore(cutoff time.Time) map[string]bool {
	h.convMu.Lock()
	defer h.convMu.Unlock()
	for conversation, seen := range h.convSeen {
		if !seen.After(cutoff) {
			delete(h.convSeen, conversation)
			delete(h.convAcct, conversation)
		}
	}
	bound := make(map[string]bool, len(h.convAcct))
	for _, uid := range h.convAcct {
		bound[uid] = true
	}
	return bound
}

// acquireAuxiliarySlot 识图子请求共用容量/保留积分规则，但不创建会话绑定。
// model 非空时只在目录声明支持该模型的账号中分配；pickAccount 保留懒目录发现。
func (h *Handler) acquireAuxiliarySlot(ctx context.Context, wait time.Duration, model string) *pool.Account {
	var eligible map[string]bool
	if model != "" {
		_ = h.pickAccount(map[string]bool{}, model)
		eligible = map[string]bool{}
		want := strings.ToLower(upstream.CanonicalModel(model))
		for _, acct := range h.cfg.Pool.Accounts() {
			if models, known := upstream.AccountModels(acct.UID); known && catalogHas(models, want) {
				eligible[acct.Name] = true
			}
		}
		if len(eligible) == 0 {
			return nil
		}
	}
	return h.cfg.Pool.WaitScheduled(ctx, wait, h.live().AccountStrategy, "", h.bindingSnapshot(), nil, eligible)
}

func (h *Handler) bindingSnapshot() map[string]bool {
	h.convMu.Lock()
	defer h.convMu.Unlock()
	bound := make(map[string]bool, len(h.convAcct))
	for _, uid := range h.convAcct {
		bound[uid] = true
	}
	return bound
}

func (h *Handler) clearConversationBinding(key, expected string) {
	if key == "" {
		return
	}
	removed := false
	h.convMu.Lock()
	if expected == "" || h.convAcct[key] == expected {
		_, removed = h.convAcct[key]
		delete(h.convAcct, key)
		delete(h.convSeen, key)
	}
	h.convMu.Unlock()
	if removed {
		h.saveChats()
	}
}

func (h *Handler) pruneConversationBindings() int {
	ttl := h.live().SessionTTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	h.bindingSnapshotBefore(time.Now().Add(-ttl))
	h.convMu.Lock()
	defer h.convMu.Unlock()
	return len(h.convAcct)
}
