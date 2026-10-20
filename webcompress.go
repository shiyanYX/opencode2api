package main

// 面板内嵌 HTML 的 HTTP 压缩（task-27）。
//
// 为什么是「预压缩」而不是「每请求压缩」：
//   static/admin.html 是 go:embed 进来的**编译期常量**，内容永不变化。
//   gzip.NewWriter 每次要建压缩表（几十次分配、数十万字节），而面板请求虽少，
//   但每开一次面板就压一次纯属浪费；预压缩后每请求只剩一次 Write。
//
// 为什么只压 HTML、不压 JSON/SSE：
//   - /api/* 是 JSON，体积小且面板高频轮询，压缩收益不抵复杂度；
//   - /api/logs/stream 是 SSE，压缩会引入缓冲，破坏「逐条推送」的实时性。
//   本文件**只**服务 adminPageHandler 与 renderLoginPage 两个出口。
//
// 协商语义（见 acceptsGzip）：
//   - 客户端没声明 gzip ⇒ 明文，且**逐字节等于**内嵌资源（不许多一个空格）；
//   - 声明了 gzip;q=0 ⇒ 视为拒绝（显式拒绝优先于 * 通配）；
//   - 两种情况都必须带 Vary: Accept-Encoding，否则中间缓存会把 gzip 版本
//     喂给不支持的客户端。

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

var (
	webCompressOnce sync.Once

	// adminPageWeb 是 adminPageHTML 的「明文 + gzip」成对体，启动后只读。
	adminPageWeb *webPayload
	// adminPageGzip 单独留一份引用，便于测试与诊断直接取压缩体。
	adminPageGzip []byte

	// loginPageVariants 缓存「登录页 + 错误消息」的各种组合：
	// renderLoginPage 的 msg 取值只有空串与三条固定文案（main.go 的 4 个调用点），
	// 但这里按需生成而非穷举，避免新增文案时漏掉预压缩。
	loginPageMu       sync.Mutex
	loginPageVariants map[string]*webPayload
)

// webPayload 是一份「明文 + 预压缩」的成对资源。
// plain 用于不支持 gzip 的客户端，gz 用于支持的客户端；两者内容必须一致。
type webPayload struct {
	plain []byte
	gz    []byte
}

// ensureWebCompression 启动期压缩一次；并发安全，重复调用无副作用。
func ensureWebCompression() {
	webCompressOnce.Do(func() {
		adminPageGzip = gzipBytes(adminPageHTML)
		adminPageWeb = &webPayload{plain: adminPageHTML, gz: adminPageGzip}
		loginPageMu.Lock()
		loginPageVariants = make(map[string]*webPayload, 4)
		loginPageMu.Unlock()
	})
}

// gzipBytes 用最高压缩级别压一次。这是一次性成本（启动期/首次请求），
// 换来的是每个请求零压缩 CPU。最高级别对 191KB 文本只多花几毫秒。
func gzipBytes(plain []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(plain) / 3) // 面板实测约 3.16 倍，预留一半即够
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		// BestCompression 是合法常量，这里不可达；退化为不压缩而不是 panic，
		// 因为面板打不开比面板大一点严重得多。
		return nil
	}
	if _, err := zw.Write(plain); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

// prewarmWebCompression 在启动期把面板与登录页的 gzip 体算好，
// 让第一个请求也不必等压缩。压缩失败不致命（退化为明文）。
func prewarmWebCompression() {
	ensureWebCompression()
	// 登录页的 4 个已知变体（空 + 三条错误文案）一并预热。
	for _, msg := range []string{"", "表单解析失败", "密码错误", "创建会话失败"} {
		loginPagePayload(msg)
	}
}

// adminPagePayload 返回面板的预压缩体。首次调用（或 main 里的显式预热）
// 会触发一次性压缩，之后每请求零 CPU。
func adminPagePayload() *webPayload {
	ensureWebCompression()
	return adminPageWeb
}

// loginPagePayload 返回「登录页 + msg」这一变体的明文与预压缩体。
// 变体在首次请求时压缩一次并缓存；msg 的取值集合很小（见 main.go 调用点）。
func loginPagePayload(msg string) *webPayload {
	ensureWebCompression()

	loginPageMu.Lock()
	if p, ok := loginPageVariants[msg]; ok {
		loginPageMu.Unlock()
		return p
	}
	loginPageMu.Unlock()

	// 明文构造必须与旧实现**逐字节一致**：先写 loginPageHTML，
	// msg 非空时再追加那段内联 script。
	plain := loginPageHTML
	if msg != "" {
		plain = make([]byte, 0, len(loginPageHTML)+256)
		plain = append(plain, loginPageHTML...)
		plain = append(plain, "<script>document.addEventListener('DOMContentLoaded',function(){var m=document.getElementById('login-msg');if(m){m.textContent='"+msg+"';m.style.display='block'}})</script>"...)
	}
	p := &webPayload{plain: plain, gz: gzipBytes(plain)}

	loginPageMu.Lock()
	if existing, ok := loginPageVariants[msg]; ok { // 并发下别人先算完了
		loginPageMu.Unlock()
		return existing
	}
	loginPageVariants[msg] = p
	loginPageMu.Unlock()
	return p
}

// acceptsGzip 解析 Accept-Encoding。
//
// 规则（RFC 9110 §12.5.3 的实用子集）：
//   - 显式的 "gzip"（或 "x-gzip" 不做）带 q 值 ⇒ 以它为准；
//   - gzip;q=0 表示**拒绝**，且显式拒绝优先于 "*" 通配；
//   - 没有显式 gzip 时，"*" 且 q>0 表示接受 gzip；
//   - 空头/identity/br/deflate 一律视为不支持。
//
// 刻意不实现 deflate/br：面板只有 HTML，gzip 覆盖所有主流浏览器与 curl。
func acceptsGzip(acceptEncoding string) bool {
	if acceptEncoding == "" {
		return false
	}
	gzipExplicit := false
	gzipOK := false
	starOK := false
	for _, part := range strings.Split(acceptEncoding, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		coding := part
		q := 1.0
		if i := strings.IndexByte(part, ';'); i >= 0 {
			coding = strings.TrimSpace(part[:i])
			for _, param := range strings.Split(part[i+1:], ";") {
				param = strings.TrimSpace(param)
				if len(param) > 2 && (param[0] == 'q' || param[0] == 'Q') && param[1] == '=' {
					if v, err := strconv.ParseFloat(strings.TrimSpace(param[2:]), 64); err == nil {
						q = v
					}
				}
			}
		}
		switch strings.ToLower(coding) {
		case "gzip":
			gzipExplicit = true
			gzipOK = q > 0
		case "*":
			starOK = q > 0
		}
	}
	if gzipExplicit {
		return gzipOK
	}
	return starOK
}

// writeCompressedHTML 是面板与登录页的**唯一**写出口。
// 它负责三件事：内容协商、Content-Length、Vary。
func writeCompressedHTML(w http.ResponseWriter, r *http.Request, p *webPayload) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Vary 必须无条件设置（哪怕这次回的是明文）：否则共享缓存可能把
	// 压缩版本回给不支持的客户端。
	w.Header().Set("Vary", "Accept-Encoding")

	body := p.plain
	if p.gz != nil && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		w.Header().Set("Content-Encoding", "gzip")
		body = p.gz
	}
	// 显式 Content-Length：避免 chunked 分块，手机端首屏更快。
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}
