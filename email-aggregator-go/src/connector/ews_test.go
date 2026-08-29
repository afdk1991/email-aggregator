package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"email-aggregator-go/src/model"
)

const ewsFindItemResp = `<?xml version="1.0" encoding="utf-8"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">
  <soap:Body>
    <FindItemResponse>
      <ResponseMessages>
        <FindItemResponseMessage>
          <ResponseCode>NoError</ResponseCode>
          <RootFolder TotalItemsInView="2" IncludesLastItemInRange="true">
            <Items>
              <Message>
                <ItemId Id="item-1" ChangeKey="ck1"/>
                <Subject>Quarterly Report</Subject>
                <From><Mailbox><Name>Finance</Name><EmailAddress>finance@corp.com</EmailAddress></Mailbox></From>
                <DateTimeReceived>2024-03-01T09:00:00Z</DateTimeReceived>
                <HasAttachments>true</HasAttachments>
                <Size>2048</Size>
                <IsRead>false</IsRead>
              </Message>
              <Message>
                <ItemId Id="item-2" ChangeKey="ck2"/>
                <Subject>Team Update</Subject>
                <From><Mailbox><Name>PM</Name><EmailAddress>pm@corp.com</EmailAddress></Mailbox></From>
                <DateTimeReceived>2024-03-02T10:00:00Z</DateTimeReceived>
                <HasAttachments>false</HasAttachments>
                <Size>1024</Size>
                <IsRead>true</IsRead>
              </Message>
            </Items>
          </RootFolder>
        </FindItemResponseMessage>
      </ResponseMessages>
    </FindItemResponse>
  </soap:Body>
</soap:Envelope>`

const ewsGetItemResp = `<?xml version="1.0" encoding="utf-8"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">
  <soap:Body>
    <GetItemResponse>
      <ResponseMessages>
        <GetItemResponseMessage>
          <Items>
            <Message>
              <ItemId Id="item-1" ChangeKey="ck1"/>
              <Subject>Quarterly Report</Subject>
              <Body BodyType="Text">Body of the email.</Body>
            </Message>
          </Items>
        </GetItemResponseMessage>
      </ResponseMessages>
    </GetItemResponse>
  </soap:Body>
</soap:Envelope>`

func newMockEWSServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		if strings.Contains(string(body), "FindItem") {
			_, _ = w.Write([]byte(ewsFindItemResp))
			return
		}
		_, _ = w.Write([]byte(ewsGetItemResp))
	}))
}

type collectSink struct {
	mu    sync.Mutex
	mails []model.CanonicalMail
}

func (s *collectSink) OnMessage(_ context.Context, m model.CanonicalMail) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mails = append(s.mails, m)
	return nil
}

func (s *collectSink) OnDelete(_ context.Context, _ string) error { return nil }

// TestEWSConnector_InitialFullSync 验证 EWS FindItem+GetItem 全链路：
// 解析邮件元数据（主题/发件人/已读/附件/大小/时间）并经 sink 回传，GetItem 取回正文。
func TestEWSConnector_InitialFullSync(t *testing.T) {
	srv := newMockEWSServer(t)
	defer srv.Close()

	c := NewEWSConnector(srv.URL)
	if err := c.Connect(context.Background(), model.Credential{
		Type: "password", Username: "user@corp.com", Password: "pw",
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if c.Capabilities().Provider != model.ProviderExchange {
		t.Fatalf("provider mismatch: %s", c.Capabilities().Provider)
	}

	sink := &collectSink{}
	if err := c.InitialFullSync(context.Background(), 0, sink); err != nil {
		t.Fatalf("InitialFullSync: %v", err)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.mails) != 2 {
		t.Fatalf("want 2 mails, got %d", len(sink.mails))
	}
	byID := map[string]model.CanonicalMail{}
	for _, m := range sink.mails {
		byID[m.ID] = m
	}
	m1, ok := byID["item-1"]
	if !ok {
		t.Fatal("item-1 missing")
	}
	if m1.Subject != "Quarterly Report" || m1.From.Email != "finance@corp.com" {
		t.Fatalf("m1 wrong: %+v", m1)
	}
	if !m1.HasAttachment || m1.Read || m1.SizeBytes != 2048 {
		t.Fatalf("m1 flags wrong: %+v", m1)
	}
	if m1.BodyText != "Body of the email." {
		t.Fatalf("m1 body wrong: %q", m1.BodyText)
	}
	m2 := byID["item-2"]
	if !m2.Read || m2.From.Email != "pm@corp.com" {
		t.Fatalf("m2 wrong: %+v", m2)
	}
}

// TestEWSConnector_StreamChangesCancel 验证长轮询在 ctx 取消时不阻塞退出。
func TestEWSConnector_StreamChangesCancel(t *testing.T) {
	srv := newMockEWSServer(t)
	defer srv.Close()

	c := NewEWSConnector(srv.URL)
	c.SetPollInterval(10 * time.Millisecond)
	_ = c.Connect(context.Background(), model.Credential{Username: "u", Password: "p"})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.StreamChanges(ctx, model.SyncCursor{}, func(_ context.Context, _ model.CanonicalMail) error {
			return nil
		})
	}()

	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("StreamChanges returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StreamChanges did not return on ctx cancel")
	}
}
