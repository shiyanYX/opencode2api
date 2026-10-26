package main

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// idleReadTimeout 上游响应体「连续多久没有任何字节」算卡死。
//
// 为什么不用「总时长上限」：模型生成本来就可能跑几分钟（实测成功非流式有 >4 分钟的），
// 只有「完全没有数据」才说明真的挂了。用总时长会把长生成/长流式一并误杀
// （见 R2 验收 blocker C1）。
//
// 生产证据（2026-10-19）：
//   - 成功非流式请求里 >120s 的样本中有 148 条是「200 + 0 token」，最长 38.7 分钟；
//   - 失败请求最长 39.7 分钟；
//   - C-1 已给 TLS 握手加上界，但 body 读取阶段此前没有任何上界。
//
// T3（2026-10-26）明确不动这里的语义：首字节 deadline 只约束「建连后等响应头」
// 这一段（由各 Transport 的 ResponseHeaderTimeout 实现）；一旦首字节到达，后续
// 仍按「连续多久没有新字节」判定卡死，绝不用总时长上限去误杀长生成。
//
// 做成 var 便于测试注入更短的值。
var idleReadTimeout = 120 * time.Second

// idleReadCloser 给上游响应体套一层「空闲读超时」：
// 每读到字节就重新计时；连续 timeout 内没有任何数据 ⇒ 关闭底层 body 并报错。
//
// 不能拿 http.Client.Timeout 或请求 ctx 的 deadline 替代：
//   - 它们是「总时长」，会误杀长生成/长流式；
//   - 流式的 body 是在 callOpenCodeAPIStream 返回【之后】才被调用方读取的，
//     deadline 更管不到那一段。
type idleReadCloser struct {
	rc      io.ReadCloser
	timeout time.Duration
	done    bool
	err     error
}

// newIdleReadCloser 包装响应体；rc 为空或 timeout<=0 时原样返回（相当于关闭该保护）。
// 幂等：已经是本类型就不重复套。
func newIdleReadCloser(rc io.ReadCloser, timeout time.Duration) io.ReadCloser {
	if rc == nil || timeout <= 0 {
		return rc
	}
	if _, ok := rc.(*idleReadCloser); ok {
		return rc
	}
	return &idleReadCloser{rc: rc, timeout: timeout}
}

func (r *idleReadCloser) Read(p []byte) (int, error) {
	if r.done {
		if r.err == nil {
			return 0, io.EOF
		}
		return 0, r.err
	}
	if len(p) == 0 {
		return 0, nil
	}

	// 读进本函数自己的缓冲：一旦超时返回，后台 goroutine 也不会写回调用方的 p
	// （否则调用方可能已经复用 p，构成数据竞争）。
	tmp := make([]byte, len(p))
	type readResult struct {
		n   int
		err error
	}
	ch := make(chan readResult, 1) // 容量 1：后台 goroutine 必定能投递完，不会泄漏
	go func() {
		n, err := r.rc.Read(tmp)
		ch <- readResult{n, err}
	}()

	timer := time.NewTimer(r.timeout)
	defer timer.Stop()

	select {
	case got := <-ch:
		if got.n > 0 {
			copy(p, tmp[:got.n])
		}
		if got.err != nil {
			r.done = true
			r.err = got.err
		}
		return got.n, got.err

	case <-timer.C:
		r.done = true
		r.err = fmt.Errorf("upstream idle timeout: %v 内没有收到任何数据", r.timeout)
		_ = r.rc.Close() // 解除后台阻塞中的 Read，避免 goroutine 泄漏
		return 0, r.err
	}
}

func (r *idleReadCloser) Close() error {
	if !r.done {
		r.done = true
		r.err = errors.New("idleReadCloser: closed")
	}
	return r.rc.Close()
}
