package connector

import (
	"context"
	"strings"
	"testing"
	"time"

	"email-aggregator-go/src/model"
)

// TestRealPOP3Connector_InitialFullSync 验证真实客户端能连上 POP3 服务端、
// 完成 USER/PASS 鉴权、UIDL 列号 + RETR 拉取，并把 RFC 822 邮件解析为 CanonicalMail
// （含 RFC 2047 编码主题解码、地址组、正文、UIDL 幂等键）。
func TestRealPOP3Connector_InitialFullSync(t *testing.T) {
	srv, stop, err := StartMockPOP3()
	if err != nil {
		t.Fatalf("start mock pop3: %v", err)
	}
	defer stop()

	c := NewRealPOP3Connector(srv.Addr(), false, nil)
	if err := c.Connect(context.Background(), cred("alice@example.com", "pw")); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	var got []model.CanonicalMail
	sink := &imapCollectSink{out: &got}
	if err := c.InitialFullSync(context.Background(), 0, sink); err != nil {
		t.Fatalf("InitialFullSync: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 mails, got %d", len(got))
	}

	m1 := got[0]
	if m1.ID != "<pop3-uidl-1@example.com>" {
		t.Errorf("mail1 ID = %q, want UIDL", m1.ID)
	}
	if m1.Subject != "测试主题，POP3" {
		t.Errorf("mail1 Subject = %q, want decoded 测试主题，POP3", m1.Subject)
	}
	if m1.From.Email != "alice@example.com" {
		t.Errorf("mail1 From = %+v, want alice@example.com", m1.From)
	}
	if len(m1.To) != 1 || m1.To[0].Email != "bob@example.com" {
		t.Errorf("mail1 To = %+v, want bob@example.com", m1.To)
	}
	if len(m1.Cc) != 1 || m1.Cc[0].Email != "carol@example.com" {
		t.Errorf("mail1 Cc = %+v, want carol@example.com", m1.Cc)
	}
	if m1.Provider != "pop3" || m1.Folder != "INBOX" || m1.AccountID != "alice@example.com" {
		t.Errorf("mail1 meta = provider=%q folder=%q account=%q", m1.Provider, m1.Folder, m1.AccountID)
	}
	if m1.InternalDate == 0 {
		t.Errorf("mail1 InternalDate not parsed (got 0)")
	}
	if !strings.Contains(m1.BodyText, "POP3 第一封邮件正文") {
		t.Errorf("mail1 BodyText = %q", m1.BodyText)
	}
	if m1.SizeBytes == 0 {
		t.Errorf("mail1 SizeBytes not set")
	}

	m2 := got[1]
	if m2.Subject != "second pop3 mail" {
		t.Errorf("mail2 Subject = %q", m2.Subject)
	}
	if m2.From.Email != "bob@example.com" {
		t.Errorf("mail2 From = %+v", m2.From)
	}
	if len(m2.To) != 0 {
		t.Errorf("mail2 To = %+v, want empty", m2.To)
	}
	if !strings.Contains(m2.BodyText, "second body line two") {
		t.Errorf("mail2 BodyText = %q", m2.BodyText)
	}
}

// TestRealPOP3Connector_InitialFullSync_Since 验证 since 过滤：早于阈值的邮件被跳过。
func TestRealPOP3Connector_InitialFullSync_Since(t *testing.T) {
	srv, stop, err := StartMockPOP3()
	if err != nil {
		t.Fatalf("start mock pop3: %v", err)
	}
	defer stop()

	c := NewRealPOP3Connector(srv.Addr(), false, nil)
	if err := c.Connect(context.Background(), cred("alice@example.com", "pw")); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	// 阈值取两封邮件之间（mail1 2026-07-17 10:00 +0000 = 1784282400000，mail2 2026-07-18 09:00 +0000 = 1784365200000）
	since := int64(1784307600000)
	var got []model.CanonicalMail
	if err := c.InitialFullSync(context.Background(), since, &imapCollectSink{out: &got}); err != nil {
		t.Fatalf("InitialFullSync: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 mail after since (the newer one), got %d", len(got))
	}
	if got[0].ID != "<pop3-uidl-2@example.com>" {
		t.Errorf("surviving mail ID = %q, want uidl-2", got[0].ID)
	}
}

// TestRealPOP3Connector_AuthFailure 验证错误密码（mock -ERR）导致 Connect 失败。
func TestRealPOP3Connector_AuthFailure(t *testing.T) {
	srv, stop, err := StartMockPOP3()
	if err != nil {
		t.Fatalf("start mock pop3: %v", err)
	}
	defer stop()

	c := NewRealPOP3Connector(srv.Addr(), false, nil)
	err = c.Connect(context.Background(), cred("alice@example.com", "badpass"))
	if err == nil {
		t.Fatal("expected auth failure, got nil")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error should mention rejection, got: %v", err)
	}
}

// TestRealPOP3Connector_IncrementalSync 验证增量：以 UIDL 高水位断点仅拉取新邮件。
func TestRealPOP3Connector_IncrementalSync(t *testing.T) {
	srv, stop, err := StartMockPOP3()
	if err != nil {
		t.Fatalf("start mock pop3: %v", err)
	}
	defer stop()

	c := NewRealPOP3Connector(srv.Addr(), false, nil)
	if err := c.Connect(context.Background(), cred("alice@example.com", "pw")); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	var got []model.CanonicalMail
	if err := c.IncrementalSync(context.Background(), model.SyncCursor{HighWaterMark: "<pop3-uidl-1@example.com>"}, &imapCollectSink{out: &got}); err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 new mail, got %d", len(got))
	}
	if got[0].ID != "<pop3-uidl-2@example.com>" {
		t.Errorf("incremental mail ID = %q, want uidl-2", got[0].ID)
	}
}

// TestRealPOP3Connector_StreamChanges 验证轮询推送在 ctx 取消后正常退出（不挂死）。
func TestRealPOP3Connector_StreamChanges(t *testing.T) {
	srv, stop, err := StartMockPOP3()
	if err != nil {
		t.Fatalf("start mock pop3: %v", err)
	}
	defer stop()

	c := NewRealPOP3Connector(srv.Addr(), false, nil)
	if err := c.Connect(context.Background(), cred("bob@example.com", "pw")); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()
	c.SetPollInterval(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var got int
	err = c.StreamChanges(ctx, model.SyncCursor{}, func(_ context.Context, _ model.CanonicalMail) error {
		got++
		return nil
	})
	if err != nil {
		t.Fatalf("StreamChanges returned error: %v", err)
	}
	if got < 2 {
		t.Errorf("expected at least 2 events from polling, got %d", got)
	}
}
