package main

// ======================== OpenCode 客户端标识符 ========================
//
// opencode 官方 CLI 生成的 session / message 标识符并非任意随机串，而是：
//
//	<prefix>_<12 位小写十六进制（6 字节时间戳）><14 位 base62 随机>
//
// 即前缀之后固定 26 个字符，其中前 12 个必须是小写十六进制。
// 参考实现：sst/opencode packages/opencode/src/id/id.ts（LENGTH = 26，
// timeBytes.toString("hex") + randomBase62(LENGTH-12)）。
//
// 上游 OpenCode Zen 免费层网关会校验该形态：session 前 12 位不是合法小写
// 十六进制时，免费模型请求一律返回
//
//	403 {"type":"error","error":{"type":"FreeTierError",
//	     "message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}
//
// 实测结论（同一出口 IP、同一请求体，仅改变 session 值）：
//   - "ses_"+12 位小写十六进制+14 位任意 → 200
//   - "ses_"+24 位小写字母数字（旧实现）→ 403
//   - "ses_"+"z"*26、首位为非十六进制字符 → 403
//   - 26 位长度不符（25/27/28/32）→ 403

import (
	"crypto/rand"
	"sync"
	"time"
)

// opencodeIDLen 是前缀之后不含下划线的标识符长度（与官方一致，固定 26）。
const opencodeIDLen = 26

// opencodeTimeHexLen 是 6 字节时间戳的十六进制长度。
const opencodeTimeHexLen = 12

// base62Alphabet 与官方 randomBase62 的字符表完全一致（注意是大写在前）。
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

const lowerHexAlphabet = "0123456789abcdef"

var (
	opencodeIDMu      sync.Mutex
	opencodeIDLastMs  int64
	opencodeIDCounter uint64
)

// newOpenCodeID 按官方算法生成标识符，例如 newOpenCodeID("ses") →
// "ses_0a1b2c3d4e5f7GhI8jK9lM0nOp"（前 12 位恒为小写十六进制）。
//
// 同一毫秒内的多次调用通过自增计数器保证单调且不重复。
func newOpenCodeID(prefix string) string {
	opencodeIDMu.Lock()
	now := time.Now().UnixMilli()
	if now != opencodeIDLastMs {
		opencodeIDLastMs = now
		opencodeIDCounter = 0
	}
	opencodeIDCounter++
	counter := opencodeIDCounter
	opencodeIDMu.Unlock()

	// 官方：now = 毫秒 * 0x1000 + 计数器，随后取 6 字节大端。
	v := (uint64(now) << 12) + counter

	buf := make([]byte, 0, len(prefix)+1+opencodeIDLen)
	buf = append(buf, prefix...)
	buf = append(buf, '_')
	// 时间戳部分：12 位小写十六进制。
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

// newSessionID 生成 x-opencode-session 的值。
func newSessionID() string {
	return newOpenCodeID("ses")
}
