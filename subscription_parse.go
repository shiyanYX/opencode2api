package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ======================= 单节点 URI 解析 =======================

func defaultPortFor(scheme string) int {
	switch scheme {
	case "vless", "vmess", "trojan", "anytls":
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

// parseNodeURI 解析单个 vless:// ss:// hysteria2:// trojan:// vmess:// anytls:// socks5:// 链接。
// 返回 nil 表示不支持（调用方记录 warning）。
func parseNodeURI(uri string) *ProxyNode {
	if strings.HasPrefix(strings.ToLower(uri), "vmess://") {
		return parseVmessURI(uri)
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme == "" {
		return nil
	}
	scheme := strings.ToLower(u.Scheme)
	if _, ok := map[string]bool{"vless": true, "ss": true, "hysteria2": true, "hy2": true, "anytls": true, "socks5": true, "socks": true, "trojan": true}[scheme]; !ok {
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
		case "tls", "":
			n.TLS = true
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
	case "trojan":
		if u.User == nil || u.User.Username() == "" {
			return nil
		}
		n := &ProxyNode{
			Name: name, Protocol: "trojan", Address: host, Port: port,
			Password: u.User.Username(), Insecure: qBool(inscureParam),
			Network: strings.ToLower(getQ("type")), Path: getQ("path"), Host: getQ("host"),
		}
		n.SNI = getQ("sni", "peer", "serverName")
		if n.Network == "" {
			n.Network = "tcp"
		}
		if strings.EqualFold(getQ("security"), "tls") || getQ("security") == "" {
			n.TLS = true
			if fp, pbk, sid := getQ("fp"), getQ("pbk"), getQ("sid"); pbk != "" && fp != "" {
				n.Reality = &RealityConfig{
					PublicKey: pbk, ShortID: sid, Fingerprint: fp,
				}
			}
		}
		return n
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

// base64DecodeLoose 尝试多种 base64 变体解码，容错对齐 mihomo/v2rayN：
// 去换行与空白、URL-safe 变体（-_ → /+）归一、自动补 padding，四引擎递进。
func base64DecodeLoose(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer("\r", "", "\n", "", "\t", "", " ", "", "_", "/", "-", "+").Replace(s)
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, e := range encodings {
		if b, err := e.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, base64.CorruptInputError(0)
}

// isInfoPseudoNode 判断节点名是否为机场插播的公告/信息伪节点
// （剩余流量、套餐到期、官网等，多为全角冒号句式）。
func isInfoPseudoNode(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	lower := strings.ToLower(name)
	if strings.Contains(lower, "：") {
		return true
	}
	for _, kw := range []string{
		"剩余流量", "已用流量", "套餐到期", "到期时间", "重置时间", "距离下次重置", "重置剩余",
		"官网", "官方网址", "官方网站", "发布页", "更新于", "更新时间", "公告", "温馨提示", "通知",
		"usage", "traffic", "expire", "expiry", "reset", "official", "website", "announcement",
	} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// parseVmessURI 解析 vmess:// 链接（body 为 base64 编码的 v2rayN 风格 JSON）。
func parseVmessURI(uri string) *ProxyNode {
	body := uri[len("vmess://"):]
	if i := strings.IndexAny(body, "\r\n#"); i >= 0 {
		body = body[:i]
	}
	if body == "" {
		return nil
	}
	var raw []byte
	if dec, err := base64DecodeLoose(body); err == nil {
		raw = dec
	} else {
		raw = []byte(body)
	}
	var vm struct {
		PS   string `json:"ps"`
		Add  string `json:"add"`
		Port string `json:"port"`
		ID   string `json:"id"`
		AID  string `json:"aid"`
		SCY  string `json:"scy"`
		Net  string `json:"net"`
		Type string `json:"type"`
		Host string `json:"host"`
		Path string `json:"path"`
		TLS  string `json:"tls"`
		SNI  string `json:"sni"`
		FP   string `json:"fp"`
	}
	if err := json.Unmarshal(raw, &vm); err != nil {
		return nil
	}
	if vm.Add == "" || vm.ID == "" {
		return nil
	}
	n := &ProxyNode{
		Name:     vm.PS,
		Protocol: "vmess",
		Address:  vm.Add,
		UserID:   vm.ID,
		Network:  strings.ToLower(vm.Net),
		Host:     vm.Host,
		Path:     vm.Path,
		Security: vm.SCY,
	}
	if vm.PS == "" {
		n.Name = vm.Add
	}
	if p, err := strconv.Atoi(vm.Port); err == nil && p > 0 {
		n.Port = p
	} else {
		n.Port = defaultPortFor("vmess")
	}
	if v, err := strconv.Atoi(vm.AID); err == nil && v > 0 {
		n.AlterIDs = uint16(v)
	}
	if n.Network == "" {
		n.Network = "tcp"
	}
	if strings.EqualFold(vm.TLS, "tls") || strings.EqualFold(vm.TLS, "reality") {
		n.TLS = true
	}
	if vm.SNI != "" {
		n.SNI = vm.SNI
	} else if n.TLS && vm.Host != "" {
		n.SNI = vm.Host
	}
	return n
}

// uniqueNodeNames 对重名节点追加 -02/-03 后缀（mihomo uniqueName 语义），
// 保证面板与路由中节点名唯一。
func uniqueNodeNames(nodes []*ProxyNode) []*ProxyNode {
	counts := map[string]int{}
	for _, n := range nodes {
		counts[n.Name]++
	}
	seen := map[string]int{}
	for _, n := range nodes {
		if counts[n.Name] <= 1 {
			continue
		}
		seen[n.Name]++
		if seen[n.Name] > 1 {
			n.Name = fmt.Sprintf("%s-%02d", n.Name, seen[n.Name])
		}
	}
	return nodes
}
