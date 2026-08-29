package events

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestTypeOfTopic_Mapping(t *testing.T) {
	cases := map[string]string{
		TopicSyncTasks:     "SyncTask",
		TopicMailIngested:  "MailIngested",
		TopicMailIndex:     "MailIndex",
		TopicNotifications: "Notification",
		TopicAudit:         "Audit",
		"unknown-topic":    "Unknown",
	}
	for topic, want := range cases {
		if got := TypeOfTopic(topic); got != want {
			t.Errorf("TypeOfTopic(%q) = %q, want %q", topic, got, want)
		}
	}
}

func TestTopicDLQ(t *testing.T) {
	if got, want := TopicDLQ(TopicMailIngested), "mail-ingested-dlq"; got != want {
		t.Errorf("TopicDLQ = %q, want %q", got, want)
	}
}

func TestMemoryDedupeStore_SeenMark(t *testing.T) {
	s := NewMemoryDedupeStore(4, time.Hour)
	ctx := context.Background()

	if seen, _ := s.Seen(ctx, "k1"); seen {
		t.Fatal("k1 should be unseen initially")
	}
	if err := s.Mark(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	if seen, _ := s.Seen(ctx, "k1"); !seen {
		t.Fatal("k1 should be seen after Mark")
	}
}

func TestMemoryDedupeStore_Eviction(t *testing.T) {
	s := NewMemoryDedupeStore(2, time.Hour)
	ctx := context.Background()
	_ = s.Mark(ctx, "a")
	_ = s.Mark(ctx, "b")
	_ = s.Mark(ctx, "c") // 超出容量，淘汰最旧 "a"
	if seen, _ := s.Seen(ctx, "a"); seen {
		t.Fatal("a should be evicted")
	}
	if seen, _ := s.Seen(ctx, "b"); !seen {
		t.Fatal("b should remain")
	}
	if seen, _ := s.Seen(ctx, "c"); !seen {
		t.Fatal("c should remain")
	}
}

func TestMemoryDedupeStore_TTLExpiry(t *testing.T) {
	s := NewMemoryDedupeStore(0, 10*time.Millisecond) // 短 TTL
	ctx := context.Background()
	_ = s.Mark(ctx, "k")
	time.Sleep(30 * time.Millisecond)
	if seen, _ := s.Seen(ctx, "k"); seen {
		t.Fatal("expired key should be treated as unseen")
	}
}

func TestWithIdempotency_Deduplicates(t *testing.T) {
	s := NewMemoryDedupeStore(0, time.Hour)
	ctx := context.Background()
	var calls atomic.Int32
	handler := WithIdempotency(s, func(_ context.Context, env EventEnvelope) error {
		calls.Add(1)
		return nil
	})

	env := EventEnvelope{Key: "tenant:acct:mail1"}
	if err := handler(ctx, env); err != nil {
		t.Fatal(err)
	}
	if err := handler(ctx, env); err != nil { // 同 key 二次投递应跳过
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", calls.Load())
	}

	// 不同 key 应正常处理
	env2 := EventEnvelope{Key: "tenant:acct:mail2"}
	_ = handler(ctx, env2)
	if calls.Load() != 2 {
		t.Fatalf("handler calls = %d, want 2", calls.Load())
	}

	// 空 key 不做去重
	env3 := EventEnvelope{Key: ""}
	_ = handler(ctx, env3)
	_ = handler(ctx, env3)
	if calls.Load() != 4 {
		t.Fatalf("empty-key handler calls = %d, want 4", calls.Load())
	}
}

func TestWithIdempotency_FailedNotMarked(t *testing.T) {
	s := NewMemoryDedupeStore(0, time.Hour)
	ctx := context.Background()
	boom := errors.New("boom")
	handler := WithIdempotency(s, func(_ context.Context, env EventEnvelope) error {
		return boom
	})
	env := EventEnvelope{Key: "k"}
	if err := handler(ctx, env); err != boom {
		t.Fatalf("want boom, got %v", err)
	}
	// 失败不标记 → 重试（同 key 重投）仍会再次执行
	var calls atomic.Int32
	handler2 := WithIdempotency(s, func(_ context.Context, _ EventEnvelope) error {
		calls.Add(1)
		return nil
	})
	_ = handler2(ctx, env)
	if calls.Load() != 1 {
		t.Fatalf("after failure, retry should execute; calls=%d", calls.Load())
	}
}

func TestRetry_SuccessOnSecondAttempt(t *testing.T) {
	ctx := context.Background()
	var attempts atomic.Int32
	err := Retry(ctx, RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}, func() error {
		if attempts.Add(1) == 1 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
}

func TestRetry_Exhausts(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("persistent")
	err := Retry(ctx, RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}, func() error {
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
}

func TestRetrying_Wrapper(t *testing.T) {
	ctx := context.Background()
	var attempts atomic.Int32
	h := Retrying(RetryConfig{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
		func(_ context.Context, env EventEnvelope) error {
			if attempts.Add(1) == 1 {
				return errors.New("transient")
			}
			return nil
		})
	if err := h(ctx, EventEnvelope{Key: "k"}); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}
