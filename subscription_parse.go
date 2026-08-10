package main

import (
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
)

// ======================= 单节点 URI 解析 =======================

func defaultPortFor(scheme string) int {
	switch scheme {
	case "vless", "anytls", "trojan":
		return 443
	case "hysteria2", "hy2":
		return 443
	case "ss":
		return 8388
	default:
		return 1080
	}
}

// qBool 解析 "1"/"true"/"yes" 为 true。
func qBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// parseNodeURI 解析单个 vless:// ss:// hysteria2:// anytls:// socks5:// 链接。
// 返回 nil 表示不支持（调用方记录 warning）。
func parseNodeURI(uri string) *ProxyNode {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme == "" {
		return nil
	}
	scheme := strings.ToLower(u.Scheme)
	if _, ok := map[string]bool{"vless": true, "ss": true, "hysteria2": true, "hy2": true, "anytls": true, "socks5": true, "socks": true}[scheme]; !ok {
		return nil
	}
	name := strings.TrimSpace(u.Fragment)
	if name == "" {
		name = u.Hostname()
	}
	host := u.Hostname()
	port := defaultPortFor(scheme)
	if p := u.Port(); p != "" {
		if v, err := strconv.Atoi(p); err == nil {
			port = v
		}
	}
	q := u.Query()
	getQ := func(keys ...string) string {
		for _, k := range keys {
			if v := q.Get(k); v != "" {
				return v
			}
		}
		return ""
	}
	inscureParam := getQ("allowInsecure", "insecure", "skip-cert-verify")

	switch scheme {
	case "vless":
		if u.User == nil {
			return nil
		}
		n := &ProxyNode{
			Name: name, Protocol: "vless", Address: host, Port: port,
			UserID:   u.User.Username(),
			Flow:     getQ("flow"),
			Insecure: qBool(inscureParam),
		}
		n.SNI = getQ("sni", "serverName", "servername")
		switch getQ("security") {
		case "reality":
			n.Reality = &RealityConfig{
				PublicKey:   getQ("pbk", "publickey"),
				ShortID:     getQ("sid"),
				SpiderX:     getQ("spx"),
				Fingerprint: getQ("fp"),
			}
		case "tls":
			// SNI 已在上面赋值
		}
		switch getQ("type") {
		case "", "tcp":
			return n
		default:
			return nil // ws/grpc/httpupgrade 等暂不支持
		}
	case "ss":
		if u.User == nil {
			return nil
		}
		ssPw, hasPw := u.User.Password()
		method, password, ok := decodeSSUserInfo(u.User.Username(), ssPw, hasPw)
		if !ok {
			return nil
		}
		if getQ("plugin") != "" {
			return nil // obfs/simple-obfs/v2ray-plugin 不支持
		}
		return &ProxyNode{
			Name: name, Protocol: "ss", Address: host, Port: port,
			Method: method, Password: password,
		}
	case "hysteria2", "hy2":
		password := ""
		if u.User != nil {
			password = u.User.Username()
		}
		if password == "" {
			password = getQ("auth", "password")
		}
		return &ProxyNode{
			Name: name, Protocol: "hy2", Address: host, Port: port,
			Password: password, Insecure: qBool(inscureParam),
			SNI: getQ("sni", "peer"),
		}
	case "anytls":
		if u.User == nil {
			return nil
		}
		return &ProxyNode{
			Name: name, Protocol: "anytls", Address: host, Port: port,
			Password: u.User.Username(), Insecure: qBool(inscureParam),
			SNI: getQ("sni", "serverName"),
		}
	case "socks5", "socks":
		n := &ProxyNode{
			Name: name, Protocol: "socks5", Address: host, Port: port,
		}
		if u.User != nil {
			n.UserID = u.User.Username()
			n.Password, _ = u.User.Password()
		}
		return n
	}
	return nil
}

// decodeSSUserInfo 解析 ss:// 的用户信息部分。
// userinfo 可能是 base64(method:password)、URL 编码的 method:password，
// 或（被 url.Parse 拆开后）明文的 method / password 两部分。
func decodeSSUserInfo(userinfo, password string, hasPassword bool) (string, string, bool) {
	if userinfo == "" {
		return "", "", false
	}
	if method, pw, ok := splitMethodPassword(userinfo); ok {
		return method, pw, true
	}
	if hasPassword && !strings.ContainsAny(userinfo, "%") {
		// 明文 "method:password@host" 被 url.Parse 拆成 username/password
		return userinfo, password, true
	}
	if dec, err := base64DecodeLoose(userinfo); err == nil {
		if method, pw, ok := splitMethodPassword(string(dec)); ok {
			return method, pw, true
		}
	}
	return "", "", false
}

func splitMethodPassword(s string) (string, string, bool) {
	i := strings.Index(s, ":")
	if i <= 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// ======================= base64 工具 =======================

// base64DecodeLoose 尝试多种 base64 变体解码。
func base64DecodeLoose(s string) ([]byte, error) {
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	s = strings.TrimSpace(s)
	for _, e := range encodings {
		if b, err := e.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, base64.CorruptInputError(0)
}
