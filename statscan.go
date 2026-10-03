package main

import "strconv"

// slimRec 只含磁盘侧聚合查询需要的 9 个字段（calllog.go:477/503/583 三个函数的
// 回调只访问这些）。events / nodes / node_names / path / req_id /
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
// 为什么不直接用 encoding/json：实测 100.4 MB 全量解析 1,628 ms，
// 而本函数 331 ms（9.3 倍）。差额来自 encoding/json 的逐 token 反射，
// 不是分配——换「瘦身结构体」只能快 1.4 倍，仍不够。
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

		key, ni := jsonReadString(line, i)
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
		tok := line[i:e]
		switch string(key) {
		case "prompt_tokens":
			r.PromptTokens, _ = strconv.ParseInt(string(tok), 10, 64)
		case "completion_tokens":
			r.CompletionTokens, _ = strconv.ParseInt(string(tok), 10, 64)
		case "cache_creation_tokens":
			r.CacheCreation, _ = strconv.ParseInt(string(tok), 10, 64)
		case "cache_read_tokens":
			r.CacheRead, _ = strconv.ParseInt(string(tok), 10, 64)
		case "ttft_ms":
			r.TTFTMs, _ = strconv.ParseInt(string(tok), 10, 64)
		case "output_speed":
			// 必须委托 strconv：手写舍入会让 3.85% 的值位模式不同。
			r.OutputSpeed, _ = strconv.ParseFloat(string(tok), 64)
		}
		i = e
	}
}

func skipJSONSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// jsonReadString 读取从 i 开始的 JSON 字符串（line[i] == '"'），返回内容与结束位置。
// 无转义时走零拷贝快路径；有转义时委托 strconv.Unquote 保证与 encoding/json 一致。
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
			return "", -1
		default:
			i++
		}
	}
	return "", -1
}

// jsonSkipContainer 跳过 line[i] 处的 {...} 或 [...]。
//
// 必须跟踪字符串状态：朴素的括号深度在遇到字符串内不成对的括号时会损坏整行。
// 实测生产数据里的 "dial tcp [2406::1]:443" 和 "[DONE]" 都是配对的，侥幸安全；
// 但任何一条含游离 ] 的错误消息都会让该记录的 token 从统计里凭空消失且不报错。
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
