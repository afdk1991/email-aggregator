package observ

import (
	"context"
	"testing"
)

func TestTraceAndTenantContext(t *testing.T) {
	ctx := context.Background()
	if TraceID(ctx) != "" {
		t.Fatal("empty ctx should have no trace id")
	}
	ctx = WithTraceID(ctx, "trace-1")
	if TraceID(ctx) != "trace-1" {
		t.Fatalf("TraceID = %q, want trace-1", TraceID(ctx))
	}
	ctx = WithTenant(ctx, "tenantA", "acc1")
	if TraceID(ctx) != "trace-1" {
		t.Fatalf("WithTenant wiped trace id: %q", TraceID(ctx))
	}
	// Logger 不应 panic，字段应随 ctx 注入。
	Info(ctx, "hello", "k", "v")
	Warn(ctx, "careful")
	Error(ctx, "boom")
}

func TestCounterIncAndSnapshot(t *testing.T) {
	reg := NewRegistry()
	c := reg.Counter("test_syncs", "demo", "tenant", "status")
	c.Inc(map[string]string{"tenant": "t1", "status": "ok"})
	c.Inc(map[string]string{"tenant": "t1", "status": "ok"})
	c.Inc(map[string]string{"tenant": "t2", "status": "error"})
	if got := c.Value(map[string]string{"tenant": "t1", "status": "ok"}); got != 2 {
		t.Fatalf("t1/ok = %d, want 2", got)
	}
	if got := c.Value(map[string]string{"tenant": "t2", "status": "error"}); got != 1 {
		t.Fatalf("t2/error = %d, want 1", got)
	}
	snap := reg.Snapshot()
	counters, ok := snap["counters"].(map[string]map[string]int64)
	if !ok {
		t.Fatalf("snapshot counters type = %T", snap["counters"])
	}
	if len(counters["test_syncs"]) != 2 {
		t.Fatalf("snapshot entries = %d, want 2", len(counters["test_syncs"]))
	}
}

func TestHistogramP95(t *testing.T) {
	reg := NewRegistry()
	h := reg.Histogram("test_latency", "demo", "backend")
	for i := 1; i <= 100; i++ {
		h.Observe(map[string]string{"backend": "stub"}, float64(i))
	}
	if got := h.Count(map[string]string{"backend": "stub"}); got != 100 {
		t.Fatalf("count = %d, want 100", got)
	}
	// p95 of 1..100: index = int(99*0.95)=94 -> cp[94]=95
	if got := h.P95(map[string]string{"backend": "stub"}); got != 95 {
		t.Fatalf("p95 = %v, want 95", got)
	}
	// 无样本组合返回 0
	if got := h.P95(map[string]string{"backend": "other"}); got != 0 {
		t.Fatalf("p95 empty = %v, want 0", got)
	}
}

func TestDefaultRegistryShared(t *testing.T) {
	a := Default().Counter("shared_demo", "x")
	b := Default().Counter("shared_demo", "x")
	a.Inc(nil)
	if b.Value(nil) != 1 {
		t.Fatalf("shared counter not reused: %d", b.Value(nil))
	}
}
