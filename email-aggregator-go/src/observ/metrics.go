package observ

import (
	"sort"
	"strings"
	"sync"
)

// Counter 带标签维度的递增计数器（线程安全）。
type Counter struct {
	desc   string
	keys   []string
	mu     sync.Mutex
	values map[string]int64
}

// NewCounter 构造计数器；keys 为标签维度名（如 "tenant","account","status"）。
func NewCounter(name, desc string, keys ...string) *Counter {
	return &Counter{desc: desc, keys: keys, values: make(map[string]int64)}
}

func labelKey(keys []string, labels map[string]string) string {
	if len(keys) == 0 {
		return ""
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}

// Inc 指定标签组合 +1。
func (c *Counter) Inc(labels map[string]string) {
	c.mu.Lock()
	c.values[labelKey(c.keys, labels)]++
	c.mu.Unlock()
}

// Add 指定标签组合加 delta。
func (c *Counter) Add(labels map[string]string, delta int64) {
	c.mu.Lock()
	c.values[labelKey(c.keys, labels)] += delta
	c.mu.Unlock()
}

// Value 读取指定标签组合当前值。
func (c *Counter) Value(labels map[string]string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[labelKey(c.keys, labels)]
}

// Snapshot 返回 {label组合: 值}。
func (c *Counter) Snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.values))
	for k, v := range c.values {
		out[k] = v
	}
	return out
}

// Histogram 简化直方图：采样全部样本，计算 count/sum/p95。
// PoC 用途：生产环境应改为限容滑动窗口或分桶（Prometheus 导出侧保留语义）。
type Histogram struct {
	desc string
	keys []string
	mu   sync.Mutex
	obs  map[string][]float64
}

// NewHistogram 构造直方图；keys 为标签维度名。
func NewHistogram(name, desc string, keys ...string) *Histogram {
	return &Histogram{desc: desc, keys: keys, obs: make(map[string][]float64)}
}

// Observe 记录一个样本。
func (h *Histogram) Observe(labels map[string]string, v float64) {
	h.mu.Lock()
	key := labelKey(h.keys, labels)
	h.obs[key] = append(h.obs[key], v)
	h.mu.Unlock()
}

// Count 样本数。
func (h *Histogram) Count(labels map[string]string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.obs[labelKey(h.keys, labels)])
}

// Sum 样本和。
func (h *Histogram) Sum(labels map[string]string) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	var s float64
	for _, v := range h.obs[labelKey(h.keys, labels)] {
		s += v
	}
	return s
}

// P95 第 95 百分位（样本升序，线性取高位索引）。无样本返回 0。
func (h *Histogram) P95(labels map[string]string) float64 {
	h.mu.Lock()
	s := h.obs[labelKey(h.keys, labels)]
	cp := make([]float64, len(s))
	copy(cp, s)
	h.mu.Unlock()
	if len(cp) == 0 {
		return 0
	}
	sort.Float64s(cp)
	return cp[int(float64(len(cp)-1)*0.95)]
}

func (h *Histogram) Snapshot() map[string]map[string]float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]map[string]float64, len(h.obs))
	for k, v := range h.obs {
		cp := make([]float64, len(v))
		copy(cp, v)
		sort.Float64s(cp)
		var sum float64
		for _, x := range cp {
			sum += x
		}
		p95 := 0.0
		if len(cp) > 0 {
			p95 = cp[int(float64(len(cp)-1)*0.95)]
		}
		out[k] = map[string]float64{"count": float64(len(cp)), "sum": sum, "p95": p95}
	}
	return out
}

// Registry 指标注册表（全局单例经 Default() 获取；按需也可 NewRegistry 隔离）。
type Registry struct {
	mu       sync.RWMutex
	counters map[string]*Counter
	hists    map[string]*Histogram
}

// NewRegistry 构造独立注册表。
func NewRegistry() *Registry {
	return &Registry{counters: make(map[string]*Counter), hists: make(map[string]*Histogram)}
}

// Counter 取/建命名计数器（同名复用，keys 仅首次生效）。
func (r *Registry) Counter(name, desc string, keys ...string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	c := NewCounter(name, desc, keys...)
	r.counters[name] = c
	return c
}

// Histogram 取/建命名直方图。
func (r *Registry) Histogram(name, desc string, keys ...string) *Histogram {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.hists[name]; ok {
		return h
	}
	hh := NewHistogram(name, desc, keys...)
	r.hists[name] = hh
	return hh
}

// Snapshot 导出全部指标（供 health/metrics 端点）。
func (r *Registry) Snapshot() map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	counters := make(map[string]map[string]int64, len(r.counters))
	for name, c := range r.counters {
		counters[name] = c.Snapshot()
	}
	hists := make(map[string]map[string]map[string]float64, len(r.hists))
	for name, h := range r.hists {
		hists[name] = h.Snapshot()
	}
	return map[string]any{"counters": counters, "histograms": hists}
}

var defaultRegistry = NewRegistry()

// Default 返回全局默认注册表（埋点统一写入此处）。
func Default() *Registry { return defaultRegistry }

// 预定义业务指标（对齐蓝图 §10 监控与运维体系 / T9 可观测性埋点 / §8 AI 统一埋点）。
//
//	SLI 覆盖：同步成功率/时延、摄取吞吐、AI 调用延迟/命中后端/脱敏命中/失败、上游连接器错误。
var (
	SyncTotal        = defaultRegistry.Counter("sync_syncs_total", "同步任务总数", "tenant", "account", "provider", "status")
	SyncDurationMs   = defaultRegistry.Histogram("sync_duration_ms", "同步时延(毫秒)", "tenant", "account", "mode")
	IngestTotal      = defaultRegistry.Counter("ingest_mails_total", "摄取邮件总数", "tenant", "account", "status")
	IngestDurationMs = defaultRegistry.Histogram("ingest_duration_ms", "单封摄取时延(毫秒)", "tenant", "account")
	AICallsTotal     = defaultRegistry.Counter("ai_calls_total", "AI 调用总数", "tenant", "capability", "backend", "allowed", "redacted")
	AIDurationMs     = defaultRegistry.Histogram("ai_duration_ms", "AI 调用时延(毫秒)", "backend")
	AIErrorsTotal    = defaultRegistry.Counter("ai_errors_total", "AI 调用失败总数", "tenant", "backend")
	ConnectorErrors  = defaultRegistry.Counter("connector_errors_total", "连接器错误总数", "tenant", "account", "provider")
)
