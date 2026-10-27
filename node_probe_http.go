package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

const defaultProbeModel = "mimo-v2.6-flash-free"

var nodeProber = newProbeManager()

func probeModelOrDefault(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultProbeModel
	}
	return s
}

func writeProbeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeProbeMethodNotAllowed(w http.ResponseWriter) {
	writeProbeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
}

func adminNodeProbeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProbeMethodNotAllowed(w)
		return
	}
	var req struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProbeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	var node *ProxyNode
	for _, n := range proxyPool.snapshot() {
		if n.Fingerprint == req.Fingerprint {
			cp := *n
			node = &cp
			break
		}
	}
	if node == nil {
		writeProbeJSON(w, http.StatusNotFound, map[string]any{"error": "node not found"})
		return
	}
	model := probeModelOrDefault(loadConfig(configPath).ProbeModel)
	res, err := nodeProber.Single(r.Context(), *node, model, proxyPool.effectiveProbeURL())
	if err != nil {
		switch {
		case errors.Is(err, errProbeCooldown):
			secs := nodeProber.SingleCooldownRemaining(node.Fingerprint)
			if secs <= 0 {
				secs = 1
			}
			writeProbeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "probe cooldown", "retry_after_s": secs})
		default:
			writeProbeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		}
		return
	}
	writeProbeJSON(w, http.StatusOK, res)
}

func adminNodeProbeAllHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		nodes := proxyPool.snapshot()
		values := make([]ProxyNode, 0, len(nodes))
		for _, n := range nodes {
			values = append(values, *n)
		}
		model := probeModelOrDefault(loadConfig(configPath).ProbeModel)
		id, total, err := nodeProber.StartAll(r.Context(), values, model, proxyPool.effectiveProbeURL())
		if err != nil {
			switch {
			case errors.Is(err, errProbeBusy):
				writeProbeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "probe already running"})
			case errors.Is(err, errProbeCooldown):
				secs := nodeProber.AllCooldownRemaining()
				if secs <= 0 {
					secs = 1
				}
				writeProbeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "probe cooldown", "retry_after_s": secs})
			default:
				writeProbeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			}
			return
		}
		writeProbeJSON(w, http.StatusAccepted, map[string]any{"job_id": id, "total": total})
	case http.MethodGet:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeProbeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
			return
		}
		view, ok := nodeProber.Status(id)
		if !ok {
			writeProbeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
			return
		}
		writeProbeJSON(w, http.StatusOK, view)
	default:
		writeProbeMethodNotAllowed(w)
	}
}

func adminNodeProbeAllCancelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProbeMethodNotAllowed(w)
		return
	}
	var req struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProbeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if req.JobID == "" || !nodeProber.Cancel(req.JobID) {
		writeProbeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	writeProbeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}
