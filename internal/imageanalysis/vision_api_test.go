package imageanalysis

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"denova/config"
)

// callVisionAPI 按 SSE 解析响应，因此请求必须携带 stream=true，否则兼容端点
// 返回完整 JSON（无 data: 行），解析结果恒为空——这正是"视觉模型返回为空"
// 批量失败的根因。本测试同时锁定请求侧（stream 开关）与解析侧（delta 拼接）。
func TestCallVisionAPIRequestsStreamingAndParsesSSE(t *testing.T) {
	var gotStream bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req chatRequest
		if err := json.Unmarshal(body, &req); err == nil {
			gotStream = req.Stream
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := &Service{cfg: &config.Config{}}
	content, err := s.callVisionAPI(context.Background(), visionModelConfig{BaseURL: srv.URL}, chatRequest{Model: "test-model"})
	if err != nil {
		t.Fatalf("callVisionAPI 返回错误: %v", err)
	}
	if !gotStream {
		t.Error("请求体未携带 stream=true，兼容端点将返回非 SSE 响应导致空解析")
	}
	if content != "Hello" {
		t.Errorf("SSE 内容拼接结果 = %q, 期望 %q", content, "Hello")
	}
}

// 空闲超时：相邻数据间隔超过阈值应报错，而不是无限等待；收到过数据的流
// 不应因整体时长超过阈值被切断（区别于 http.Client.Timeout 的整体语义）。
func TestCallVisionAPIIdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n"))
		flusher.Flush()
		// 客户端在空闲超时后断开，request context 随之取消，handler 立即退出。
		<-r.Context().Done()
	}))
	defer srv.Close()

	cfg := &config.Config{}
	silence := 1
	cfg.ImageAnalysis.RequestTimeoutSeconds = &silence
	s := &Service{cfg: cfg}

	start := time.Now()
	_, err := s.callVisionAPI(context.Background(), visionModelConfig{BaseURL: srv.URL}, chatRequest{Model: "test-model"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("空闲超时未触发，callVisionAPI 正常返回")
	}
	if !strings.Contains(err.Error(), "空闲超时") {
		t.Errorf("错误信息 = %q, 期望包含 %q", err.Error(), "空闲超时")
	}
	// 客户端断开后 handler 经 context 取消立即退出，整个测试不应显著超过阈值。
	if elapsed > 3*time.Second {
		t.Errorf("空闲超时触发耗时 %v，疑似未按空闲语义生效", elapsed)
	}
}
