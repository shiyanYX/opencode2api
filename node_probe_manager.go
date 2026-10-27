package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

var (
	errProbeCooldown = errors.New("probe cooldown")
	errProbeBusy     = errors.New("probe already running")
	errProbeNotFound = errors.New("probe job not found")
)

const (
	probeSingleCooldown = 60 * time.Second
	probeAllCooldown    = 10 * time.Minute
	probeAllConcurrency = 4
	probeAllJitter      = 150 * time.Millisecond
)

var probeClock = time.Now

var probeJobSeq atomic.Uint64

type probeJobView struct {
	ID         string         `json:"id"`
	Running    bool           `json:"running"`
	Done       int            `json:"done"`
	Total      int            `json:"total"`
	Results    []probeResult  `json:"results"`
	Summary    map[string]int `json:"summary"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at"`
}

type probeJob struct {
	id         string
	running    bool
	canceled   bool
	done       int
	total      int
	results    []probeResult
	summary    map[string]int
	startedAt  time.Time
	finishedAt time.Time
	cancel     context.CancelFunc
}

type probeManager struct {
	mu         sync.Mutex
	lastSingle map[string]time.Time
	results    map[string]probeResult
	job        *probeJob
	lastAllEnd time.Time
}

func newProbeManager() *probeManager {
	return &probeManager{
		lastSingle: make(map[string]time.Time),
		results:    make(map[string]probeResult),
	}
}

func (m *probeManager) Single(ctx context.Context, node ProxyNode, model, healthURL string) (probeResult, error) {
	now := probeClock()
	m.mu.Lock()
	if last, ok := m.lastSingle[node.Fingerprint]; ok && now.Sub(last) < probeSingleCooldown {
		m.mu.Unlock()
		return probeResult{}, errProbeCooldown
	}
	m.lastSingle[node.Fingerprint] = now
	m.mu.Unlock()

	res := probeOne(ctx, node, model, healthURL)

	m.mu.Lock()
	m.results[node.Fingerprint] = res
	m.mu.Unlock()
	return res, nil
}

// SingleCooldownRemaining 返回单节点探测剩余冷却秒数（向上取整，0=无冷却）。
func (m *probeManager) SingleCooldownRemaining(fp string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	last, ok := m.lastSingle[fp]
	if !ok {
		return 0
	}
	return cooldownSeconds(probeSingleCooldown - (probeClock().Sub(last)))
}

// AllCooldownRemaining 返回全量探测剩余冷却秒数（向上取整，0=无冷却）。
func (m *probeManager) AllCooldownRemaining() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastAllEnd.IsZero() {
		return 0
	}
	return cooldownSeconds(probeAllCooldown - (probeClock().Sub(m.lastAllEnd)))
}

func cooldownSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(math.Ceil(d.Seconds()))
}

func (m *probeManager) StartAll(ctx context.Context, nodes []ProxyNode, model, healthURL string) (string, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.job != nil && m.job.running {
		return "", 0, errProbeBusy
	}
	now := probeClock()
	if !m.lastAllEnd.IsZero() && now.Sub(m.lastAllEnd) < probeAllCooldown {
		return "", 0, errProbeCooldown
	}

	id := fmt.Sprintf("probe-%d", probeJobSeq.Add(1))
	jobCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	job := &probeJob{
		id:        id,
		running:   true,
		total:     len(nodes),
		results:   make([]probeResult, 0, len(nodes)),
		summary:   make(map[string]int),
		startedAt: now,
		cancel:    cancel,
	}
	m.job = job
	go m.run(jobCtx, job, nodes, model, healthURL)
	return id, len(nodes), nil
}

func (m *probeManager) run(ctx context.Context, job *probeJob, nodes []ProxyNode, model, healthURL string) {
	sem := make(chan struct{}, probeAllConcurrency)
	var wg sync.WaitGroup
	wg.Add(len(nodes))
	for _, node := range nodes {
		go func(node ProxyNode) {
			defer wg.Done()
			select {
			case <-ctx.Done():
				return
			case <-time.After(probeAllJitter):
			}
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if ctx.Err() != nil {
				return
			}
			res := probeOne(ctx, node, model, healthURL)

			m.mu.Lock()
			if job.canceled && ctx.Err() != nil && res.Class == probeClassUnreachable {
				res.Class = "canceled"
				res.Error = "canceled"
			}
			job.results = append(job.results, res)
			job.done++
			job.summary[res.Class]++
			m.mu.Unlock()
		}(node)
	}
	wg.Wait()
	job.cancel()

	m.mu.Lock()
	job.running = false
	job.finishedAt = probeClock()
	m.lastAllEnd = job.finishedAt
	m.mu.Unlock()
}

func (m *probeManager) Status(jobID string) (probeJobView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job == nil || m.job.id != jobID {
		return probeJobView{}, false
	}
	return probeJobView{
		ID:         m.job.id,
		Running:    m.job.running,
		Done:       m.job.done,
		Total:      m.job.total,
		Results:    append([]probeResult(nil), m.job.results...),
		Summary:    copyProbeSummary(m.job.summary),
		StartedAt:  m.job.startedAt,
		FinishedAt: m.job.finishedAt,
	}, true
}

func (m *probeManager) Cancel(jobID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job == nil || m.job.id != jobID || !m.job.running {
		return false
	}
	m.job.canceled = true
	m.job.cancel()
	return true
}

func (m *probeManager) Snapshot() map[string]probeResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]probeResult, len(m.results))
	for fp, res := range m.results {
		out[fp] = res
	}
	return out
}

func copyProbeSummary(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
