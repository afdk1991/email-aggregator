package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"email-aggregator-go/src/observ"
)

// TestHealthWithObserv_DetailAndMetrics 验证挂载 observ 后 /api/health 返回 goroutines + 指标快照，
// 且每个响应注入 X-Trace-ID；/api/metrics 返回 JSON 指标快照。
func TestHealthWithObserv_DetailAndMetrics(t *testing.T) {
	reg := observ.NewRegistry()
	reg.Counter("demo_total", "demo", "status").Inc(map[string]string{"status": "ok"})

	s := NewApiServer(nil, nil, nil, 0).WithObserv(reg)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Trace-ID") == "" {
		t.Fatal("X-Trace-ID header not injected by trace middleware")
	}
	var body struct {
		OK         bool           `json:"ok"`
		Metrics    map[string]any `json:"metrics"`
		Goroutines int            `json:"goroutines"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK {
		t.Fatal("ok = false")
	}
	if body.Metrics == nil {
		t.Fatal("health metrics snapshot is nil")
	}
	if body.Goroutines <= 0 {
		t.Fatalf("goroutines = %d, want > 0", body.Goroutines)
	}

	resp2, err := http.Get(srv.URL + "/api/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("metrics status = %d, want 200", resp2.StatusCode)
	}
	var mbody map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&mbody); err != nil {
		t.Fatal(err)
	}
	if _, ok := mbody["counters"]; !ok {
		t.Fatal("/api/metrics missing counters")
	}
	if _, ok := mbody["histograms"]; !ok {
		t.Fatal("/api/metrics missing histograms")
	}
}

// TestTraceIDPassthrough 验证客户端传入的 X-Trace-ID 被原样透传。
func TestTraceIDPassthrough(t *testing.T) {
	s := NewApiServer(nil, nil, nil, 0).WithObserv(observ.NewRegistry())
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/health", nil)
	req.Header.Set("X-Trace-ID", "trace-xyz")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Trace-ID"); got != "trace-xyz" {
		t.Fatalf("X-Trace-ID not passthrough: got %q, want trace-xyz", got)
	}
}

// TestMetricsEndpoint_NotMounted 验证未挂载 observ 时 /api/metrics 不注册（501）。
func TestMetricsEndpoint_NotMounted(t *testing.T) {
	s := NewApiServer(nil, nil, nil, 0)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unmounted /api/metrics status = %d, want 404 (route not registered)", resp.StatusCode)
	}
}
