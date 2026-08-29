// Package ingest 实现事件驱动的"邮件摄取消费侧"（深化文档 §13 时序的消费端）。
//
// 职责：订阅 mail-ingested，对每封邮件完成
//  1. 元数据落库（MetadataStore，幂等去重）
//  2. 正文内容寻址落对象存储（ContentStore）
//  3. 全文/字段索引（SearchIndex）
//  4. 发布 mail-index（供其他消费者 / 审计）
//  5. 实时通知（notifications：new-mail）
//  6. 审计事件（audit）
//
// 该组件只依赖既有接口（events.EventBus / store.* / search.SearchIndex / notify.Notifier），
// 与具体中间件无关——InMemory 与真实 Kafka/PG 实现可无缝替换（ADR-002/ADR-003）。
package ingest

import (
	"context"
	"encoding/json"
	"time"

	"email-aggregator-go/src/events"
	"email-aggregator-go/src/model"
	"email-aggregator-go/src/notify"
	"email-aggregator-go/src/search"
	"email-aggregator-go/src/store"
	"email-aggregator-go/src/tenant"
)

// IngestWorker 邮件摄取消费者。
type IngestWorker struct {
	bus      events.EventBus
	metadata store.MetadataStore
	content  store.ContentStore
	index    search.SearchIndex
	notifier notify.Notifier
}

// NewIngestWorker 装配（依赖注入，便于替换真实适配器 / 单测）。
func NewIngestWorker(
	bus events.EventBus,
	metadata store.MetadataStore,
	content store.ContentStore,
	index search.SearchIndex,
	notifier notify.Notifier,
) *IngestWorker {
	return &IngestWorker{bus: bus, metadata: metadata, content: content, index: index, notifier: notifier}
}

// Start 订阅 mail-ingested 并启动消费（随 ctx 取消而停止）。
func (w *IngestWorker) Start(ctx context.Context) error {
	return w.bus.Subscribe(ctx, events.TopicMailIngested, "ingest", w.handle)
}

// handle 单封邮件的摄取逻辑；返回 error 会被总线透传（真实环境接 DLQ）。
//
// ADR-009 多租户：TenantID 从 mail-ingested 事件载荷透传（contracts.go 已补字段，
// omitempty 向后兼容旧事件）。此处用 tenant.Resolve 规范化，旧事件缺 TenantID
// 时回退 DefaultTenantID，保证单租户场景与改造前行为完全一致（可逆）。
// tid 贯穿 落库 / 索引 / 通知 / mail-index / audit 全链路，构成数据面隔离边界。
func (w *IngestWorker) handle(ctx context.Context, env events.EventEnvelope) error {
	var ev events.MailIngestedEvent
	if err := json.Unmarshal(env.Payload, &ev); err != nil {
		return err
	}

	// ADR-009：从事件透传 TenantID，旧事件缺省回退 DefaultTenantID
	tid := tenant.Resolve(ev.TenantID)

	// 由事件重建 CanonicalMail（事件已携带索引所需字段）。
	mail := model.CanonicalMail{
		TenantID:     tid,
		ID:           ev.MailID,
		AccountID:    ev.AccountID,
		Folder:       ev.Folder,
		From:         model.Address{Email: ev.From},
		Subject:      ev.Subject,
		BodyText:     ev.BodyText,
		InternalDate: ev.InternalDate,
		SizeBytes:    ev.SizeBytes,
		HasAttachment: ev.HasAttachment,
		RawObjectKey: ev.RawObjectKey,
	}

	// 1) 元数据落库（幂等：同 (tid, ID) 重复摄取不新增）
	if err := w.metadata.UpsertMail(tid, mail); err != nil {
		return err
	}

	// 2) 正文内容寻址落对象存储（相同正文只存一份）
	if _, err := w.content.Put([]byte(ev.BodyText)); err != nil {
		return err
	}

	// 3) 索引（按 tid 隔离命名空间）
	if err := w.index.Index(tid, mail); err != nil {
		return err
	}

	// 4) 发布 mail-index（供分析 / 下游消费者）
	// ADR-009：携带 TenantID；分区键用 tid（同租户事件落同分区，保证消费顺序）
	idxEv := events.MailIndexEvent{
		TenantID:  tid,
		AccountID: ev.AccountID,
		MailID:    ev.MailID,
		IndexedAt: nowMs(),
	}
	if b, err := json.Marshal(idxEv); err == nil {
		_ = w.bus.Publish(ctx, events.TopicMailIndex, tid, b)
	}

	// 5) 实时通知（new-mail → WS 推送，按 tid 路由到该租户的连接）
	w.notifier.Publish(tid, ev.AccountID, notify.NotificationPayload{
		Kind:      notify.KindNewMail,
		AccountID: ev.AccountID,
		Preview:   ev.Subject,
		TS:        nowMs(),
	})

	// 6) 审计（按 tid 分区，满足数据驻留合规约束）
	audit := events.AuditEvent{
		TenantID: tid,
		Actor:    "ingest-worker",
		Action:   "mail-ingested",
		Target:   ev.AccountID + "/" + ev.MailID,
		At:       nowMs(),
	}
	if b, err := json.Marshal(audit); err == nil {
		_ = w.bus.Publish(ctx, events.TopicAudit, tid, b)
	}

	return nil
}

func nowMs() int64 { return time.Now().UnixMilli() }
