// Package observ 提供零依赖的可观测性原语：
//   - 结构化日志（log/slog，JSON），自动携带 trace/tenant/account 上下文字段；
//   - 进程内指标注册表（Counter 带标签 / Histogram 简化 P95 / Snapshot 导出）；
//   - trace-id 跨调用链贯穿（HTTP → 编排 → 摄取 → AI 网关）。
//
// 真实导出侧（Prometheus exposition 文本）由 //go:build integration 隔离在 prometheus.go，
// 默认构建仅保留进程内 Snapshot()，供 /api/health 与 /api/metrics 返回。
// 对齐架构蓝图 §10 监控与运维体系 / T9 可观测性埋点 / 第 8 章基础设施层。
package observ

import (
	"context"
	"log/slog"
	"os"
)

// octx 贯穿一次请求/任务的上下文字段。
type octx struct {
	TraceID   string
	TenantID  string
	AccountID string
}

type ctxKeyType int

const octxKey ctxKeyType = 0

func getCtx(ctx context.Context) octx {
	if v, ok := ctx.Value(octxKey).(octx); ok {
		return v
	}
	return octx{}
}

func setCtx(ctx context.Context, fn func(*octx)) context.Context {
	c := getCtx(ctx)
	fn(&c)
	return context.WithValue(ctx, octxKey, c)
}

// WithTraceID 注入分布式追踪 trace-id（贯穿 HTTP → 编排 → 摄取 → AI）。
func WithTraceID(ctx context.Context, id string) context.Context {
	return setCtx(ctx, func(c *octx) { c.TraceID = id })
}

// TraceID 读取当前 trace-id（空串表示未注入）。
func TraceID(ctx context.Context) string { return getCtx(ctx).TraceID }

// WithTenant 绑定租户与账户上下文（ADR-009 多租户隔离边界贯穿日志）。
func WithTenant(ctx context.Context, tenantID, accountID string) context.Context {
	return setCtx(ctx, func(c *octx) { c.TenantID = tenantID; c.AccountID = accountID })
}

// Logger 返回携带 ctx 上下文字段（trace/tenant/account）的结构化日志器。
func Logger(ctx context.Context) *slog.Logger {
	c := getCtx(ctx)
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	l := slog.New(h)
	if c.TraceID != "" {
		l = l.With("trace_id", c.TraceID)
	}
	if c.TenantID != "" {
		l = l.With("tenant_id", c.TenantID)
	}
	if c.AccountID != "" {
		l = l.With("account_id", c.AccountID)
	}
	return l
}

// Info/Warn/Error 结构化日志快捷方法（字段自动带 trace/tenant/account）。
func Info(ctx context.Context, msg string, args ...any) {
	Logger(ctx).Info(msg, args...)
}

func Warn(ctx context.Context, msg string, args ...any) {
	Logger(ctx).Warn(msg, args...)
}

func Error(ctx context.Context, msg string, args ...any) {
	Logger(ctx).Error(msg, args...)
}
