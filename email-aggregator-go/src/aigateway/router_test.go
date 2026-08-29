package aigateway

import (
	"context"
	"strings"
	"testing"
)

func TestDecide_PrivateForcedSelfHosted(t *testing.T) {
	dec := Decide(TierPrivate, SensitivityPII, false)
	if !dec.Allowed || dec.Provider != "self-hosted" || dec.RequireRedaction {
		t.Fatalf("private tier must force self-hosted without redaction need: %+v", dec)
	}
}

func TestDecide_PublicPIIWithoutConsentDenied(t *testing.T) {
	dec := Decide(TierPublic, SensitivityPII, false)
	if dec.Allowed {
		t.Fatalf("public PII without consent must be denied: %+v", dec)
	}
}

func TestDecide_PublicPIIConsentedThirdParty(t *testing.T) {
	dec := Decide(TierPublic, SensitivityPII, true)
	if !dec.Allowed || dec.Provider != "third-party" || !dec.RequireRedaction {
		t.Fatalf("public PII with consent must go third-party with redaction: %+v", dec)
	}
}

func newTestRouter() (*Router, *[]AuditEvent) {
	self := &StubProvider{Name_: "vllm", Backend: "self-hosted"}
	third := &StubProvider{Name_: "claude", Backend: "third-party"}
	var audited []AuditEvent
	r := NewRouter(self, third, NewRegexRedactor(), func(_ context.Context, ev AuditEvent) {
		audited = append(audited, ev)
	})
	return r, &audited
}

func TestRouter_PrivateRoutesSelfHosted(t *testing.T) {
	r, _ := newTestRouter()
	resp, err := r.RouteAndChat(context.Background(), ChatRequest{
		TenantID:    "t-priv",
		TenantTier:  TierPrivate,
		Capability:  CapChat,
		Sensitivity: SensitivityPII,
		Messages:    []Message{{Role: RoleUser, Content: "联系 alice@example.com 或 13800138000"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Backend != "self-hosted" {
		t.Fatalf("private must route to self-hosted, got %s", resp.Backend)
	}
}

func TestRouter_PublicPIIRedactedBeforeThirdParty(t *testing.T) {
	r, audited := newTestRouter()
	resp, err := r.RouteAndChat(context.Background(), ChatRequest{
		TenantID:      "t-pub",
		TenantTier:    TierPublic,
		Capability:    CapChat,
		Sensitivity:   SensitivityPII,
		UserConsented: true,
		Messages:      []Message{{Role: RoleUser, Content: "我的邮箱是 bob@example.com 电话 13912345678"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Backend != "third-party" {
		t.Fatalf("public consented PII must route third-party, got %s", resp.Backend)
	}
	if !resp.Redacted {
		t.Fatal("expected Redacted=true for third-party PII path")
	}
	if strings.Contains(resp.Content, "bob@example.com") || strings.Contains(resp.Content, "13912345678") {
		t.Fatalf("PII leaked to third party: %s", resp.Content)
	}
	if len(*audited) != 1 || !(*audited)[0].Allowed || !(*audited)[0].Redacted {
		t.Fatalf("audit event mismatch: %+v", *audited)
	}
}

func TestRouter_PublicPIINoConsentDenied(t *testing.T) {
	r, _ := newTestRouter()
	_, err := r.RouteAndChat(context.Background(), ChatRequest{
		TenantID:      "t-pub",
		TenantTier:    TierPublic,
		Capability:    CapChat,
		Sensitivity:   SensitivityPII,
		UserConsented: false,
		Messages:      []Message{{Role: RoleUser, Content: "有 PII"}},
	})
	if err == nil {
		t.Fatal("expected denial for public PII without consent")
	}
}
