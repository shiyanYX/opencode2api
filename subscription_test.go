package main

import (
	"encoding/base64"
	"testing"
)

func TestParseVlessRealityLink(t *testing.T) {
	uri := "vless://9b2c3d4e-5f6a-4b7c-8d9e-0f1a2b3c4d5e@1.2.3.4:443?encryption=none&security=reality&sni=www.microsoft.com&fp=chrome&pbk=AbCdeFg12345&sid=abc123&spx=%2F&flow=xtls-rprx-vision&type=tcp#US-REALITY-01"
	n := parseNodeURI(uri)
	if n == nil {
		t.Fatal("parse failed")
	}
	if n.Protocol != "vless" || n.Address != "1.2.3.4" || n.Port != 443 {
		t.Fatalf("bad node: %+v", n)
	}
	if n.Name != "US-REALITY-01" {
		t.Fatalf("name=%q", n.Name)
	}
	if n.Reality == nil || n.Reality.PublicKey != "AbCdeFg12345" || n.Reality.ShortID != "abc123" || n.Reality.Fingerprint != "chrome" {
		t.Fatalf("reality: %+v", n.Reality)
	}
}

func TestParseVlessWSOmitted(t *testing.T) {
	uri := "vless://uuid@example.com:443?security=tls&type=ws#WS"
	if n := parseNodeURI(uri); n != nil {
		t.Fatalf("expected ws unsupported, got %+v", n)
	}
}

func TestParseSSBase64(t *testing.T) {
	userinfo := base64.RawStdEncoding.EncodeToString([]byte("aes-256-gcm:sekret"))
	uri := "ss://" + userinfo + "@8.8.8.8:8388#SG"
	n := parseNodeURI(uri)
	if n == nil {
		t.Fatal("parse failed")
	}
	if n.Protocol != "ss" || n.Method != "aes-256-gcm" || n.Password != "sekret" || n.Port != 8388 {
		t.Fatalf("bad node: %+v", n)
	}
}

func TestParseSSPlain(t *testing.T) {
	uri := "ss://chacha20-ietf-poly1305:secret@host.test:443#plain"
	n := parseNodeURI(uri)
	if n == nil || n.Method != "chacha20-ietf-poly1305" || n.Password != "secret" || n.Port != 443 {
		t.Fatalf("bad node: %+v", n)
	}
}

func TestParseHysteria2(t *testing.T) {
	uri := "hysteria2://tok3n@example.com:8443?insecure=1&sni=cdn.example.com#HK"
	n := parseNodeURI(uri)
	if n == nil || n.Protocol != "hy2" || n.Port != 8443 || !n.Insecure || n.SNI != "cdn.example.com" || n.Password != "tok3n" {
		t.Fatalf("bad node: %+v", n)
	}
}

func TestParseAnyTLS(t *testing.T) {
	uri := "anytls://pw123@example.com:443?insecure=1#AT"
	n := parseNodeURI(uri)
	if n == nil || n.Protocol != "anytls" || n.Password != "pw123" || !n.Insecure {
		t.Fatalf("bad node: %+v", n)
	}
}

func TestParseSocks5(t *testing.T) {
	uri := "socks5://user:pass@192.0.2.10:1080#SOCKS"
	n := parseNodeURI(uri)
	if n == nil || n.Protocol != "socks5" || n.UserID != "user" || n.Password != "pass" {
		t.Fatalf("bad node: %+v", n)
	}
}

func TestParseClashYAML(t *testing.T) {
	y := `
proxies:
  - name: "JP-ss"
    type: ss
    server: 1.1.1.1
    port: 443
    cipher: aes-128-gcm
    password: p1
  - name: "US-reality"
    type: vless
    server: 2.2.2.2
    port: 443
    uuid: uuu
    network: tcp
    tls: true
    flow: xtls-rprx-vision
    servername: example.com
    reality-opts:
      public-key: pk1
      short-id: 123
      fingerprint: safari
  - name: "skip-trojan"
    type: trojan
    server: 3.3.3.3
    port: 443
    password: x
  - name: "HK-hy2"
    type: hysteria2
    server: 4.4.4.4
    port: 443
    password: hpass
`
	nodes := parseClashYAML(y)
	if len(nodes) != 4 {
		t.Fatalf("expected 4 nodes, got %d", len(nodes))
	}
	var ss, vl, hy, tr *ProxyNode
	for _, n := range nodes {
		switch n.Protocol {
		case "ss":
			ss = n
		case "vless":
			vl = n
		case "hy2":
			hy = n
		case "trojan":
			tr = n
		}
	}
	if ss == nil || ss.Method != "aes-128-gcm" || ss.Password != "p1" {
		t.Fatalf("ss: %+v", ss)
	}
	if hy == nil || hy.Password != "hpass" {
		t.Fatalf("hy2: %+v", hy)
	}
	if tr == nil || tr.Password != "x" || !tr.TLS || tr.SNI != "" || tr.Network != "tcp" {
		t.Fatalf("trojan: %+v", tr)
	}
	if vl == nil || vl.Reality == nil || vl.Reality.PublicKey != "pk1" || vl.Flow != "xtls-rprx-vision" {
		t.Fatalf("vless: %+v", vl)
	}
}

func TestParseSubscriptionBase64Wrapped(t *testing.T) {
	inner := "vless://uuid@1.2.3.4:443?security=reality&pbk=k&fp=chrome&type=tcp#A\nss://YWVzLTEyOC1nY206cDE@5.5.5.5:9000#B\n"
	b64 := base64.StdEncoding.EncodeToString([]byte(inner))
	nodes := parseSubscriptionText(b64)
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
}

func TestPoolSetNodesPreservesRuntimeState(t *testing.T) {
	p := newProxyPool("")
	n1 := &ProxyNode{Name: "a", Protocol: "socks5", Address: "1.2.3.4", Port: 80}
	p.setNodes([]*ProxyNode{n1})
	if !p.mark(n1.Fingerprint, NodeExhausted, "quota") {
		t.Fatal("mark failed")
	}
	// 用同等凭据的新列表替换：状态应保留
	p.setNodes([]*ProxyNode{&ProxyNode{Name: "a2", Protocol: "socks5", Address: "1.2.3.4", Port: 80}})
	if len(p.nodes) != 1 {
		t.Fatalf("nodes=%d", len(p.nodes))
	}
	n2 := p.nodes[0]
	if n2.State != NodeExhausted {
		t.Fatalf("state not preserved: %v", n2.State)
	}
	// 冷却期内 pick 不应选它（回退直连）
	if got := p.pick(false); got != nil {
		t.Fatalf("expected no usable node, got %s", got.Name)
	}
}

func TestPickForcesDifferentNode(t *testing.T) {
	p := newProxyPool("")
	p.setNodes([]*ProxyNode{
		{Name: "n1", Protocol: "socks5", Address: "1.1.1.1", Port: 1},
		{Name: "n2", Protocol: "socks5", Address: "2.2.2.2", Port: 1},
	})
	first := p.pick(false)
	if first == nil {
		t.Fatal("no node")
	}
	second := p.pick(true)
	if second == nil || second.Fingerprint == first.Fingerprint {
		t.Fatalf("force switch failed: %s -> %s", first.Name, second.Name)
	}
}

func TestManualNodeConfigNormalizesProtocol(t *testing.T) {
	cases := map[string]string{
		"hysteria2": "hy2", "hy2": "hy2", "Hysteria2": "hy2",
		"socks5": "socks5", "socks": "socks5", "vless": "vless", "SS": "ss",
	}
	for in, want := range cases {
		n := (ProxyNodeConfig{Protocol: in, Address: "1.2.3.4", Port: 443}).toNode()
		if n.Protocol != want {
			t.Fatalf("protocol %q -> %q, want %q", in, n.Protocol, want)
		}
	}
}

func TestManualNodeConfigCarriesReality(t *testing.T) {
	c := ProxyNodeConfig{Protocol: "vless", Address: "h.x", Port: 443,
		UserID: "u", SNI: "h.x",
		Reality: &RealityConfig{PublicKey: "pk", ShortID: "sid", SpiderX: "/"}}
	n := c.toNode()
	if n.Reality == nil || n.Reality.PublicKey != "pk" || n.Reality.ShortID != "sid" {
		t.Fatalf("reality not carried: %#v", n.Reality)
	}
}

func TestClashVlessWSAndServernamePrecedence(t *testing.T) {
	// servername 与 sni 同时存在时 servername 优先（与借鉴仓库 sing-box 一致）。
	// vless+ws 传输字段（network/path/host）应解析保留。
	y := `
proxies:
  - name: "CF-ws"
    type: vless
    server: cf.example.com
    port: 2096
    uuid: 7a3bac2b-b3ae-4bf6-845a-31fa95bfde26
    tls: true
    servername: edt.example.org
    sni: sni.example.org
    network: ws
    ws-opts:
      path: /
      headers:
        Host: edt.example.org
  - name: "SNI-only"
    type: vmess
    server: 1.2.3.4
    port: 443
    uuid: d6e7f8a9-0123-4567-89ab-cdef01234567
    sni: sni2.example.net
`
	nodes := parseClashYAML(y)
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	v := nodes[0]
	if v.Protocol != "vless" || v.SNI != "edt.example.org" || v.Network != "ws" ||
		v.Path != "/" || v.Host != "edt.example.org" || !v.TLS {
		t.Fatalf("vless-ws: %+v", v)
	}
	if nodes[1].SNI != "sni2.example.net" {
		t.Fatalf("vmess sni-only: %+v", nodes[1])
	}
}

func TestParseVmessAndTrojanURIs(t *testing.T) {
	vmessJSON := `{"v":"2","ps":"HK-vmess","add":"9.9.9.9","port":"443","id":"8f9c1b3e-5a2d-4e7f-9a1c-2b3d4e5f6a7b","aid":"0","scy":"auto","net":"tcp","type":"none","host":"","path":"","tls":"tls","sni":"cdn.example.com"}`
	vmessURL := "vmess://" + base64.StdEncoding.EncodeToString([]byte(vmessJSON)) + "#HK-vmess"
	vm := parseNodeURI(vmessURL)
	if vm == nil || vm.Protocol != "vmess" || vm.Address != "9.9.9.9" || vm.Port != 443 ||
		vm.UserID != "8f9c1b3e-5a2d-4e7f-9a1c-2b3d4e5f6a7b" || !vm.TLS || vm.SNI != "cdn.example.com" {
		t.Fatalf("vmess: %+v", vm)
	}

	tr := parseNodeURI("trojan://pw123@3.4.5.6:8443?security=tls&sni=t.example.com#T-node")
	if tr == nil || tr.Protocol != "trojan" || tr.Address != "3.4.5.6" || tr.Port != 8443 ||
		tr.Password != "pw123" || !tr.TLS || tr.SNI != "t.example.com" {
		t.Fatalf("trojan: %+v", tr)
	}
}
