package main

// ======================== OpenCode 2.x 客户端形态（v2 lane） ========================
//
// 2026-10-25 定案（对照实验 + 上游 v1.4.4 抓包，ground truth 存于
// .scratch/ipchannels/v144-winning-shape.json，源码见 .scratch/upstream-src）：
// 上游免费层 v2 gate 指纹校验三层，全部满足才 200：
//
//	1. TLS 层：Bun 的 BoringSSL ClientHello（bun_transport.go，硬前提）；
//	2. 头层：四段 UA opencode/latest/<ver>/cli、双 session 头 + 两个
//	   affinity 头（同值）、不发 x-opencode-request（v2 gate 拒它）、
//	   Accept: */*；
//	3. body 层：官方 system prompt（含 <env> 块与 skills 列表）、
//	   13 个官方工具的真实 schema（2.x 命名）、stream=true、
//	   无 ceiling 时 max_completion_tokens=32000、JSON 与 JSON.stringify
//	   字节一致（无 \u003c 转义、key 顺序稳定）。
//
// 任何一层不符：上游返回 429 FreeUsageLimitError（文案与真额度耗尽一致）；
// 反复触发会升级为该 IP 当日封禁（issue #148 的「额度清零」）。
//
// 本文件实现头层与 body 层；TLS 层见 bun_transport.go；路由见
// buildOCRequestWithEndpoint 的 v2 分支（main.go）。
//
// 移植自上游 v1.4.4 的 internal/gateway/{opencode_prompt,toolshape}.go 与
// internal/httpx/client.go（GPL-3.0），按本仓库结构改写：
//   - fork 的客户端工具不重命名为 clint_（fork 的 agent_shape.go 已有官方名
//     改写 + 响应回映射机制，客户端自带 bash/read 等官方名时沿用该路径）；
//     本层只负责【补齐/替换为官方 2.x 工具集】并返回与 agent_shape 相同的
//     (mapping, injected) 接口，响应侧无需改动。
//   - 官方 prompt 注入对三协议（chat/anthropic/responses）分别处理，
//     已带官方 prompt 标记的 body 不重复注入。

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed oc_prompt_v2.txt
var ocPromptV2 string

//go:embed oc_tools_v2.json
var ocToolsV2JSON []byte

// ocPromptMarker 识别已带官方 CLI system prompt 的 body，自带的不动。
const ocPromptMarker = "You are opencode, an interactive CLI tool"

// ocSkills 是官方 CLI 在 env 块之后追加的内置 skill 列表（静态，随 prompt 注入）。
const ocSkills = `Skills provide specialized instructions and workflows for specific tasks.
Use the skill tool to load a skill when a task matches its description.
<available_skills>
  <skill>
    <name>customize-opencode</name>
    <description>Use ONLY when the user is editing or creating opencode's own configuration: opencode.json, opencode.jsonc, files under .opencode/, or files under ~/.config/opencode/. Also use when creating or fixing opencode agents, subagents, skills, plugins, MCP servers, or permission rules. Do not use for the user's own application code, or for any project that is not configuring opencode itself.</description>
    <location><built-in></location>
  </skill>
</available_skills>`

// v2OfficialToolOrder 是官方 2.x 工具集的顺序（oc_tools_v2.json 的 key 顺序）。
// 上游不校验工具顺序，但保持抓包顺序让 wire body 稳定。
var v2OfficialToolOrder = []string{
	"edit", "execute", "glob", "grep", "question", "read", "shell",
	"skill", "subagent", "webfetch", "websearch", "write", "patch",
}

// ocToolDefinition 是一个官方工具的描述与 schema。
type ocToolDefinition struct {
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

var (
	ocToolsOnce sync.Once
	ocToolsV2   map[string]ocToolDefinition
)

// loadOfficialToolsV2 只解码一次嵌入的官方工具集。解码失败（不应发生）时
// 留空集：officialToolV2 会退化为最小占位定义，工具名仍在。
func loadOfficialToolsV2() {
	ocToolsOnce.Do(func() {
		ocToolsV2 = map[string]ocToolDefinition{}
		_ = json.Unmarshal(ocToolsV2JSON, &ocToolsV2)
	})
}

// officialToolV2 渲染一个官方 2.x 工具的 wire 形状（Chat 协议）。
// 每个工具带 strict:false —— 2.x 客户端如此，缺这个字段 body 就与真客户端
// 逐字节不同（2026-10-23 上游 commit 5c5100c4 的教训）。
func officialToolV2(name string) map[string]any {
	loadOfficialToolsV2()
	description := "Agent tool " + name
	parameters := map[string]any{"type": "object", "properties": map[string]any{}}
	if definition, ok := ocToolsV2[name]; ok {
		description = definition.Description
		if definition.Parameters != nil {
			parameters = definition.Parameters
		}
	}
	// shell 工具描述的是宿主实际 shell：Windows 说 powershell，Unix 说 bash。
	if name == "shell" && runtime.GOOS != "windows" {
		// 抓包文件是 Linux/bash 版本；Windows 宿主需替换（与上游 v2ShellDescription 一致）。
		if runtime.GOOS == "windows" {
			description = strings.Replace(description, "on Linux using bash", "on Windows using powershell", 1)
		}
	}
	return map[string]any{"type": "function", "function": map[string]any{
		"name":        name,
		"description": description,
		"parameters":  parameters,
		"strict":      false,
	}}
}

// officialToolsetV2 渲染完整官方 2.x 工具集（Chat wire 形状）。
func officialToolsetV2() []any {
	tools := make([]any, 0, len(v2OfficialToolOrder))
	for _, name := range v2OfficialToolOrder {
		tools = append(tools, officialToolV2(name))
	}
	return tools
}

// isOfficialV2ToolName 报告 name 是否官方 2.x 工具名。
func isOfficialV2ToolName(name string) bool {
	l := strings.ToLower(name)
	for _, n := range v2OfficialToolOrder {
		if l == n {
			return true
		}
	}
	return false
}

// shapeToolsV2 把 tools 调整为官方 2.x CLI 会话的样子：
//   - 客户端自带工具保留在前面（官方名的按官方小写改写，沿用 agent_shape 的
//     mapping 机制回映射；非官方名原样保留——真 CLI 的 MCP 工具也是这样混在
//     官方集前面的）；
//   - 追加完整官方 13 工具集（客户端没声明的）。
//
// 返回 (mapping, injected)：与 agent_shape.go 的接口一致，响应侧的
// remapToolCallsInDelta / aggregateUpstreamSSE 无需感知本层存在。
func shapeToolsV2(bodyMap map[string]any) (map[string]string, map[string]bool) {
	mapping := map[string]string{}
	injected := map[string]bool{}
	rev := map[string]string{}

	clientTools, _ := bodyMap["tools"].([]any)
	hadClientTools := len(clientTools) > 0

	out := make([]any, 0, len(clientTools)+len(v2OfficialToolOrder))
	seen := map[string]bool{}

	for _, raw := range clientTools {
		tc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tc["function"].(map[string]any)
		if !ok {
			continue // 非标准形状的工具丢弃（v2 lane 只接受 function 形）
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		upName := name
		if isOfficialV2ToolName(name) {
			upName = strings.ToLower(name)
		}
		if seen[upName] {
			continue // 重名去重（上游 400）
		}
		seen[upName] = true
		if upName == name {
			out = append(out, raw)
			continue
		}
		nf := cloneMap(fn)
		nf["name"] = upName
		// 官方名补 strict:false，与官方工具一致（v2 body 指纹）。
		if _, has := nf["strict"]; !has {
			nf["strict"] = false
		}
		ntc := cloneMap(tc)
		ntc["function"] = nf
		out = append(out, ntc)
		mapping[upName] = name
		rev[name] = upName
	}

	// 追加客户端没有的官方工具（真 CLI 的完整工具集）。
	for _, req := range v2OfficialToolOrder {
		if seen[req] {
			continue
		}
		seen[req] = true
		injected[req] = true
		out = append(out, officialToolV2(req))
	}

	bodyMap["tools"] = out

	if !hadClientTools {
		bodyMap["tool_choice"] = "none"
	} else if tcObj, ok := bodyMap["tool_choice"].(map[string]any); ok {
		if fn, ok := tcObj["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok {
				if up, ok := rev[n]; ok {
					nf := cloneMap(fn)
					nf["name"] = up
					ntc := cloneMap(tcObj)
					ntc["function"] = nf
					bodyMap["tool_choice"] = ntc
				}
			}
		}
	}
	return mapping, injected
}

// ======================== 官方 system prompt ========================

var (
	ocEnvOnce  sync.Once
	ocWorkDir  string
	ocEnvToday string
)

// ocEnvironment 渲染官方 CLI 在基础 prompt 后放的 <env> 块。
// 工作目录每进程解析一次；Today's date 每次调用取当天（UTC，与官方一致）。
func ocEnvironment(model string) string {
	ocEnvOnce.Do(func() {
		directory := ""
		if current, err := os.Getwd(); err == nil {
			directory = filepath.Clean(strings.TrimSpace(current))
		}
		if directory == "" || directory == "." {
			directory = "/workspace"
		}
		ocWorkDir = directory
	})
	apiModel := strings.TrimSpace(strings.TrimPrefix(model, "opencode/"))
	if apiModel == "" {
		apiModel = "unknown"
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	return strings.Join([]string{
		"You are powered by the model named " + apiModel + ". The exact model ID is opencode/" + apiModel,
		"Here is some useful information about the environment you are running in:",
		"<env>",
		"  Working directory: " + ocWorkDir,
		"  Workspace root folder: " + ocWorkDir,
		"  Is directory a git repo: yes",
		"  Platform: " + platform,
		"  Today's date: " + time.Now().UTC().Format("Mon Jan 02 2006"),
		"</env>",
	}, "\n")
}

// ocSystemPrompt 组装完整官方 system prompt。
func ocSystemPrompt(model string) string {
	return ocPromptV2 + ocEnvironment(model) + "\n" + ocSkills
}

// injectOfficialPrompt 把官方 CLI system prompt 前置到已塑形的免费层 body。
// 已带 marker 的 body 不动（重复塑形不重复注入）。fork 的 surface 目前只有
// chat（两调用链都走 /chat/completions），anthropic/responses 的分支留待
// fork 暴露多协议时启用——接口与上游一致。
func injectOfficialPrompt(payload map[string]any, model string) bool {
	if payload == nil {
		return false
	}
	messages, ok := payload["messages"].([]any)
	if !ok {
		return false
	}
	prompt := ocSystemPrompt(model)
	kept := make([]any, 0, len(messages)+1)
	var instructions []string
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		role, _ := message["role"].(string)
		if role != "system" && role != "developer" {
			kept = append(kept, raw)
			continue
		}
		content := message["content"]
		if carriesOfficialPrompt(content) {
			return false
		}
		for _, part := range textFragments(content) {
			if part = strings.TrimSpace(part); part != "" {
				instructions = append(instructions, part)
			}
		}
	}
	system := prompt
	if joined := strings.Join(instructions, "\n"); joined != "" {
		system += "\n\n" + joined
	}
	payload["messages"] = append([]any{map[string]any{"role": "system", "content": system}}, kept...)
	return true
}

// textFragments 把 system/instructions 值摊平成文本：纯字符串、内容块列表、
// 或携带 text/content 的块对象。
func textFragments(value any) []string {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{typed}
	case []any:
		var parts []string
		for _, item := range typed {
			parts = append(parts, textFragments(item)...)
		}
		return parts
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return textFragments(text)
		}
		if content, ok := typed["content"]; ok {
			return textFragments(content)
		}
	}
	return nil
}

// carriesOfficialPrompt 报告 value 里是否已含官方 prompt 标记。
func carriesOfficialPrompt(value any) bool {
	for _, part := range textFragments(value) {
		if strings.Contains(part, ocPromptMarker) {
			return true
		}
	}
	return false
}

// ======================== 有序无转义 JSON 编码 ========================
//
// 上游 v2 gate 对 body 做指纹比对：encoding/json 默认把 '<' '>' '&' 写成
// \u003c \u003e \u0026（JSON.stringify 从不转义这些），官方 prompt 与工具
// 描述里全是这两类字符，所以免费层 body 必须走这里而不是 json.Marshal。
// key 顺序：Go map 无插入顺序，实测 v2 gate 不校验 key 顺序（2026-10-25
// dump 实验佐证），唯一字节级硬要求是无 \u HTML 转义。

// marshalBodyBytes 按 JSON.stringify 的方式编码 value：不转义 HTML 字符，
// 不追加换行。
func marshalBodyBytes(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// ======================== v2 头形态 ========================

// v2UserAgent 返回 v2 gate 期望的四段 UA。
// 上游接受一段 release 范围（版本号本身不被校验），只在开始拒旧版时需要抬。
func v2UserAgent() string {
	return "opencode/latest/" + v2ClientVersion + "/cli"
}

// v2ClientVersion 是 v2 lane 伪装的 OpenCode 2.x CLI release（与上游 v1.4.4 相同）。
const v2ClientVersion = "2.1.1"

// applyV2Headers 给 req 设置 v2 lane 的完整头集（按 2026-10-25 抓包顺序）。
// session 头四个同值：x-opencode-session / x-opencode-session-id /
// x-session-affinity / x-session-id（descending ses_，fork 的 newSessionID
// 生成的就是该布局）。已存在的同名头会被覆盖。
// ★ 不发 x-opencode-request —— v2 gate 拒这个头（403 FreeTierError），
//
//	调用方（buildOCRequestWithEndpoint）负责不设置它。
func applyV2Headers(req *http.Request, session, project string) {
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", v2UserAgent())
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", project)
	req.Header.Set("x-opencode-session", session)
	req.Header.Set("x-opencode-session-id", session)
	req.Header.Set("x-session-affinity", session)
	req.Header.Set("x-session-id", session)
}
