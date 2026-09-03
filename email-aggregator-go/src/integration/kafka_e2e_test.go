//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"email-aggregator-go/src/events"
)

// 真实 Kafka e2e（需本地 deploy-kafka 容器运行，broker 见 KAFKA_BROKERS）。
// 运行方式：go test -tags integration ./src/integration/ -run KafkaAdapter_RealE2E -v
// 主题名带时间戳避免跨次运行残留数据污染；消费组各自唯一避免消费到历史 offset。

const kafkaBroker = "127.0.0.1:9092"

func kafkaUnique(name string) string {
	return fmt.Sprintf("%s-%d", name, time.Now().UnixNano())
}

// kafkaEnsureTopic 尽力创建主题：broker 已开 auto-create（KAFKA_AUTO_CREATE_TOPICS_ENABLE=true），
// CreateTopics 会因 controller 通告为容器名 kafka:9093（宿主机不可解析）而 EOF——可忽略，
// 主题交由 auto-create 在首次元数据请求时延迟创建（kafkaWaitTopicMetadata 负责等待就绪）。
func kafkaEnsureTopic(t *testing.T, topic string) {
	t.Helper()
	conn, err := kafkago.Dial("tcp", kafkaBroker)
	if err != nil {
		t.Fatalf("dial kafka: %v", err)
	}
	defer conn.Close()
	_ = conn.CreateTopics(kafkago.TopicConfig{Topic: topic, NumPartitions: 1, ReplicationFactor: 1})
}

// kafkaWaitTopicMetadata 轮询等待主题元数据可见（ReadPartitions 会触发 broker auto-create；
// 创建后 partition leader 就绪才返回），避免发布竞态。
func kafkaWaitTopicMetadata(t *testing.T, topic string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := kafkago.Dial("tcp", kafkaBroker)
		if err == nil {
			parts, perr := conn.ReadPartitions(topic)
			conn.Close()
			if perr == nil && len(parts) > 0 {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("topic %s metadata not ready within %v", topic, timeout)
}

// kafkaPublish 发布并容忍 auto-create 延迟：首投报 "Unknown Topic Or Partition" 时退避重试。
func kafkaPublish(t *testing.T, adapter *KafkaAdapter, ctx context.Context, topic, key string, payload []byte) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := adapter.Publish(ctx, topic, key, payload); err == nil {
			return
		} else {
			lastErr = err
			if !strings.Contains(err.Error(), "Unknown Topic Or Partition") {
				t.Fatalf("publish: %v", err)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("publish timeout after retries: %v", lastErr)
}

// kafkaDialOverride 重写拨号目标：broker 把 advertised listener 通告为容器名
// deploy-kafka-1:9092（供 docker 内部 mixmlaal 业务链解析），宿主机需改写为 127.0.0.1:9092。
// 这验证 KafkaAdapter.WithDial 生产场景（advertised 与客户端网络不一致）。
func kafkaDialOverride() func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, kafkaBroker)
	}
}

// TestKafkaAdapter_RealE2E_RoundTrip 验证：发布 → 消费 全链路，
// 校验 key / payload 原样往返、Type 由主题推导（mail-ingested → MailIngested）。
func TestKafkaAdapter_RealE2E_RoundTrip(t *testing.T) {
	topic := events.TopicMailIngested // 用规范主题验证 Type 映射
	kafkaEnsureTopic(t, topic)
	// 等待主题元数据/分区 leader 就绪（对齐 DLQ 测试），避免长生命周期 broker 上
	// 主题被删除重建或刚创建时 leader 未稳定，消费者入组前发布导致丢消息（偶发超时）。
	kafkaWaitTopicMetadata(t, topic, 8*time.Second)
	group := kafkaUnique("e2e-roundtrip-grp")
	adapter := NewKafkaAdapter([]string{kafkaBroker}).WithDial(kafkaDialOverride())
	defer adapter.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	got := make(chan events.EventEnvelope, 8)
	if err := adapter.Subscribe(ctx, topic, group, func(_ context.Context, env events.EventEnvelope) error {
		got <- env
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	time.Sleep(800 * time.Millisecond) // 等待消费者加入消费组

	payload := []byte(`{"tenantId":"t1","accountId":"a1","mailId":"m1","subject":"e2e"}`)
	kafkaPublish(t, adapter, ctx, topic, "t1:a1:m1", payload)

	// 循环消费直到命中本测试消息（跳过历史积压/其他生产者消息），
	// 使共享规范主题 mail-ingested 在长生命周期 broker 上也能稳定往返。
	for {
		select {
		case env := <-got:
			if env.Key != "t1:a1:m1" {
				continue // 非本测试消息（历史积压或运行中服务生产），跳过
			}
			if string(env.Payload) != string(payload) {
				t.Fatalf("payload mismatch: %s vs %s", env.Payload, payload)
			}
			if env.Type != "MailIngested" {
				t.Fatalf("Type = %q, want MailIngested", env.Type)
			}
			if env.Topic != topic {
				t.Fatalf("Topic = %q", env.Topic)
			}
			goto roundTripOK
		case <-ctx.Done():
			t.Fatal("timeout waiting for round-trip message")
		}
	}
roundTripOK:
}

// TestKafkaAdapter_RealE2E_DLQ 验证：handler 持续失败 → 有界重试耗尽 → 消息转投 <topic>-dlq，
// DLQ 值为信封 JSON（携带原始 payload，供重放/审计）。
func TestKafkaAdapter_RealE2E_DLQ(t *testing.T) {
	topic := kafkaUnique("e2e-dlq")
	dlqTopic := events.TopicDLQ(topic)
	kafkaEnsureTopic(t, topic)
	kafkaEnsureTopic(t, dlqTopic)
	kafkaWaitTopicMetadata(t, topic, 8*time.Second)
	kafkaWaitTopicMetadata(t, dlqTopic, 8*time.Second)
	group := kafkaUnique("e2e-dlq-grp")
	adapter := NewKafkaAdapter([]string{kafkaBroker}).WithDial(kafkaDialOverride())
	defer adapter.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	boom := errors.New("poison-pill")
	// 主消费者：永远失败
	if err := adapter.Subscribe(ctx, topic, group, func(_ context.Context, _ events.EventEnvelope) error {
		return boom
	}); err != nil {
		t.Fatalf("subscribe main: %v", err)
	}
	// DLQ 消费者：收集转投的消息
	dlqGot := make(chan events.EventEnvelope, 8)
	if err := adapter.Subscribe(ctx, dlqTopic, kafkaUnique("e2e-dlq-grp2"), func(_ context.Context, env events.EventEnvelope) error {
		dlqGot <- env
		return nil
	}); err != nil {
		t.Fatalf("subscribe dlq: %v", err)
	}
	time.Sleep(1000 * time.Millisecond) // 等两组消费者就绪

	kafkaPublish(t, adapter, ctx, topic, "k-dlq-1", []byte(`{"v":1}`))

	select {
	case env := <-dlqGot:
		var back events.EventEnvelope
		if err := json.Unmarshal(env.Payload, &back); err != nil {
			t.Fatalf("dlq payload should be envelope JSON: %v", err)
		}
		if back.Key != "k-dlq-1" {
			t.Fatalf("dlq inner key = %q", back.Key)
		}
		if back.Topic != topic {
			t.Fatalf("dlq inner topic = %q, want %q", back.Topic, topic)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for dlq message (retry+DLQ path broken)")
	}
}

// TestKafkaAdapter_RealE2E_IdempotentConsume 验证：WithIdempotency 在真实 Kafka 上
// 对同 key 事件去重（恰好一次处理）；以不同 key 哨兵确认两条同 key 消息均已消费。
func TestKafkaAdapter_RealE2E_IdempotentConsume(t *testing.T) {
	topic := kafkaUnique("e2e-idem")
	kafkaEnsureTopic(t, topic)
	kafkaWaitTopicMetadata(t, topic, 8*time.Second)
	group := kafkaUnique("e2e-idem-grp")
	adapter := NewKafkaAdapter([]string{kafkaBroker}).WithDial(kafkaDialOverride())
	defer adapter.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var processed atomic.Int32
	store := events.NewMemoryDedupeStore(0, time.Hour)
	handler := events.WithIdempotency(store, func(_ context.Context, _ events.EventEnvelope) error {
		processed.Add(1)
		return nil
	})
	if err := adapter.Subscribe(ctx, topic, group, handler); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	time.Sleep(800 * time.Millisecond)

	// 同 key 发两条 + 不同 key 哨兵
	kafkaPublish(t, adapter, ctx, topic, "same-key", []byte(`1`))
	kafkaPublish(t, adapter, ctx, topic, "same-key", []byte(`2`))
	kafkaPublish(t, adapter, ctx, topic, "sentinel", []byte(`s`))

	// 等哨兵被处理（此时同 key 两条也已消费完，但只应处理一次）
	deadline := time.Now().Add(20 * time.Second)
	for processed.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := processed.Load(); got != 2 {
		t.Fatalf("processed = %d, want 2（同 key 去重失败）", got)
	}
}
