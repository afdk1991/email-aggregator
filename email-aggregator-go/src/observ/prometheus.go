//go:build integration

package observ

import (
	"fmt"
	"sort"
	"strings"
)

// PrometheusExposition 把注册表渲染为 Prometheus 文本 exposition 格式。
// 零依赖手写（不引入 prometheus client，遵守项目零新依赖约束），可直接挂载为 /metrics 端点，
// 由 Prometheus 抓取后接入 Grafana（对齐蓝图 §10 Prometheus + Grafana）。
func (r *Registry) PrometheusExposition() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var b strings.Builder
	b.WriteString("# metrics exported by email-aggregator-go observ (zero-dep Prometheus exposition)\n")

	names := make([]string, 0, len(r.counters))
	for n := range r.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		c := r.counters[name]
		fmt.Fprintf(&b, "# HELP %s %s\n", name, c.desc)
		fmt.Fprintf(&b, "# TYPE %s counter\n", name)
		for lbl, v := range c.Snapshot() {
			if lbl == "" {
				fmt.Fprintf(&b, "%s %d\n", name, v)
			} else {
				fmt.Fprintf(&b, "%s{%s} %d\n", name, lbl, v)
			}
		}
	}

	hnames := make([]string, 0, len(r.hists))
	for n := range r.hists {
		hnames = append(hnames, n)
	}
	sort.Strings(hnames)
	for _, name := range hnames {
		h := r.hists[name]
		fmt.Fprintf(&b, "# HELP %s %s\n", name, h.desc)
		fmt.Fprintf(&b, "# TYPE %s histogram\n", name)
		for lbl, s := range h.Snapshot() {
			suffix := ""
			if lbl != "" {
				suffix = "{" + lbl + "}"
			}
			fmt.Fprintf(&b, "%s_count%s %d\n", name, suffix, int64(s["count"]))
			fmt.Fprintf(&b, "%s_sum%s %v\n", name, suffix, s["sum"])
			fmt.Fprintf(&b, "%s_p95%s %v\n", name, suffix, s["p95"])
		}
	}
	return b.String()
}
