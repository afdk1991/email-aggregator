package main

import (
	"context"
	"encoding/json"
	"testing"

	"email-aggregator-go/src/security"
)

func TestResolveSecret_Plaintext(t *testing.T) {
	v := security.NewCredentialVault(security.NewInMemoryKmsWithKEK("test-kek"))
	got, err := resolveSecret(context.Background(), v, "my-plain-secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "my-plain-secret" {
		t.Fatalf("plaintext mismatch: got %q", got)
	}
}

func TestResolveSecret_Envelope(t *testing.T) {
	v := security.NewCredentialVault(security.NewInMemoryKmsWithKEK("test-kek"))
	// 将明文密封为信封，再经 resolveSecret 解析回来，验证 ADR-004 运行期接入闭环
	env, err := v.Seal(context.Background(), []byte("super-secret-pass"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	blob, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	got, err := resolveSecret(context.Background(), v, string(blob))
	if err != nil {
		t.Fatalf("unseal via resolveSecret: %v", err)
	}
	if got != "super-secret-pass" {
		t.Fatalf("envelope roundtrip mismatch: got %q", got)
	}
}

func TestResolveSecret_TamperedEnvelope(t *testing.T) {
	v := security.NewCredentialVault(security.NewInMemoryKmsWithKEK("test-kek"))
	env, _ := v.Seal(context.Background(), []byte("super-secret-pass"))
	env.Ciphertext = "AAAA" + env.Ciphertext // 篡改密文
	blob, _ := json.Marshal(env)
	if _, err := resolveSecret(context.Background(), v, string(blob)); err == nil {
		t.Fatalf("expected unseal error on tampered envelope, got nil")
	}
}
