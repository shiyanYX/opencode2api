package main

// ======================== 形态漂移检测（shape drift guard） ========================
//
// 2026-10-25 实验定案：上游免费层把【形态被拒】伪装成 429 FreeUsageLimitError
// （与真额度耗尽不可区分），反复触发会升级为 IP 当日封禁。见 oc_shape_v2.go
// 文件头与 bun_transport.go 文件头。
//
// 对既有配额切换逻辑（main.go 的 classifyQuota 分支）的风险：
//   网关形态一旦漂移（上游又收紧闸门），每个出口都会返回 429。旧逻辑会把
//   这解释成「每个节点配额耗尽」——在 maxQuotaSwitches 预算内连环切换出口，
//   把坏形态请求散布到 N 个 IP 上。每个 IP 挨的坏请求都会累积到当日的
//   封禁阈值（实测 ~16 次量级），等于主动烧节点池。
//
// 本守卫的回答：**这个 429 是不是新出现的、全出口一致的现象？**
//   - 进程启动以来的第一次 FreeUsageLimitError：正常路径，交给既有配额
//     逻辑（真的可能只是这个出口额度耗尽）。
//   - 短窗口内 N 个【不同出口】都报 FreeUsageLimitError：形态漂移信号。
//     此时停掉配额切换（返回 true），让调用方按原错误返回，不再烧新出口；
//     同时打一条 warn 日志指认漂移。
//
// 判据刻意保守：只统计 FreeUsageLimitError 签名（真额度耗尽在多出口同时
// 出现是可能的——比如所有出口昨天就烧完了——但那种情况下切节点本来也无济
// 于事，停下来同样正确）。窗口和阈值做成 var 供测试调整。

import (
	"sync"
	"time"
)

var (
	shapeDriftMu      sync.Mutex
	shapeDriftSeen    = map[string]time.Time{} // egressKey -> 最近一次 FreeUsageLimitError
	shapeDriftWindow  = 10 * time.Minute       // 聚拢同批请求的观察窗口
	shapeDriftMinimum = 3                      // 窗口内不同出口报错的个数阈值
	shapeDriftArmed   = false                  // 触发后置位，直到窗口滑空
)

// shapeDriftShouldHalt 报告一次 FreeUsageLimitError 之后是否应停配额切换。
// egressKey 是报错出口的分桶键（EgressResult.EgressKey()），quotaReason 是
// classifyQuota 的签名命中说明（含 "type:FreeUsageLimitError" 时才算）。
// 返回 (halt, driftConfirmed)：
//
//	halt          —— 本次之后不要再切出口（按原错误返回）
//	driftConfirmed—— 已确认形态漂移（首条 warn 时机）
func shapeDriftShouldHalt(egressKey, quotaReason string, now time.Time) (halt, driftConfirmed bool) {
	// 只对 FreeUsageLimitError 签名计数；insufficient_quota / credits_error
	// 等 4xx 计费信号是 key 级问题，与形态无关，不参与。
	if !isFreeUsageLimitReason(quotaReason) {
		// 计费类配额信号同样停不下来没意义——但那属于 key 问题，
		// 既有逻辑对它切节点本来就是历史行为，不在本守卫职责内。
		return false, false
	}
	shapeDriftMu.Lock()
	defer shapeDriftMu.Unlock()

	// 清掉窗口外的旧记录。
	for k, ts := range shapeDriftSeen {
		if now.Sub(ts) > shapeDriftWindow {
			delete(shapeDriftSeen, k)
		}
	}
	if shapeDriftArmed && len(shapeDriftSeen) == 0 {
		// 窗口滑空且无新报错：解除，恢复正常配额切换。
		shapeDriftArmed = false
	}

	shapeDriftSeen[egressKey] = now
	if !shapeDriftArmed && len(shapeDriftSeen) >= shapeDriftMinimum {
		shapeDriftArmed = true
		return true, true
	}
	if shapeDriftArmed {
		return true, false
	}
	return false, false
}

// isFreeUsageLimitReason 报告 quotaReason 是否 FreeUsageLimitError 签名
// （type 或 keyword 命中皆算——上游两种错误体形态都出现过）。
func isFreeUsageLimitReason(quotaReason string) bool {
	return quotaReason == "type:FreeUsageLimitError" ||
		quotaReason == "keyword:free usage limit"
}

// shapeDriftState 返回当前守卫状态（管理面板/测试用）。
func shapeDriftState() (armed bool, distinctEgress int) {
	shapeDriftMu.Lock()
	defer shapeDriftMu.Unlock()
	return shapeDriftArmed, len(shapeDriftSeen)
}
