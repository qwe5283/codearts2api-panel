package server

import (
	"strings"
	"sync"
	"time"
)

// 模型可用性学习。
//
// 背景（实测）：上游 /v1/models 的发现结果 ≠ 账号真能调用的模型。
// agent-center 与 /v1/model/builtin 会列出未在当前账号注册的模型（实测
// GLM-5.2-ArkTS-SPARK / OpenPangu-2.0-Pro / OpenPangu-2.0-Flash 全部返回
// InferHub.002002009.404）。
//
// 这里**不主动探测**：真实 chat 流量本身就会写结论（成功 markUsable / 失败
// markUnusable），空闲时不再额外打上游。因此某模型在被真实调用一次之前可能是
// 「未知」——此时 /v1/models 照列、聊天照放行，交给上游作答；一旦真实请求报
// not registered 即标记不可用并从列表下架。代价是「从未被调用过的僵尸模型」会
// 短暂出现在列表里，收益是网关在空闲时对上游零额外请求。

const (
	// availabilityTTL 结论有效期。过了就重新视为未知、交给真实流量再判定，
	// 避免套餐变化后一直按旧结论办事。
	availabilityTTL = 30 * time.Minute
)

type availState int

const (
	availUnknown availState = iota
	availUsable
	availUnusable
)

type availEntry struct {
	state  availState
	reason string
	at     time.Time
}

var availability = struct {
	sync.Mutex
	byKey map[string]availEntry // accountID|modelLower -> 状态
}{byKey: map[string]availEntry{}}

func availKey(accountID, model string) string {
	return accountID + "|" + strings.ToLower(model)
}

// modelAvailability 返回缓存的可用性；过期视为未知。
func modelAvailability(accountID, model string) (availState, string) {
	availability.Lock()
	defer availability.Unlock()
	e, ok := availability.byKey[availKey(accountID, model)]
	if !ok || time.Since(e.at) > availabilityTTL {
		return availUnknown, ""
	}
	return e.state, e.reason
}

// markUnusable 记录某账号下某模型不可用（来自真实请求失败）。
func markUnusable(accountID, model, reason string) {
	if accountID == "" || model == "" {
		return
	}
	availability.Lock()
	defer availability.Unlock()
	availability.byKey[availKey(accountID, model)] = availEntry{state: availUnusable, reason: reason, at: time.Now()}
}

// markUsable 记录某账号下某模型可用。
func markUsable(accountID, model string) {
	if accountID == "" || model == "" {
		return
	}
	availability.Lock()
	defer availability.Unlock()
	availability.byKey[availKey(accountID, model)] = availEntry{state: availUsable, at: time.Now()}
}

// modelKnownUnusable 报告该模型是否在**所有**健康账号上都被判定为不可用。
// 只要有一个账号没结论或可用，就返回 false（列表保留该模型）。
func (h *Handler) modelKnownUnusable(model string) bool {
	anyKnown := false
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		st, _ := modelAvailability(acct.UID, model)
		switch st {
		case availUnknown:
			return false // 还没结论，别急着下架
		case availUsable:
			return false
		}
		anyKnown = true
	}
	return anyKnown
}
