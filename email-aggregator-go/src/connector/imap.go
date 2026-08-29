package connector

import (
	"context"
	"fmt"
	"sync"

	"email-aggregator-go/src/model"
)

// InMemoryMailServer 演示用内存邮件源（模拟 IMAP 服务端，用于本地无依赖跑通）
type InMemoryMailServer struct {
	mu    sync.RWMutex
	mails []model.CanonicalMail
}

// NewInMemoryMailServer 构造
func NewInMemoryMailServer() *InMemoryMailServer {
	return &InMemoryMailServer{}
}

// Add 注入一封邮件（模拟服务端收到新邮件 / 供测试）。并发安全。
func (s *InMemoryMailServer) Add(m model.CanonicalMail) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mails = append(s.mails, m)
}

// snapshot 返回当前邮件的副本（读锁保护，供采集方法使用）
func (s *InMemoryMailServer) snapshot() []model.CanonicalMail {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.CanonicalMail, len(s.mails))
	copy(out, s.mails)
	return out
}

// IMAPConnector IMAP 适配器骨架。
// 真实环境：用 github.com/linode/go-imap 等库实现网络层，本骨架保持接口契约一致。
type IMAPConnector struct {
	server *InMemoryMailServer
	cap    model.ConnectorCapabilities
}

// NewIMAPConnector 构造（注入内存服务器用于演示）
func NewIMAPConnector(server *InMemoryMailServer) *IMAPConnector {
	return &IMAPConnector{
		server: server,
		cap: model.ConnectorCapabilities{
			Provider:            model.ProviderIMAP,
			SupportsIncremental: true,
			SupportsIdle:        true,
			SupportsQresync:     true,
			SupportsCondstore:   true,
			SupportsOAuth:       true,
		},
	}
}

// Connect 建立连接（真实环境：IMAP LOGIN / OAUTH2 XOAUTH2）
func (c *IMAPConnector) Connect(_ context.Context, _ model.Credential) error {
	return nil
}

// Capabilities 返回能力声明
func (c *IMAPConnector) Capabilities() model.ConnectorCapabilities { return c.cap }

// InitialFullSync 初始全量（真实环境：UID FETCH 1:* 分页）
func (c *IMAPConnector) InitialFullSync(_ context.Context, since int64, sink model.SyncSink) error {
	for _, m := range c.server.snapshot() {
		if m.InternalDate >= since {
			if err := sink.OnMessage(context.Background(), m); err != nil {
				return err
			}
		}
	}
	return nil
}

// IncrementalSync 增量（真实环境：UID FETCH uidnext:* 或 QRESYNC）
func (c *IMAPConnector) IncrementalSync(_ context.Context, cursor model.SyncCursor, sink model.SyncSink) error {
	for _, m := range c.server.snapshot() {
		if m.Cursor.LastUID > cursor.LastUID {
			if err := sink.OnMessage(context.Background(), m); err != nil {
				return err
			}
		}
	}
	return nil
}

// StreamChanges 长连接推送（真实环境：IMAP IDLE + EXISTS 响应）
func (c *IMAPConnector) StreamChanges(ctx context.Context, _ model.SyncCursor, _ func(ctx context.Context, m model.CanonicalMail) error) error {
	fmt.Println("[IMAPConnector:stub] IDLE 监听中（演示环境无真实连接）")
	<-ctx.Done()
	return nil
}

// Close 释放连接
func (c *IMAPConnector) Close() error { return nil }
