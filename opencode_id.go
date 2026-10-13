package main

// ======================== OpenCode 客户端标识符 ========================
//
// opencode 官方 CLI 生成的标识符布局（LENGTH = 26，参考 packages/opencode/src/id/id.ts）：
//
//	<prefix>_<6 字节时间分量的大端小写十六进制><14 位 base62 随机>
//
// 其中 6 字节来自 `now = 毫秒 * 0x1000 + 计数器`（同一毫秒内计数器自增）。
// **方向（direction）分两种，且官方对不同用途用不同方向**：
//
//	ascending : timeBytes = bytes(now)
//	descending: timeBytes = bytes(~now)      // BigInt 取反，按字节提取
//
// 2026-10-13 抓真实 CLI（1.18.33）确认：
//
//	session : ses_eca19c29dffe... -> 前缀与 descending(now) 同族（前缀 0xeca1…）
//	request : msg_135e63dc6001... -> 前缀与 ascending(now) 同族（前缀 0x135e…）
//
// 即 **session 必须用 descending，message/request 用 ascending**。
//
// 上游免费层会校验 session 的前 12 位，且校验的是「能否解回一个合理的时间」：
// 用 ascending 生成的 session 直接被拒（本网关旧实现正是如此，故全量 403）。
//
// 实测（真实 CLI 的 headers/body，仅替换 x-opencode-session）：
//
//	descending(now) 前缀 + 任意后缀        -> 200
//	descending(now±1 年) 前缀              -> 200
//	ascending(now) 前缀                    -> 403
//	随机 48 位前缀 / 全 0 前缀             -> 403
//	descending 但 counter >= 64            -> 403   （counter <= 48 通过）
//	12 位前缀之外的后 14 位内容完全自由     -> 200
//
// 旧实现对本项目造成的影响：session 一律 ascending，服务端收紧后免费模型
// 全量 403 FreeTierError（"…Download it at https://opencode.ai and keep building."）。

import (
	"crypto/rand"
	"sync"
	"time"
)

// opencodeIDLen 是前缀之后不含下划线的标识符长度（与官方一致，固定 26）。
const opencodeIDLen = 26

// opencodeTimeHexLen 是 6 字节时间分量的十六进制长度。
const opencodeTimeHexLen = 12

// opencodeTimeMask 是 6 字节时间分量的位掩码。
const opencodeTimeMask uint64 = (1 << 48) - 1

// maxSafeCounter 是同一毫秒内可用的最大计数器值。
// 实测 counter >= 64 会被上游拒绝（<= 48 通过），这里保守取 15；
// 超出则借用一个后续毫秒，保证生成的 session 始终落在被接受的范围。
const maxSafeCounter = 15

// base62Alphabet 与官方 randomBase62 的字符表完全一致（注意是大写在前）。
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

const lowerHexAlphabet = "0123456789abcdef"

var (
	opencodeIDMu      sync.Mutex
	opencodeIDLastMs  int64
	opencodeIDCounter uint64
)

// newOpenCodeID 生成 ascending 方向的标识符（官方用于 message / request / part）。
func newOpenCodeID(prefix string) string {
	return newOpenCodeIDDir(prefix, false)
}

// newSessionID 生成 x-opencode-session 的值。
// **必须是 descending 方向**，否则上游免费层返回 403 FreeTierError。
func newSessionID() string {
	return newOpenCodeIDDir("ses", true)
}

// newOpenCodeIDDir 按官方算法生成标识符。
// descending 为真时对时间分量取反（等价官方 BigInt 的 `~now`，再按字节提取，
// 即 48 位补码）。
func newOpenCodeIDDir(prefix string, descending bool) string {
	opencodeIDMu.Lock()
	now := time.Now().UnixMilli()
	if now != opencodeIDLastMs {
		opencodeIDLastMs = now
		opencodeIDCounter = 0
	}
	opencodeIDCounter++
	if opencodeIDCounter > maxSafeCounter {
		// 借用下一个毫秒，counter 归零重来：既保证不重复，也避开上游的 counter 上界。
		opencodeIDLastMs++
		now = opencodeIDLastMs
		opencodeIDCounter = 1
	}
	counter := opencodeIDCounter
	opencodeIDMu.Unlock()

	v := (uint64(now) << 12) + counter
	if descending {
		v = (^v) & opencodeTimeMask
	}

	buf := make([]byte, 0, len(prefix)+1+opencodeIDLen)
	buf = append(buf, prefix...)
	buf = append(buf, '_')
	// 时间分量：12 位小写十六进制（6 字节大端）。
	for i := 0; i < 6; i++ {
		b := byte(v >> (40 - 8*i))
		buf = append(buf, lowerHexAlphabet[b>>4], lowerHexAlphabet[b&0x0f])
	}
	// 随机部分：14 位 base62。
	randBytes := make([]byte, opencodeIDLen-opencodeTimeHexLen)
	if _, err := rand.Read(randBytes); err != nil {
		// crypto/rand 失败时退化为时间派生的确定性填充，保证仍是合法形态。
		for i := range randBytes {
			randBytes[i] = byte(v >> (uint(i) % 48))
		}
	}
	for _, rb := range randBytes {
		buf = append(buf, base62Alphabet[int(rb)%len(base62Alphabet)])
	}
	return string(buf)
}
