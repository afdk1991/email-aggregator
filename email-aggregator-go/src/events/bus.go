package events

import (
	"context"
	"time"
)

// EventBus 消息总线接口（at-least-once 语义，消费端自行幂等）
type EventBus interface {
	Publish(ctx context.Context, topic, key string, payload []byte) error
	Subscribe(ctx context.Context, topic, group string, handler func(ctx context.Context, env EventEnvelope) error) error
	Close() error
}

// InMemoryBus 进程内实现（PoC / 单测 / 本地无外部依赖跑通）
type InMemoryBus struct {
	subs map[string][]func(ctx context.Context, env EventEnvelope) error
}

// NewInMemoryBus 构造
func NewInMemoryBus() *InMemoryBus {
	return &InMemoryBus{subs: map[string][]func(ctx context.Context, env EventEnvelope) error{}}
}

// Publish 同步 fan-out 给订阅者（演示用；生产由 Kafka 分区保证顺序与重放）
func (b *InMemoryBus) Publish(ctx context.Context, topic, key string, payload []byte) error {
	env := EventEnvelope{Topic: topic, Key: key, TS: time.Now().UnixMilli(), Type: TypeOfTopic(topic), Payload: payload}
	for _, h := range b.subs[topic] {
		if err := h(ctx, env); err != nil {
			return err
		}
	}
	return nil
}

// Subscribe 注册处理器（同主题可多 group 消费）
func (b *InMemoryBus) Subscribe(_ context.Context, topic, _ string, handler func(ctx context.Context, env EventEnvelope) error) error {
	b.subs[topic] = append(b.subs[topic], handler)
	return nil
}

// Close 释放
func (b *InMemoryBus) Close() error { return nil }

// 真实 Kafka 适配器见 integration/integration.go 中的 KafkaAdapter（//go:build integration）。
// 接口与 InMemoryBus 完全一致，替换时无需改动编排/采集逻辑。
