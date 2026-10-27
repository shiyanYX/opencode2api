package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	probeClassOK          = "ok"
	probeClassQuota       = "quota"
	probeClassDenied      = "denied"
	probeClassUnreachable = "unreachable"
	probeClassError       = "error"
)

const (
	probePreflightTimeout = 1500 * time.Millisecond
	probeModelTimeout     = 20 * time.Second
	probeBodyLimit        = 4096
)

type probeResult struct {
	Fingerprint string    `json:"fingerprint"`
	Class       string    `json:"class"`
	Status      int       `json:"status"`
	LatencyMS   int64     `json:"latency_ms"`
	Error       string    `json:"error"`
	At          time.Time `json:"at"`
}

func classifyProbeResponse(status int, body []byte) string {
	switch {
	case status >= 200 && status < 300:
		return probeClassOK
	case status == http.StatusTooManyRequests:
		if ok, _ := classifyQuota(status, body); ok {
			return probeClassQuota
		}
		return probeClassError
	case status == http.StatusForbidden:
		if isFreeTierErrorBody(body) {
			return probeClassDenied
		}
		return probeClassError
	default:
		return probeClassError
	}
}

var defaultProbeClient = func(fp string) *http.Client {
	return bunClientFor(proxyPool.getClient(fp))
}

var probeClientFor = defaultProbeClient

func probeOne(ctx context.Context, node ProxyNode, model, healthURL string) probeResult {
	res := probeResult{Fingerprint: node.Fingerprint, At: time.Now()}
	client := probeClientFor(node.Fingerprint)
	if client == nil {
		res.Class = probeClassUnreachable
		res.Error = "node client unavailable"
		return res
	}

	preCtx, cancel := context.WithTimeout(ctx, probePreflightTimeout)
	preReq, err := http.NewRequestWithContext(preCtx, http.MethodGet, healthURL, nil)
	if err != nil {
		cancel()
		res.Class = probeClassUnreachable
		res.Error = err.Error()
		return res
	}
	preReq.Header.Set("Authorization", (UpstreamAuth{Mode: AuthRoutePublic}).authorizationHeader())
	preReq.Header.Set("x-opencode-session", getOrCreateSessionByNode(node.Fingerprint))
	preResp, err := client.Do(preReq)
	cancel()
	if err != nil {
		res.Class = probeClassUnreachable
		res.Error = err.Error()
		return res
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(preResp.Body, probeBodyLimit))
	preResp.Body.Close()
	if preResp.StatusCode < 200 || preResp.StatusCode >= 300 {
		res.Class = probeClassUnreachable
		res.Status = preResp.StatusCode
		res.Error = fmt.Sprintf("preflight status %d", preResp.StatusCode)
		return res
	}

	bodyMap := map[string]any{
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	}
	req, err := buildOCRequestWithEndpoint(model, bodyMap, UpstreamAuth{Mode: AuthRoutePublic}, false, node.Fingerprint, ocClientVersion())
	if err != nil {
		res.Class = probeClassError
		res.Error = err.Error()
		return res
	}
	modelCtx, cancel := context.WithTimeout(ctx, probeModelTimeout)
	req = req.WithContext(modelCtx)
	start := time.Now()
	resp, err := client.Do(req)
	cancel()
	if err != nil {
		res.Class = probeClassUnreachable
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))
	res.LatencyMS = time.Since(start).Milliseconds()
	res.Status = resp.StatusCode
	res.At = time.Now()
	if readErr != nil {
		res.Class = probeClassError
		res.Error = readErr.Error()
		return res
	}
	res.Class = classifyProbeResponse(resp.StatusCode, body)
	return res
}
