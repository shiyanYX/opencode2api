package main

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
)

// slimRec 只含磁盘侧聚合查询需要的 9 个字段——真正读它们的是 calllog.go 的
// forEachBucketedCallRecord / trendsFromCallLog / trendsByModelFromCallLog /
// modelStatsFromCallLog 及其回调。（写函数名而不是 file:line：行号会随上游
// 改动腐烂，之前注释里的 calllog.go:477/503/583 已经全部失准。）
// events / nodes / node_names / path / req_id /
// route_mode / duration_ms / err_msg / stream 从不被统计访问。
type slimRec struct {
	TS               string
	Model            string
	Status           string
	PromptTokens     int64
	CompletionTokens int64
	CacheCreation    int64
	CacheRead        int64
	TTFTMs           int64
	OutputSpeed      float64
}

// parseStatsLine 把一行 JSON 解析进 slimRec。
//
// 为什么不直接用 encoding/json：同机同数据（/tmp/prod_log.jsonl，100.4 MB，
// 186,212 行，-benchtime 10x）实测 encoding/json 2,009 ms，本函数 340 ms，
// **5.9 倍**。注意别再写成「9.3 倍」——1,628/331 = 4.9，本来就算错；
// 而且那两个数来自不同机器，倍率只有按同机数据报才有意义。
//
// 差额**主要来自分配，不是「只是反射」**：同一组数据下 encoding/json 每行新声明
// 一个 CallRecord，分配 4,562,270 次 / 202.1 MB；本函数 1,132,217 次 / 18.4 MB。
// 分配**次数**只差 4.0 倍，但分配的**字节数差 11.0 倍**——大头是每个字段值、每个
// 字符串的堆拷贝。所以 jsonReadKey 那条「键不构造 string」的路径是主要收益来源
// （改之前本函数是 3,526,334 次 / 44.1 MB，即 18.9 次/行 ≈ 键的个数）。
//
// 两条不可妥协的正确性约束：
//  1. 浮点必须交给 strconv.ParseFloat。手写 f=f*10+digit 的舍入方式与它不同，
//     实测会让 3.85% 的 output_speed 位模式不一致。
//  2. 时间戳原样保留给 time.Parse(RFC3339)，本文件不做任何时区判断。
//     生产日志混存 185,722 行 +08:00 与 490 行 Z，偏移符号在下标 23（下标 19 是小数点）；
//     误判会让 99.74% 的记录当 UTC，日桶整体偏 8 小时且不报错。
func parseStatsLine(line []byte, r *slimRec) bool {
	i := skipJSONSpace(line, 0)
	if i >= len(line) || line[i] != '{' {
		return false
	}
	i++
	*r = slimRec{}
	for {
		i = skipJSONSpace(line, i)
		if i >= len(line) {
			return false
		}
		switch line[i] {
		case '}':
			return true
		case ',':
			i++
			continue
		case '"':
		default:
			return false
		}

		key, ni := jsonReadKey(line, i)
		if ni < 0 {
			return false
		}
		i = skipJSONSpace(line, ni)
		if i >= len(line) || line[i] != ':' {
			return false
		}
		i = skipJSONSpace(line, i+1)
		if i >= len(line) {
			return false
		}

		if line[i] == '{' || line[i] == '[' {
			i = jsonSkipContainer(line, i)
			continue
		}
		if line[i] == '"' {
			s, ni := jsonReadString(line, i)
			if ni < 0 {
				return false
			}
			switch string(key) {
			case "ts":
				r.TS = s
			case "model":
				r.Model = s
			case "status":
				r.Status = s
			}
			i = ni
			continue
		}

		e := i
		for e < len(line) && line[e] != ',' && line[e] != '}' {
			e++
		}
		// 必须 TrimRight：合法 JSON 允许值与 ',' / '}' 之间有空白，
		// 而 strconv.ParseInt/ParseFloat 对 "42 " 直接报错 → 静默留 0。
		// 实测 {"prompt_tokens":42 } 会被读成 0，而 encoding/json 给 42。
		// 代价：行尾的制表/换行同样被吃掉，而那些已被 skipJSONSpace 处理。
		tok := bytes.TrimRight(line[i:e], " \t")
		switch string(key) {
		case "prompt_tokens":
			r.PromptTokens = jsonInt(tok)
		case "completion_tokens":
			r.CompletionTokens = jsonInt(tok)
		case "cache_creation_tokens":
			r.CacheCreation = jsonInt(tok)
		case "cache_read_tokens":
			r.CacheRead = jsonInt(tok)
		case "ttft_ms":
			r.TTFTMs = jsonInt(tok)
		case "output_speed":
			r.OutputSpeed = jsonFloat(tok)
		}
		i = e
	}
}

// jsonInt 解析一个 JSON 整数字段，失败一律返回 0。
//
// 判据是 err != nil，**不是**「值等于 MaxInt64 就丢」：ParseInt 对
// 9223372036854775807 返回 (MaxInt64, nil)，encoding/json 也照收，
// 按哨兵值过滤会把合法值误杀，同时放跑真正的越界输入。
//
// 为什么必须看 err：strconv 在 ErrRange 时返回**饱和值**而不是 0——
// ParseInt("99999999999999999999") 给 MaxInt64，ParseFloat("1e999") 给 +Inf。
// 把 err 丢掉等于把饱和值写进 slimRec，后果不是数字难看：下游桶聚合的
// avg += (v-avg)/n 一旦吃到 +Inf，该模型的均值就被**永久**污染且全程不报错。
// encoding/json 对同样的输入是报错 + 留零值；本函数为了「脏数值不中断整行」
// 选择返回 true，那就必须同样留零值，否则与对拍基准分叉。
//
// 语法错（"prompt_tokens":"abc"、null、true）走同一个分支留 0，这是既定宽容
// 行为：脏数值不中断整行，但不进统计。
func jsonInt(tok []byte) int64 {
	v, err := strconv.ParseInt(string(tok), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// jsonFloat 解析一个 JSON 浮点字段，失败一律返回 0。
//
// 必须委托 strconv：手写 f=f*10+digit 的舍入方式与它不同，实测会让 3.85% 的
// output_speed 位模式不一致。
//
// 光判 err 不够：ParseFloat 对 "Inf" / "+Inf" / "Infinity" / "NaN" 返回
// (Inf|NaN, **nil**)——这几个都不是合法 JSON，但 strconv 照单全收。
// 所以判据是 err == nil **且** 结果有限，两个条件缺一不可。
func jsonFloat(tok []byte) float64 {
	v, err := strconv.ParseFloat(string(tok), 64)
	if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0
	}
	return v
}

func skipJSONSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// jsonReadKey 读取一个 JSON 键（b[i] == '"'），返回**原始字节**与结束位置。
//
// 和 jsonReadString 唯一的区别是返回 []byte 而不是 string：键只用来 switch
// 匹配，从不需要留下来，构造 string 就是白拷一份。`switch string(key)` 会被
// 编译器识别成免分配比较（只比字节不建串）。
//
// 生产日志实测平均 12.86 个顶层键/行，每个键省一次 string 拷贝 ≈ 每行省 12.9 次分配。
// 编译器对**已是 string** 的值做 string(key) 不分配（老代码里那句写法本身没开销），
// 对 []byte 才需要这条专门的读取路径——这才是真正的大头。
//
// 遇到转义就退回 jsonReadString：原始字节不等于逻辑键（"t\u0073" 的逻辑键是
// "ts"）。键来自结构体 tag，生产日志里不会出现转义键，但保持正确不花代价。
func jsonReadKey(b []byte, i int) ([]byte, int) {
	start := i + 1 // 跳过起始引号
	for i = start; i < len(b); i++ {
		switch b[i] {
		case '\\':
			s, ni := jsonReadString(b, start-1)
			if ni < 0 {
				return nil, -1
			}
			return []byte(s), ni
		case '"':
			return b[start:i], i + 1
		}
	}
	return nil, -1
}

// jsonReadString 读取从 i 开始的 JSON 字符串（line[i] == '"'），返回内容与结束位置。
// 无转义时走单次拷贝快路径（内容是子切片，仍要 string() 一次）；
// 有转义时委托 strconv.Unquote 保证与 encoding/json 一致。
//
// 🔴 **兜底 json.Unmarshal 不是可选优化，是正确性的一部分。**
//
// 原因不在「扫描器该更宽容」，而在**参照系用错了**：strconv.Unquote 的
// 语法是 **Go 字符串字面量**，不是 JSON。它拒绝 \uD800-\uDFFF 是因为 Go 源码里
// 不允许裸代理字符，而**在 JSON 语境下成对代理恰恰合法**（encoding/json 照收，
// 解成 1 个 rune）。所以 Unquote 失败既可能是「真的非法 JSON」，也可能只是
// 「合法但非 Go 字面量」。两种情况都被压成 ("", -1)，parseStatsLine 随之返回
// false —— **整条合法记录从所有统计里静默消失**。实测 18 万行生产日志里
// 代理对出现 0 次，所以这个洞从未被数据触发过，只是一直在。
//
// 边界必须分开看，别混成一句「比 encoding/json 宽容」：
//   - **数值上刻意更宽**：饱和值归零（jsonInt/jsonFloat）、脏数值不中断整行，
//     这是 Task 5 有意为之的偏离。
//   - **字符串上「丢记录」方向已归零，但并非「正好等于」**。这条兜底修的是最严重的
//     失败模式——整条合法记录静默消失。400,000 条随机字节 fuzz 实测：修好 1,158 条、
//     弄坏 0 条、「我们拒收而 json 收」残余 0 条；`\uXXXX` 穷举 234,256 条零分歧。
//     但扫描器仍比 encoding/json **宽**，宽的两类都在**未改动**的代码路径上：
//     a) 无反斜杠快路径：裸控制字节 0x00-0x1F（JSON 禁止未转义出现）照收；
//     非法 UTF-8 原样透传而 json 折成 U+FFFD → 两侧都收但内容不同（222,368 条）
//     b) Go 专有转义 `\xNN` / `\OOO` / `\UXXXXXXXX` / `\a` / `\v` 被
//     strconv.Unquote 放行，json 拒收（6,668 条，`\xNN` 整类 256/256）
//     两类净方向都是「多收」而非「丢记录」。对本项目不可达——Go 的 json.Marshal
//     从不产出裸控制字节、非法 UTF-8（它自己换 U+FFFD）或上述 Go 专有转义，
//     只有日志被手改或由第三方写入时才可能触发。
//     孤立代理（\uD83d、\uDE00 单独出现）encoding/json 一律折成 U+FFFD，本函数现在也如此。
//
// 热路径代价为零：这条兜底只在 Unquote **已经失败**时才进。生产 186,212 行里
// 走转义慢路径的只有 2 次，且 json.Marshal 不转义非 ASCII，所以两次都是零收益
// 也不亏。全量日志里高代理（\uD800-\uDBFF）出现 0 次。
func jsonReadString(b []byte, i int) (string, int) {
	start := i
	i++
	escaped := false
	for i < len(b) {
		switch b[i] {
		case '\\':
			escaped = true
			i += 2
		case '"':
			i++
			if !escaped {
				return string(b[start+1 : i-1]), i
			}
			if s, err := strconv.Unquote(string(b[start:i])); err == nil {
				return s, i
			}
			// Unquote 说「不是 Go 字面量」。这**不等于**「不是合法 JSON 字符串」——
			// 代理对转义正是如此。改问真正的参照系。
			var s string
			if err := json.Unmarshal(b[start:i], &s); err == nil {
				return s, i
			}
			return "", -1
		default:
			i++
		}
	}
	return "", -1
}

// jsonSkipContainer 跳过 line[i] 处的 {...} 或 [...]。
//
// 必须跟踪字符串状态，且**字符串内必须再跟踪 escaped**，两个状态缺一不可：
//  1. 没有 inStr：字符串内的游离括号（"dial tcp [2406::1]:443"、"[DONE]"、
//     或错误消息里的 "]")会被当成结构符号，容器提前截断，后面需要取值的字段
//     整条从统计里消失且不报错。
//  2. 没有 escaped：`\"` 会提前关闭字符串，同上把字符串内容里的括号算进深度。
//
// 这两条都**测不出来**，因为生产数据恰好两边都踩空：
//   - 上面那些字符串内括号全部配对；
//   - 全量 186,212 行里有 73,282 个 `\"`（10,649 行，全在 events[].detail 的
//     错误消息里），但**没有任何一个**出现在同时含 {}[] 的字符串中——翻转字符串
//     状态也不会误算任何结构字符，所以深度照样算对。
//
// 换句话说，现在的正确性靠的是「数据碰巧不含游离括号」，不是靠这段代码。
// 能咬住这两条的用例见 statscan_test.go 的
// TestParseStatsLineEscapedQuoteInContainerSkip 与
// TestParseStatsLineStringAwareContainerSkip。
func jsonSkipContainer(b []byte, i int) int {
	depth := 0
	inStr := false
	escaped := false
	for i < len(b) {
		c := b[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			i++
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
		i++
		if depth == 0 {
			return i
		}
	}
	return i
}
