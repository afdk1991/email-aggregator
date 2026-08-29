package connector

import (
	"context"
	"strings"
	"testing"
	"time"

	"email-aggregator-go/src/model"
)

// gmailCollectSink 收集邮件并记录删除（供 Gmail 删除分支断言）。
type gmailCollectSink struct {
	mails []model.CanonicalMail
	dels  []string
}

func (s *gmailCollectSink) OnMessage(_ context.Context, m model.CanonicalMail) error {
	s.mails = append(s.mails, m)
	return nil
}
func (s *gmailCollectSink) OnDelete(_ context.Context, id string) error {
	s.dels = append(s.dels, id)
	return nil
}

// gmailRaw 构造一封 Gmail 风格 RFC 822 邮件。
func gmailRaw(subject, from, to, body, date string) string {
	return "From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: " + date + "\r\n" +
		"Message-ID: <gm-" + subject + "@example.com>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		body + "\r\n"
}

// credOAuth 构造 OAuth2 凭据（Gmail Bearer 令牌）。
func credOAuth(user, token string) model.Credential {
	return model.Credential{
		Type:     "oauth2",
		Username: user,
		OAuth:    &model.OAuthToken{AccessToken: token},
	}
}

const (
	gmDate1 = "Fri, 17 Jul 2026 10:00:00 +0000" // 1784282400000
	gmDate2 = "Sat, 18 Jul 2026 09:00:00 +0000" // 1784365200000
	gmDate3 = "Sun, 19 Jul 2026 08:00:00 +0000" // 1784448000000
)

// TestRealGmailConnector_InitialFullSync 验证：mock Gmail API 鉴权（Bearer）+ 分页列表 +
// format=raw 拉取 + RFC 822 解析为 CanonicalMail（provider=gmail，游标基线=profile.historyId）。
func TestRealGmailConnector_InitialFullSync(t *testing.T) {
	srv, stop, err := StartMockGmailAPI()
	if err != nil {
		t.Fatalf("start mock gmail: %v", err)
	}
	defer stop()
	srv.SetPageSize(2) // 注入 3 封 → 必须跨 2 页

	srv.Inject(gmailRaw("gmail 第一封", "alice@example.com", "bob@example.com", "Gmail 第一封正文", gmDate1), 1784282400000)
	srv.Inject(gmailRaw("gmail second", "bob@example.com", "carol@example.com", "second body", gmDate2), 1784365200000)
	srv.Inject(gmailRaw("gmail third", "carol@example.com", "alice@example.com", "third body", gmDate3), 1784448000000)

	c := NewRealGmailConnector(srv.Addr() + "/gmail/v1")
	if err := c.Connect(context.Background(), credOAuth("alice@example.com", "tok-1")); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	sink := &gmailCollectSink{}
	if err := c.InitialFullSync(context.Background(), 0, sink); err != nil {
		t.Fatalf("InitialFullSync: %v", err)
	}
	if len(sink.mails) != 3 {
		t.Fatalf("expected 3 mails, got %d", len(sink.mails))
	}
	m1 := sink.mails[0]
	if m1.Subject != "gmail 第一封" {
		t.Errorf("m1 Subject = %q", m1.Subject)
	}
	if m1.From.Email != "alice@example.com" {
		t.Errorf("m1 From = %+v", m1.From)
	}
	if len(m1.To) != 1 || m1.To[0].Email != "bob@example.com" {
		t.Errorf("m1 To = %+v", m1.To)
	}
	if m1.Provider != "gmail" || m1.Folder != "INBOX" || m1.AccountID != "alice@example.com" {
		t.Errorf("m1 meta = provider=%q folder=%q account=%q", m1.Provider, m1.Folder, m1.AccountID)
	}
	if m1.InternalDate != 1784282400000 {
		t.Errorf("m1 InternalDate = %d, want 1784282400000", m1.InternalDate)
	}
	if !strings.Contains(m1.BodyText, "Gmail 第一封正文") {
		t.Errorf("m1 BodyText = %q", m1.BodyText)
	}
	// 游标基线 = profile.historyId（此时 3）
	if got := m1.Cursor.ProviderSpecific["historyId"]; got != "3" {
		t.Errorf("m1 cursor historyId = %q, want 3", got)
	}
	if c.LastHistoryID() != "3" {
		t.Errorf("LastHistoryID = %q, want 3", c.LastHistoryID())
	}
}

// TestRealGmailConnector_IncrementalSync 验证：historyId 断点增量 →
// 仅新增邮件回传（携带最新 historyId 游标），再次增量（同游标）无重复。
func TestRealGmailConnector_IncrementalSync(t *testing.T) {
	srv, stop, err := StartMockGmailAPI()
	if err != nil {
		t.Fatalf("start mock gmail: %v", err)
	}
	defer stop()

	id1 := srv.Inject(gmailRaw("mail one", "a@example.com", "b@example.com", "one", gmDate1), 1784282400000)

	c := NewRealGmailConnector(srv.Addr() + "/gmail/v1")
	if err := c.Connect(context.Background(), credOAuth("me@example.com", "tok")); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	// 全量建立基线
	sink1 := &gmailCollectSink{}
	if err := c.InitialFullSync(context.Background(), 0, sink1); err != nil {
		t.Fatalf("InitialFullSync: %v", err)
	}
	if len(sink1.mails) != 1 || sink1.mails[0].ID != id1 {
		t.Fatalf("full sync got %d mails, want [%s]", len(sink1.mails), id1)
	}
	baseline := sink1.mails[0].Cursor.ProviderSpecific["historyId"]

	// 增量：注入新邮件
	id2 := srv.Inject(gmailRaw("mail two", "b@example.com", "c@example.com", "two", gmDate2), 1784365200000)
	sink2 := &gmailCollectSink{}
	cursor := model.SyncCursor{ProviderSpecific: map[string]string{"historyId": baseline}}
	if err := c.IncrementalSync(context.Background(), cursor, sink2); err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if len(sink2.mails) != 1 || sink2.mails[0].ID != id2 {
		t.Fatalf("incremental got %d mails, want [%s]", len(sink2.mails), id2)
	}
	if got := sink2.mails[0].Cursor.ProviderSpecific["historyId"]; got == baseline || got == "" {
		t.Errorf("incremental cursor = %q, want advanced from %q", got, baseline)
	}
	adv := sink2.mails[0].Cursor.ProviderSpecific["historyId"]

	// 再增量（同游标）→ 无新增
	sink3 := &gmailCollectSink{}
	if err := c.IncrementalSync(context.Background(), model.SyncCursor{ProviderSpecific: map[string]string{"historyId": adv}}, sink3); err != nil {
		t.Fatalf("IncrementalSync#2: %v", err)
	}
	if len(sink3.mails) != 0 {
		t.Fatalf("expected no duplicate, got %d", len(sink3.mails))
	}
}

// TestRealGmailConnector_Delete 验证：删除经 messagesDeleted → sink.OnDelete 回调。
func TestRealGmailConnector_Delete(t *testing.T) {
	srv, stop, err := StartMockGmailAPI()
	if err != nil {
		t.Fatalf("start mock gmail: %v", err)
	}
	defer stop()

	id1 := srv.Inject(gmailRaw("to delete", "a@example.com", "b@example.com", "d", gmDate1), 1784282400000)

	c := NewRealGmailConnector(srv.Addr() + "/gmail/v1")
	if err := c.Connect(context.Background(), credOAuth("me@example.com", "tok")); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	sink1 := &gmailCollectSink{}
	if err := c.InitialFullSync(context.Background(), 0, sink1); err != nil {
		t.Fatalf("InitialFullSync: %v", err)
	}
	baseline := sink1.mails[0].Cursor.ProviderSpecific["historyId"]

	// 删除该邮件
	srv.Delete(id1)

	sink2 := &gmailCollectSink{}
	if err := c.IncrementalSync(context.Background(), model.SyncCursor{ProviderSpecific: map[string]string{"historyId": baseline}}, sink2); err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if len(sink2.mails) != 0 {
		t.Errorf("expected 0 added, got %d", len(sink2.mails))
	}
	if len(sink2.dels) != 1 || sink2.dels[0] != id1 {
		t.Errorf("expected delete [%s], got %v", id1, sink2.dels)
	}
}

// TestRealGmailConnector_AuthFailure 验证：错误 Bearer 令牌 → Connect profile 探测 401。
func TestRealGmailConnector_AuthFailure(t *testing.T) {
	srv, stop, err := StartMockGmailAPI()
	if err != nil {
		t.Fatalf("start mock gmail: %v", err)
	}
	defer stop()
	srv.SetToken("correct-token")

	c := NewRealGmailConnector(srv.Addr() + "/gmail/v1")
	err = c.Connect(context.Background(), credOAuth("me@example.com", "wrong-token"))
	if err == nil {
		t.Fatal("expected auth failure, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention 401, got: %v", err)
	}
}

// TestRealGmailConnector_MissingToken 验证：无 OAuth 令牌 → Connect 报错。
func TestRealGmailConnector_MissingToken(t *testing.T) {
	c := NewRealGmailConnector("http://127.0.0.1:1")
	err := c.Connect(context.Background(), model.Credential{Type: "password", Username: "x", Password: "y"})
	if err == nil || !strings.Contains(err.Error(), "token required") {
		t.Fatalf("expected token-required error, got: %v", err)
	}
}

// TestRealGmailConnector_StreamChanges 验证：轮询推送在 ctx 取消后正常退出且收到新增事件。
func TestRealGmailConnector_StreamChanges(t *testing.T) {
	srv, stop, err := StartMockGmailAPI()
	if err != nil {
		t.Fatalf("start mock gmail: %v", err)
	}
	defer stop()

	srv.Inject(gmailRaw("stream one", "a@example.com", "b@example.com", "s1", gmDate1), 1784282400000)

	c := NewRealGmailConnector(srv.Addr() + "/gmail/v1")
	if err := c.Connect(context.Background(), credOAuth("me@example.com", "tok")); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()
	c.SetPollInterval(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var got int
	var firstID string
	err = c.StreamChanges(ctx, model.SyncCursor{}, func(_ context.Context, m model.CanonicalMail) error {
		got++
		firstID = m.ID
		return nil
	})
	if err != nil {
		t.Fatalf("StreamChanges returned error: %v", err)
	}
	if got < 1 {
		t.Errorf("expected at least 1 event from polling, got %d", got)
	}
	if firstID == "" {
		t.Error("expected a message id from stream")
	}
}
