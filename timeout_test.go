package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// slowSSEHandler 模拟上游 SSE 流：每 interval 发一个 chunk，共 count 个 chunk + [DONE]。
// 通过请求 context 控制退出——客户端断开时 handler 自动停止，避免 httptest.Server.Close 阻塞。
func slowSSEHandler(interval time.Duration, count int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		for i := 1; i <= count; i++ {
			chunk := `{"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[{"delta":{"content":"token"},"finish_reason":null}]}`
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			flusher.Flush()
			// 可中断的 sleep：客户端断开时 context 取消，handler 立即退出
			select {
			case <-time.After(interval):
			case <-r.Context().Done():
				return
			}
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
}

// readSSEStream 用 bufio 逐行读取 SSE 流，返回所有收到的 data 行和最终错误。
func readSSEStream(client *http.Client, url string) (lines []string, readErr error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			lines = append(lines, strings.TrimSpace(line))
		}
		if err != nil {
			if err == io.EOF {
				return lines, nil
			}
			return lines, err
		}
	}
}

// TestClientTimeoutKillsLongSSEStream 复现 bug：
// http.Client.Timeout 从请求发起时就开始倒计时，包含读取 Body 的时间。
// 当 SSE 流持续时间超过 Timeout 时，Go runtime 会取消 context，
// 导致 reader.Read 返回 "context deadline exceeded (Client.Timeout or context cancellation while reading body)"。
//
// 这正是生产环境中 stream_interrupt + "EOF without [DONE]" 的根因。
func TestClientTimeoutKillsLongSSEStream(t *testing.T) {
	// 6 个 chunk × 800ms = 4.8s 总耗时，远超 2s 的 Client.Timeout
	srv := httptest.NewServer(slowSSEHandler(800*time.Millisecond, 6))
	defer srv.Close()

	shortTimeoutClient := &http.Client{
		Timeout: 2 * time.Second,
	}

	lines, readErr := readSSEStream(shortTimeoutClient, srv.URL)

	// 断言：流被中断
	if readErr == nil {
		t.Fatalf("期望 stream 被 Client.Timeout 中断，但读取成功完成；收到 %d 行", len(lines))
	}

	errMsg := readErr.Error()
	if !strings.Contains(errMsg, "Client.Timeout") && !strings.Contains(errMsg, "context deadline exceeded") {
		t.Fatalf("期望错误包含 'Client.Timeout' 或 'context deadline exceeded'，实际: %s", errMsg)
	}

	// 断言：没有收到 [DONE]（流被强制中断）
	for _, line := range lines {
		if line == "data: [DONE]" {
			t.Fatalf("流被 Client.Timeout 中断前不应收到 [DONE]，但收到了；共 %d 行", len(lines))
		}
	}

	t.Logf("✓ Client.Timeout 复现成功：读到 %d 行后被中断，错误: %s", len(lines), errMsg)
}

// TestTransportTimeoutSurvivesLongSSEStream 对照组：
// 不设 Client.Timeout，仅用 Transport 级别的 ResponseHeaderTimeout。
// 连接建立和头部阶段有超时保护，但 Body 读取阶段不受限制，
// SSE 流可以正常完成并收到 [DONE]。
func TestTransportTimeoutSurvivesLongSSEStream(t *testing.T) {
	srv := httptest.NewServer(slowSSEHandler(800*time.Millisecond, 6))
	defer srv.Close()

	transportClient := &http.Client{
		// 不设 Timeout！
		Transport: &http.Transport{
			ResponseHeaderTimeout: 5 * time.Second,
		},
	}

	lines, readErr := readSSEStream(transportClient, srv.URL)

	if readErr != nil {
		t.Fatalf("Transport 超时模式下流应正常完成，但出错: %s；收到 %d 行", readErr, len(lines))
	}

	found := false
	for _, line := range lines {
		if line == "data: [DONE]" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("应收到 [DONE] 终止信号，实际共 %d 行: %v", len(lines), lines)
	}

	t.Logf("✓ Transport 超时模式成功：读到 %d 行，正常收到 [DONE]", len(lines))
}

// TestContextCancellationStopsStream 验证：当上游 context 被取消时，
// handler 能立即停止（不会阻塞 httptest.Server.Close），
// 且客户端收到的错误也是 context 相关的。
func TestContextCancellationStopsStream(t *testing.T) {
	// 一个很慢的流：每个 chunk 间隔 5s，总共 10 个 chunk
	srv := httptest.NewServer(slowSSEHandler(5*time.Second, 10))
	defer srv.Close()

	// 用一个短超时的 context 来驱动请求
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// 连接阶段超时也算通过
		if ctx.Err() != nil {
			t.Logf("✓ context 取消生效（连接阶段）: %s", err)
			return
		}
		t.Fatalf("意外错误: %s", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	var lines []string
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			lines = append(lines, strings.TrimSpace(line))
		}
		if err != nil {
			if ctx.Err() != nil {
				t.Logf("✓ context 取消生效（读取阶段）：读到 %d 行后中断，错误: %s", len(lines), err)
				return
			}
			t.Fatalf("意外读取错误: %s", err)
		}
	}
}
