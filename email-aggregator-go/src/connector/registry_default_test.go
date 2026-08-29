package connector

import (
	"testing"

	"email-aggregator-go/src/model"
)

// TestDefaultRegistry 验证默认注册表能按协议创建三种真实连接器，
// 并对缺失必填参数 / 未注册协议返回错误（不静默降级）。
func TestDefaultRegistry(t *testing.T) {
	reg := NewDefaultRegistry()

	// IMAP 缺 address 应报错
	if _, err := reg.Create(model.ProviderIMAP, nil); err == nil {
		t.Fatal("expected error for imap without cfg[\"address\"]")
	}

	// IMAP 带 address 应成功，且报告 provider=imap
	c, err := reg.Create(model.ProviderIMAP, map[string]string{"address": "imap.example.com:993", "useTLS": "true"})
	if err != nil {
		t.Fatalf("create imap: %v", err)
	}
	if c.Capabilities().Provider != model.ProviderIMAP {
		t.Fatalf("imap provider mismatch: %s", c.Capabilities().Provider)
	}

	// Gmail 无 cfg 走默认生产 Gmail API 基址，且应成功创建（RealGmailConnector，OAuth Bearer 由 Connect 提供）
	g, err := reg.Create(model.ProviderGmail, nil)
	if err != nil {
		t.Fatalf("create gmail: %v", err)
	}
	if g == nil {
		t.Fatal("gmail connector is nil")
	}
	if g.Capabilities().Provider != model.ProviderGmail {
		t.Fatalf("gmail provider mismatch: %s", g.Capabilities().Provider)
	}

	// Exchange / EWS
	e, err := reg.Create(model.ProviderExchange, map[string]string{"endpoint": "https://owa.corp.com/EWS/Exchange.asmx"})
	if err != nil {
		t.Fatalf("create exchange: %v", err)
	}
	if e.Capabilities().Provider != model.ProviderExchange {
		t.Fatalf("exchange provider mismatch: %s", e.Capabilities().Provider)
	}

	// POP3 缺 address 应报错（不静默降级）
	if _, err := reg.Create(model.ProviderPOP3, nil); err == nil {
		t.Fatal("expected error for pop3 without cfg[\"address\"]")
	}

	// POP3 带 address 应成功，且报告 provider=pop3
	p, err := reg.Create(model.ProviderPOP3, map[string]string{"address": "pop3.example.com:110"})
	if err != nil {
		t.Fatalf("create pop3: %v", err)
	}
	if p.Capabilities().Provider != model.ProviderPOP3 {
		t.Fatalf("pop3 provider mismatch: %s", p.Capabilities().Provider)
	}

	// 未注册协议（enterprise）应报错
	if _, err := reg.Create(model.ProviderEnterprise, nil); err == nil {
		t.Fatal("expected error for unregistered enterprise")
	}
}
