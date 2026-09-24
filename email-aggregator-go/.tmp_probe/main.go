package main

import (
	"context"
	"fmt"
	"os"
	"time"

	kafkago "github.com/segmentio/kafka-go"
)

func main() {
	// 方式A：默认 Writer（系统拨号，无自定义 Transport）
	w := &kafkago.Writer{Addr: kafkago.TCP("127.0.0.1:9092")}
	ctx := context.Background()
	_ = w.WriteMessages(ctx, kafkago.Message{Topic: "mail-ingested", Key: []byte("w1"), Value: []byte(`{}`)})
	n := 5
	st := time.Now()
	for i := 0; i < n; i++ {
		_ = w.WriteMessages(ctx, kafkago.Message{Topic: "mail-ingested", Key: []byte(fmt.Sprintf("ra:%d", i)), Value: []byte(`{}`)})
	}
	fmt.Printf("default-writer: %d msgs %s -> %.0f ms/msg\n", n, time.Since(st), float64(time.Since(st).Milliseconds())/float64(n))

	// 方式B：显式 RequireOne + BatchTimeout
	w2 := &kafkago.Writer{
		Addr:         kafkago.TCP("127.0.0.1:9092"),
		RequiredAcks: kafkago.RequireOne,
		BatchTimeout: 100 * time.Millisecond,
	}
	_ = w2.WriteMessages(ctx, kafkago.Message{Topic: "mail-ingested", Key: []byte("w2"), Value: []byte(`{}`)})
	st2 := time.Now()
	for i := 0; i < n; i++ {
		_ = w2.WriteMessages(ctx, kafkago.Message{Topic: "mail-ingested", Key: []byte(fmt.Sprintf("rb:%d", i)), Value: []byte(`{}`)})
	}
	fmt.Printf("reqone+bt100ms: %d msgs %s -> %.0f ms/msg\n", n, time.Since(st2), float64(time.Since(st2).Milliseconds())/float64(n))

	// 方式C：Async 模式
	w3 := &kafkago.Writer{Addr: kafkago.TCP("127.0.0.1:9092"), Async: true, RequiredAcks: kafkago.RequireOne}
	st3 := time.Now()
	for i := 0; i < n; i++ {
		_ = w3.WriteMessages(ctx, kafkago.Message{Topic: "mail-ingested", Key: []byte(fmt.Sprintf("rc:%d", i)), Value: []byte(`{}`)})
	}
	fmt.Printf("async: %d msgs %s -> %.0f ms/msg (入队即返)\n", n, time.Since(st3), float64(time.Since(st3).Milliseconds())/float64(n))
	os.Exit(0)
}
