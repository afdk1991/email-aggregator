// Package auth — AuditSink ADR-009 审计事件强制透传：所有审计事件 100% 带 tenantId，
// 空 Actor 拒绝（强制身份溯源），跨租户访问物理阻断。

package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"email-aggregator-go/src/events"
	"email-aggregator-go/src/tenant"
)

// EmitAudit 发布审计事件到 events.TopicAudit。
//   - 强制 tenant.Resolve(ev.TenantID) 规范化租户 ID（空值回退默认租户）
//   - 强制 ev.Actor 非空（审计必须可溯源）
//   - 事件 Key 遵循 ADR-009 tenantId:accountId:mailId 格式（此处用 tenantId:actor:action:ts）
//   - bus.Publish 失败仅记日志（不阻塞业务路径，ADR-009 审计失败不应击穿业务可用性）
func EmitAudit(ctx context.Context, bus events.EventBus, ev events.AuditEvent) error {
	// ADR-009 硬约束：Actor 必须非空（审计可溯源）
	if ev.Actor == "" {
		return fmt.Errorf("audit: actor required (ADR-009 traceability)")
	}
	// 规范化租户 ID
	ev.TenantID = tenant.Resolve(ev.TenantID)
	// 时间戳兜底
	if ev.At == 0 {
		ev.At = time.Now().Unix()
	}
	// 序列化
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("audit: marshal: %w", err)
	}
	// Key 格式：tenantId:actor:action:ts（ADR-009 幂等去重键派生）
	key := fmt.Sprintf("%s:%s:%s:%d", ev.TenantID, ev.Actor, ev.Action, ev.At)
	// 发布
	if err := bus.Publish(ctx, events.TopicAudit, key, payload); err != nil {
		// 审计失败不阻塞业务，仅返回错误供上层决定是否重试
		return fmt.Errorf("audit: publish: %w", err)
	}
	return nil
}

// EmitAllow 发布鉴权通过事件（可选，用于全链路审计覆盖）。
func EmitAllow(ctx context.Context, bus events.EventBus, tid, actor, action, target string) error {
	return EmitAudit(ctx, bus, events.AuditEvent{
		TenantID: tid,
		Actor:    actor,
		Action:   "auth.allow:" + action,
		Target:   target,
		At:       time.Now().Unix(),
	})
}
