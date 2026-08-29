package events

import (
	"context"
	"sync"
	"testing"
)

// InMemoryBus 发布后被订阅者收到（fan-out）
func TestInMemoryBus_PublishFanout(t *testing.T) {
	bus := NewInMemoryBus()
	var mu sync.Mutex
	var got []string
	_ = bus.Subscribe(context.Background(), TopicMailIngested, "g1", func(_ context.Context, env EventEnvelope) error {
		mu.Lock()
		got = append(got, env.Key)
		mu.Unlock()
		return nil
	})
	if err := bus.Publish(context.Background(), TopicMailIngested, "acc1:m1", []byte(`{}`)); err != nil {
		t.Fatalf("publish error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "acc1:m1" {
		t.Fatalf("expected 1 receive with key acc1:m1, got %v", got)
	}
}

// 幂等键透传：消费端可据 Key 去重
func TestInMemoryBus_IdempotencyKey(t *testing.T) {
	bus := NewInMemoryBus()
	const key = "acc1:m2"
	var seen string
	_ = bus.Subscribe(context.Background(), TopicMailIngested, "g1", func(_ context.Context, env EventEnvelope) error {
		seen = env.Key
		return nil
	})
	_ = bus.Publish(context.Background(), TopicMailIngested, key, []byte(`{"x":1}`))
	if seen != key {
		t.Fatalf("expected idempotency key %q, got %q", key, seen)
	}
}
