// 事件总线演进层（ADR-002 契约增强）：类型化信封、幂等消费、有界重试。
//
// 设计取舍：
//   - 不改动 EventBus 接口（Publish/Subscribe/Close 保持稳定），演进以"包装器"
//     （WithIdempotency / Retry）与派生函数（TypeOfTopic / TopicDLQ）叠加，
//     消费端按需组合，替换总线实现零改动。
//   - 幂等去重以 Envelope.Key 为键（tenantId:accountId:mailId），与既有契约一致；
//     MemoryDedupeStore 是有界 LRU 近似（TTL + 容量上限），生产可换 PG/Redis 实现。
//   - 重试采用指数退避；最终失败由 KafkaAdapter 转投 <topic>-dlq（详见 integration 包）。
package events

import (
	"context"
	"sync"
	"time"
)

// topicTypes 主题 → 事件类型名映射（ADR-002 五主题契约的单一事实源）。
var topicTypes = map[string]string{
	TopicSyncTasks:     "SyncTask",
	TopicMailIngested:  "MailIngested",
	TopicMailIndex:     "MailIndex",
	TopicNotifications: "Notification",
	TopicAudit:         "Audit",
}

// TypeOfTopic 返回主题对应的事件类型名（未知主题回退 "Unknown"，保证消费端可安全 switch）。
func TypeOfTopic(topic string) string {
	if t, ok := topicTypes[topic]; ok {
		return t
	}
	return "Unknown"
}

// TopicDLQ 返回主题对应的 DLQ 主题名（<topic>-dlq）。
func TopicDLQ(topic string) string { return topic + "-dlq" }

// Handler 事件处理函数签名（与 EventBus.Subscribe 参数一致，便于组合）。
type Handler func(ctx context.Context, env EventEnvelope) error

// DedupeStore 幂等去重存储（消费端按 Envelope.Key 去重）。
type DedupeStore interface {
	// Seen 判断 key 是否已处理；返回 true 表示应跳过。
	Seen(ctx context.Context, key string) (bool, error)
	// Mark 标记 key 已处理。
	Mark(ctx context.Context, key string) error
}

// MemoryDedupeStore 进程内去重：有界容量 + TTL，超出后按最久未用淘汰。
// 足够支撑单实例幂等；多实例水平扩展需换共享存储（PG/Redis）实现同一接口。
type MemoryDedupeStore struct {
	mu     sync.Mutex
	seen   map[string]time.Time
	order  []string // 按 Mark 时间排序的 key 队列（队首最旧）
	max    int
	ttl    time.Duration
}

// NewMemoryDedupeStore 构造（max<=0 默认 65536，ttl<=0 默认 24h）。
func NewMemoryDedupeStore(max int, ttl time.Duration) *MemoryDedupeStore {
	if max <= 0 {
		max = 65536
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &MemoryDedupeStore{seen: map[string]time.Time{}, max: max, ttl: ttl}
}

// Seen 判断是否已处理；同时惰性清理过期 key。
func (d *MemoryDedupeStore) Seen(ctx context.Context, key string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if t, ok := d.seen[key]; ok {
		if now.Sub(t) <= d.ttl {
			return true, nil
		}
		delete(d.seen, key) // 过期即视为未处理
	}
	return false, nil
}

// Mark 记录 key 已处理，超容量时淘汰最旧的 key。
func (d *MemoryDedupeStore) Mark(ctx context.Context, key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[key]; !ok {
		d.order = append(d.order, key)
	}
	d.seen[key] = time.Now()
	for len(d.order) > d.max {
		old := d.order[0]
		d.order = d.order[1:]
		delete(d.seen, old)
	}
	return nil
}

// WithIdempotency 包装 handler：已处理的 key 直接跳过（至少一次语义下以去重保证"恰好一次处理"）。
// 查重失败一律跳过（err 返回 nil）：宁可漏处理一次，也不重复触发副作用；
// 生产环境可用重试兜底（见 Retry）。
func WithIdempotency(store DedupeStore, next Handler) Handler {
	return func(ctx context.Context, env EventEnvelope) error {
		if env.Key == "" {
			return next(ctx, env) // 无 key 的事件不做去重（如广播类）
		}
		seen, err := store.Seen(ctx, env.Key)
		if err != nil || seen {
			return nil
		}
		if err := next(ctx, env); err != nil {
			return err
		}
		return store.Mark(ctx, env.Key)
	}
}

// RetryConfig 有界重试配置。
type RetryConfig struct {
	MaxAttempts int           // 总尝试次数（含首次），<=0 默认 3
	BaseDelay   time.Duration // 初始退避，<=0 默认 200ms
	MaxDelay    time.Duration // 退避上限，<=0 默认 2s
}

// DefaultRetryConfig 默认重试配置（3 次 / 200ms 起 / 封顶 2s）。
var DefaultRetryConfig = RetryConfig{MaxAttempts: 3, BaseDelay: 200 * time.Millisecond, MaxDelay: 2 * time.Second}

// Retry 执行 fn 直到成功或耗尽 MaxAttempts；指数退避，ctx 取消立即返回。
// 返回最后一次错误；成功返回 nil。适用于 at-least-once 语义下对消费处理的重试。
func Retry(ctx context.Context, cfg RetryConfig, fn func() error) error {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultRetryConfig.MaxAttempts
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = DefaultRetryConfig.BaseDelay
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = DefaultRetryConfig.MaxDelay
	}
	var lastErr error
	delay := cfg.BaseDelay
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if attempt == cfg.MaxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay *= 2; delay > cfg.MaxDelay {
			delay = cfg.MaxDelay
		}
	}
	return lastErr
}

// Retrying 包装 handler：内部用 Retry 有界重试，耗尽后返回最终错误
// （由上层——如 KafkaAdapter——决定转 DLQ）。
func Retrying(cfg RetryConfig, next Handler) Handler {
	return func(ctx context.Context, env EventEnvelope) error {
		return Retry(ctx, cfg, func() error { return next(ctx, env) })
	}
}
