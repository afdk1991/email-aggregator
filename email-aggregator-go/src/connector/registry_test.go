package connector

import (
	"context"
	"testing"

	"email-aggregator-go/src/model"
)

// 注册表：注册后可创建对应适配器；未注册返回错误
func TestRegistry_RegisterAndCreate(t *testing.T) {
	reg := NewConnectorRegistry()
	reg.Register(model.ProviderIMAP, func(_ model.Provider, _ map[string]string) (model.Connector, error) {
		return NewIMAPConnector(NewInMemoryMailServer()), nil
	})
	c, err := reg.Create(model.ProviderIMAP, nil)
	if err != nil {
		t.Fatalf("create imap error: %v", err)
	}
	if c.Capabilities().Provider != model.ProviderIMAP {
		t.Fatalf("unexpected provider capability")
	}
	if _, err := reg.Create(model.ProviderPOP3, nil); err == nil {
		t.Fatalf("expected error for unregistered provider")
	}
}

// IMAP 适配器全量同步经 sink 回传邮件
func TestIMAPConnector_InitialFullSync(t *testing.T) {
	server := NewInMemoryMailServer()
	server.Add(model.CanonicalMail{ID: "m1", AccountID: "acc1", Folder: "INBOX", Subject: "A", InternalDate: 100, Cursor: model.SyncCursor{LastUID: 1}})
	server.Add(model.CanonicalMail{ID: "m2", AccountID: "acc1", Folder: "INBOX", Subject: "B", InternalDate: 200, Cursor: model.SyncCursor{LastUID: 2}})

	c := NewIMAPConnector(server)
	if err := c.Connect(context.Background(), model.Credential{Username: "u", Password: "p"}); err != nil {
		t.Fatalf("connect error: %v", err)
	}
	var count int
	sink := &recordingSink{onMsg: func(_ context.Context, _ model.CanonicalMail) error {
		count++
		return nil
	}}
	if err := c.InitialFullSync(context.Background(), 0, sink); err != nil {
		t.Fatalf("full sync error: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 mails, got %d", count)
	}
}

type recordingSink struct {
	onMsg func(ctx context.Context, m model.CanonicalMail) error
}

func (s *recordingSink) OnMessage(ctx context.Context, m model.CanonicalMail) error {
	return s.onMsg(ctx, m)
}
func (s *recordingSink) OnDelete(_ context.Context, _ string) error { return nil }
