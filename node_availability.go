package main

import "time"

const (
	availabilityOK          = "ok"
	availabilityQuota       = "quota"
	availabilityCooling     = "cooling"
	availabilityDead        = "dead"
	availabilityUnreachable = "unreachable"
	availabilityDenied      = "denied"
	availabilityUntested    = "untested"
)

// nodeAvailability 是面板唯一的可用性判定入口。v5 状态优先，旧池状态只在
// v5 缺失（off/shadow）且没有可用的探测结论时兜底。
func nodeAvailability(oldState string, v *v5NodeView, p *probeResult, now time.Time) (string, string) {
	nowMS := now.UnixMilli()
	if v != nil {
		if v.Lifecycle == "dead" {
			// DeadUntil==0 是迁移/手动标记留下的无期限停用；有期限的停用在
			// 30 分钟冷却到期后回到「可尝试」，此时以最新探测为准。
			if v.DeadUntil == 0 || v.DeadUntil > nowMS {
				return availabilityDead, "节点已停用"
			}
			if a, r := probeAvailability(p); a != "" {
				return a, r
			}
			return availabilityUntested, "冷却结束待复探"
		}
		if v.QuotaUntil != nil && *v.QuotaUntil > nowMS {
			return availabilityQuota, "配额冷却中"
		}
		if v.PairUntil > nowMS {
			return availabilityCooling, "pair 冷却中"
		}
		if v.Gate != nil && v.Gate.Active {
			return availabilityCooling, "全池熔断中"
		}
		if a, r := probeAvailability(p); a != "" {
			return a, r
		}
		return availabilityUntested, "暂无探测数据"
	}

	if a, r := probeAvailability(p); a != "" {
		return a, r
	}
	switch oldState {
	case "available":
		return availabilityOK, "旧状态可用"
	case "exhausted":
		return availabilityCooling, "旧状态耗尽"
	case "dead":
		return availabilityDead, "旧状态停用"
	default:
		return availabilityUntested, "暂无探测数据"
	}
}

func probeAvailability(p *probeResult) (string, string) {
	if p == nil || p.Class == "" {
		return "", ""
	}
	switch p.Class {
	case probeClassOK:
		return availabilityOK, "最近探测可用"
	case probeClassDenied:
		return availabilityDenied, "最近探测 403"
	case probeClassUnreachable, "timeout":
		return availabilityUnreachable, "最近探测不可达"
	case probeClassError:
		return availabilityUnreachable, "最近探测 5xx"
	case probeClassQuota, probeClassQuotaUnknown:
		return availabilityQuota, "最近探测配额 429"
	default:
		return "", ""
	}
}
