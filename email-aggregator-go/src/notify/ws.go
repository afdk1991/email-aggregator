// Package notify 通知层：把 notifications 事件推送给订阅了某账户的客户端。
// InMemoryNotifier 用于演示/单测；WsNotifier 为真实 WebSocket 推送（RFC6455）。
package notify

import (
	"encoding/json"
	"sync"

	"email-aggregator-go/src/tenant"
)

// NotifyKind 通知类型
type NotifyKind string

const (
	KindNewMail      NotifyKind = "new-mail"
	KindSyncState    NotifyKind = "sync-state"
	KindError        NotifyKind = "error"
	KindMailUpdated  NotifyKind = "mail-updated"
	KindMailDeleted  NotifyKind = "mail-deleted"
)

// NotificationPayload 推送载荷
type NotificationPayload struct {
	Kind      NotifyKind `json:"kind"`
	TenantID  string     `json:"tenantId,omitempty"` // ADR-009 多租户：通知归属租户（随发布链路透传）
	AccountID string     `json:"accountId"`
	Preview   string     `json:"preview,omitempty"`
	TS        int64      `json:"ts"`
	MailID    string     `json:"mailId,omitempty"` // 关联的邮件 ID（更新/删除事件）
	Read      bool       `json:"read,omitempty"`   // 更新后的已读状态
}

// PushSink 推送目标（WS 连接 / 测试回调）
type PushSink interface {
	Send(payload NotificationPayload)
	Close()
}

// Notifier 通知接口
type Notifier interface {
	AddSink(accountID string, sink PushSink)
	RemoveSink(accountID string, sink PushSink)
	// Publish 向某账户的订阅者广播；tenantID 随载荷透传（ADR-009 全链路）。
	Publish(tenantID, accountID string, payload NotificationPayload)
}

// InMemoryNotifier 演示/单测用
type InMemoryNotifier struct {
	mu       sync.RWMutex
	sinks    map[string]map[PushSink]struct{}
	received []NotificationPayload
}

// NewInMemoryNotifier 构造
func NewInMemoryNotifier() *InMemoryNotifier {
	return &InMemoryNotifier{sinks: map[string]map[PushSink]struct{}{}}
}

// AddSink 注册
func (n *InMemoryNotifier) AddSink(accountID string, sink PushSink) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.sinks[accountID] == nil {
		n.sinks[accountID] = map[PushSink]struct{}{}
	}
	n.sinks[accountID][sink] = struct{}{}
}

// RemoveSink 注销
func (n *InMemoryNotifier) RemoveSink(accountID string, sink PushSink) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.sinks[accountID], sink)
}

// Publish 广播给某账户的所有订阅者（tenantID 写入载荷，供观测/隔离）
func (n *InMemoryNotifier) Publish(tenantID, accountID string, payload NotificationPayload) {
	n.mu.Lock()
	defer n.mu.Unlock()
	payload.TenantID = tenant.Resolve(tenantID)
	n.received = append(n.received, payload)
	for s := range n.sinks[accountID] {
		s.Send(payload)
	}
}

// Received 演示断言用：已收到的通知列表
func (n *InMemoryNotifier) Received() []NotificationPayload {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return append([]NotificationPayload{}, n.received...)
}

// funcSink 用闭包实现 PushSink（单测/演示）
type funcSink struct {
	send func(NotificationPayload)
}

func (f *funcSink) Send(p NotificationPayload) { f.send(p) }
func (f *funcSink) Close()                     {}

// NewFuncSink 便捷构造（回调式 sink，用于单测断言）
func NewFuncSink(fn func(NotificationPayload)) PushSink {
	return &funcSink{send: fn}
}

// encodeFrame 封装单帧（服务端→客户端，不掩码）。opcode 为帧类型（0x1=text, 0x8=close, 0xA=pong…）。
// 长度 < 126：2 字节头；< 65536：4 字节；否则 10 字节（与 TS 版对齐）。
func encodeFrame(opcode byte, data []byte) []byte {
	length := len(data)
	var header []byte
	switch {
	case length < 126:
		header = []byte{0x80 | opcode, byte(length)}
	case length < 65536:
		header = []byte{0x80 | opcode, 126, byte(length >> 8), byte(length)}
	default:
		header = []byte{0x80 | opcode, 127, 0, 0, 0, 0, byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length)}
	}
	return append(header, data...)
}

// EncodeTextFrame 导出文本帧封装，供 Hub 推送新邮件通知使用。
func EncodeTextFrame(text string) []byte {
	return encodeFrame(0x1, []byte(text))
}

// MarshalPayload 便捷序列化
func MarshalPayload(p NotificationPayload) ([]byte, error) {
	return json.Marshal(p)
}
