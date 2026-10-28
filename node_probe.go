package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	probeClassOK           = "ok"
	probeClassQuota        = "quota"
	probeClassDenied       = "denied"
	probeClassUnreachable  = "unreachable"
	probeClassError        = "error"
	probeClassQuotaUnknown = "quota_unknown"
)

const (
	probePreflightTimeout = 6 * time.Second
	probeModelTimeout     = 20 * time.Second
	probeBodyLimit        = 4096
)

type probeResult struct {
	Fingerprint string    `json:"fingerprint"`
	Class       string    `json:"class"`
	Stage       string    `json:"stage"`
	Status      int       `json:"status"`
	LatencyMS   int64     `json:"latency_ms"`
	Error       string    `json:"error"`
	Timeout     bool      `json:"timeout,omitempty"`
	At          time.Time `json:"at"`
}

func probeErrorTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func classifyProbeResponse(status int, body []byte) string {
	switch {
	case status >= 200 && status < 300:
		return probeClassOK
	case status == http.StatusTooManyRequests:
		if ok, _ := classifyQuota(status, body); ok {
			return probeClassQuota
		}
		return probeClassQuotaUnknown
	case status == http.StatusForbidden:
		return probeClassDenied
	default:
		return probeClassError
	}
}

func classifyProbePreflight(status int, body []byte) string {
	switch {
	case status >= 200 && status < 300:
		return probeClassOK
	case status == http.StatusTooManyRequests:
		if ok, _ := classifyQuota(status, body); ok {
			return probeClassQuota
		}
		return probeClassQuotaUnknown
	case status == http.StatusForbidden:
		return probeClassDenied
	default:
		return probeClassError
	}
}

var probeReqSeq uint64

func newProbeReqID() string {
	return fmt.Sprintf("req_probe_%d_%d", time.Now().UnixNano(), atomic.AddUint64(&probeReqSeq, 1))
}

func probeCallLogEnabled() bool {
	callLog.mu.RLock()
	defer callLog.mu.RUnlock()
	return callLog.path != ""
}

var defaultProbeClient = func(fp string) *http.Client {
	return bunClientFor(proxyPool.getClient(fp))
}

var probeClientFor = defaultProbeClient

func probeOne(ctx context.Context, node ProxyNode, model, healthURL string) (res probeResult) {
	res = probeResult{Fingerprint: node.Fingerprint, Stage: string(ProbeStagePreflight), At: time.Now()}
	if probeCallLogEnabled() {
		ctx = context.WithValue(ctx, reqIDKey, newProbeReqID())
		ctx = beginCallLog(ctx, "/api/nodes/probe", model, false, "probe")
		defer func() {
			status := res.Status
			if status == 0 && res.Class == probeClassOK {
				status = http.StatusOK
			}
			callLogEvent(ctx, "probe", node.Fingerprint,
				fmt.Sprintf("class=%s stage=%s status=%d latency=%dms", res.Class, res.Stage, res.Status, res.LatencyMS))
			callLogFinish(ctx, status, res.Error, 0, 0, 0, 0)
		}()
	}
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
		res.Timeout = probeErrorTimeout(err)
		res.Error = err.Error()
		return res
	}
	preBody, _ := io.ReadAll(io.LimitReader(preResp.Body, probeBodyLimit))
	preResp.Body.Close()
	if preResp.StatusCode < 200 || preResp.StatusCode >= 300 {
		res.Stage = string(ProbeStagePreflight)
		res.Class = classifyProbePreflight(preResp.StatusCode, preBody)
		res.Status = preResp.StatusCode
		res.Error = fmt.Sprintf("preflight status %d", preResp.StatusCode)
		return res
	}

	res.Stage = string(ProbeStageRequest)
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
		res.Timeout = probeErrorTimeout(err)
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
