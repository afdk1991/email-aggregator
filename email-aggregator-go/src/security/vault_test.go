package security

import (
	"context"
	"strings"
	"testing"
)

// 信封加密往返：Seal 后 Unseal 必须还原明文
func TestVault_SealUnsealRoundTrip(t *testing.T) {
	ctx := context.Background()
	v := NewCredentialVault(NewInMemoryKms())
	plain := []byte(`{"username":"alice","password":"s3cret"}`)
	env, err := v.Seal(ctx, plain)
	if err != nil {
		t.Fatalf("Seal error: %v", err)
	}
	// 明文不得出现在密文/包裹中
	if strings.Contains(env.Ciphertext, "alice") || strings.Contains(env.WrappedDEK, "alice") {
		t.Fatalf("plaintext leaked into envelope")
	}
	got, err := v.Unseal(ctx, env)
	if err != nil {
		t.Fatalf("Unseal error: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("roundtrip mismatch: want %q got %q", plain, got)
	}
}

// 密钥轮换：新 KEK 包裹后仍可拆封
func TestVault_Rotate(t *testing.T) {
	ctx := context.Background()
	v := NewCredentialVault(NewInMemoryKms())
	env, _ := v.Seal(ctx, []byte("top-secret"))
	rotated, err := v.Rotate(ctx, env)
	if err != nil {
		t.Fatalf("Rotate error: %v", err)
	}
	if rotated.KEKID != "kek-2" {
		t.Fatalf("expected rotated KEK id kek-2, got %s", rotated.KEKID)
	}
	got, err := v.Unseal(ctx, rotated)
	if err != nil {
		t.Fatalf("Unseal after rotate error: %v", err)
	}
	if string(got) != "top-secret" {
		t.Fatalf("post-rotate plaintext mismatch")
	}
}

// 篡改密文必须导致 Unseal 失败（完整性）
func TestVault_TamperFails(t *testing.T) {
	ctx := context.Background()
	v := NewCredentialVault(NewInMemoryKms())
	env, _ := v.Seal(ctx, []byte("data"))
	if len(env.Ciphertext) > 0 {
		// 翻转最后一个 base64 字符以模拟篡改（简化：直接标记错误路径由 GCM 校验）
		env.Ciphertext = "AAAA" + env.Ciphertext
	}
	if _, err := v.Unseal(ctx, env); err == nil {
		t.Fatalf("expected Unseal to fail on tampered ciphertext")
	}
}
