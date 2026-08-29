package notify

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"email-aggregator-go/src/tenant"
)

// TestHubPush 验证零依赖 WS Hub：握手返回 101 + Sec-WebSocket-Accept，
// 且 Notifier.Publish 能通过 WS 将 new-mail 通知以文本帧推送到客户端。
func TestHubPush(t *testing.T) {
	n := NewInMemoryNotifier()
	hub := NewHub(n)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.Upgrade(w, r, "acc")
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// 构造 WS 握手请求
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	req := "GET / HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	// 读取 101 响应
	buf := make([]byte, 512)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n1, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read handshake: %v", err)
	}
	resp := string(buf[:n1])
	if !strings.Contains(resp, "101 Switching Protocols") {
		t.Fatalf("expected 101, got: %q", resp)
	}
	if !strings.Contains(resp, "Sec-WebSocket-Accept: ") {
		t.Fatalf("missing accept header: %q", resp)
	}

	// 发布通知，验证客户端收到文本帧（tenantID 随载荷透传）
	payload := NotificationPayload{Kind: KindNewMail, AccountID: "acc", Preview: "hi", TS: 1}
	go n.Publish(tenant.DefaultTenantID, "acc", payload)

	frame := make([]byte, 256)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n2, err := conn.Read(frame)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if n2 < 2 {
		t.Fatalf("frame too short: %d", n2)
	}
	if frame[0] != 0x81 {
		t.Fatalf("expected text frame opcode 0x81, got 0x%02x", frame[0])
	}
	length := int(frame[1] & 0x7F)
	body := string(frame[2 : 2+length])
	var got NotificationPayload
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got.Kind != KindNewMail || got.Preview != "hi" {
		t.Fatalf("unexpected payload: %+v", got)
	}
}
