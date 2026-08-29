package connector

import (
	"context"
	"testing"
	"time"

	"email-aggregator-go/src/model"
)

// ── 测试用例 ─────────────────────────────────────────────────────────────────
//
// 注：mock IMAP server 已抽到非测试文件 connector/imap_mock.go（StartMockIMAP），
// 以便 syncsv 集成测试跨包复用；本文件仅消费该 helper。

// TestRealIMAPConnector_InitialFullSync 验证真实客户端能连上 IMAP 服务端、
// 拉取并正确解析两封邮件的关键字段（含附件探测、已读状态、地址组、日期、字面量）。
func TestRealIMAPConnector_InitialFullSync(t *testing.T) {
	srv, stop, err := StartMockIMAP()
	if err != nil {
		t.Fatalf("start mock imap: %v", err)
	}
	defer stop()
	addr := srv.Addr()

	c := NewRealIMAPConnector(addr, false, nil)
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
	if m1.ID != "1" {
		t.Errorf("mail1 ID = %q, want 1", m1.ID)
	}
	if m1.Subject != "测试邮件主题" {
		t.Errorf("mail1 Subject = %q", m1.Subject)
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
	if m1.SizeBytes != 1024 {
		t.Errorf("mail1 SizeBytes = %d, want 1024", m1.SizeBytes)
	}
	if m1.InternalDate == 0 {
		t.Errorf("mail1 InternalDate not parsed (got 0)")
	}
	if !m1.HasAttachment {
		t.Errorf("mail1 HasAttachment should be true (doc.pdf attachment present)")
	}
	if !m1.Read {
		t.Errorf("mail1 Read should be true (FLAGS \\Seen)")
	}
	if m1.Provider != "imap" || m1.Folder != "INBOX" || m1.AccountID != "alice@example.com" {
		t.Errorf("mail1 meta = provider=%q folder=%q account=%q", m1.Provider, m1.Folder, m1.AccountID)
	}

	m2 := got[1]
	if m2.Subject != "第二封无地址" {
		t.Errorf("mail2 Subject = %q", m2.Subject)
	}
	if m2.From.Email != "" {
		t.Errorf("mail2 From should be empty (NIL), got %+v", m2.From)
	}
	if m2.HasAttachment {
		t.Errorf("mail2 HasAttachment should be false")
	}
	if m2.Read {
		t.Errorf("mail2 Read should be false (no \\Seen)")
	}
}

// TestRealIMAPConnector_StreamChanges 验证 IDLE 握手与 ctx 取消后正常退出（不挂死、不 panic）。
func TestRealIMAPConnector_StreamChanges(t *testing.T) {
	srv, stop, err := StartMockIMAP()
	if err != nil {
		t.Fatalf("start mock imap: %v", err)
	}
	defer stop()
	addr := srv.Addr()

	c := NewRealIMAPConnector(addr, false, nil)
	if err := c.Connect(context.Background(), cred("bob@example.com", "pw")); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = c.StreamChanges(ctx, model.SyncCursor{LastUID: 0}, func(_ context.Context, _ model.CanonicalMail) error {
		return nil
	})
	if err != nil {
		t.Fatalf("StreamChanges returned error: %v", err)
	}
}

// TestParseFetchLiteral 直接验证含字面量 {N} 的 FETCH 响应能被解析而不报错。
func TestParseFetchLiteral(t *testing.T) {
	raw := "* 1 FETCH (UID 7 RFC822.SIZE 10 BODY[] {5}\r\nhello\r\n FLAGS ())\n"
	mails, err := parseFetchResponses(raw)
	if err != nil {
		t.Fatalf("parseFetchResponses: %v", err)
	}
	if len(mails) != 1 {
		t.Fatalf("expected 1 mail, got %d", len(mails))
	}
	if mails[0].ID != "7" || mails[0].SizeBytes != 10 {
		t.Errorf("parsed mail = %+v", mails[0])
	}
}

// ── 测试辅助 ─────────────────────────────────────────────────────────────────

func cred(user, pass string) model.Credential {
	return model.Credential{Type: "password", Username: user, Password: pass}
}

// imapCollectSink 收集 OnMessage 回传的邮件（实现 model.SyncSink）。
// 注意：ews_test.go 已定义同名 collectSink（并发安全版），此处改名以避免包级重声明。
type imapCollectSink struct {
	out *[]model.CanonicalMail
}

func (s *imapCollectSink) OnMessage(_ context.Context, m model.CanonicalMail) error {
	*s.out = append(*s.out, m)
	return nil
}

func (s *imapCollectSink) OnDelete(_ context.Context, _ string) error { return nil }
