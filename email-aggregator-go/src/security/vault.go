// Package security 实现凭据保险库与 KMS 信封加密（ADR-004）。
// 明文零落盘：一次性 DEK 加密数据，KEK（由 KMS 持有）包裹 DEK。
package security

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
)

// Kms 密钥管理服务抽象（生产替换为云 KMS / HashiCorp Vault Transit）
type Kms interface {
	Encrypt(ctx context.Context, plaintext []byte) (blob []byte, err error)
	Decrypt(ctx context.Context, blob []byte) (plaintext []byte, err error)
	RotateKEK(ctx context.Context) (newKEKID string, err error)
}

// Envelope 信封加密产物（仅此结构可持久化，不含明文）
type Envelope struct {
	KEKID      string `json:"kekId"`
	WrappedDEK string `json:"wrappedDek"` // base64(KEK 包裹的 DEK)
	IV         string `json:"iv"`         // base64(GCM nonce)
	Ciphertext string `json:"ciphertext"` // base64(AES-GCM 输出)
}

// CredentialVault 凭据保险库
type CredentialVault struct {
	kms Kms
}

// NewCredentialVault 构造
func NewCredentialVault(kms Kms) *CredentialVault {
	return &CredentialVault{kms: kms}
}

// Seal 信封加密：生成一次性 DEK → AES-GCM 加密明文 → KEK 包裹 DEK
func (v *CredentialVault) Seal(ctx context.Context, plaintext []byte) (Envelope, error) {
	dek := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return Envelope{}, err
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return Envelope{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Envelope{}, err
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return Envelope{}, err
	}
	ct := gcm.Seal(nil, iv, plaintext, nil)
	wrapped, err := v.kms.Encrypt(ctx, dek)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		KEKID:      "kek-1",
		WrappedDEK: base64.StdEncoding.EncodeToString(wrapped),
		IV:         base64.StdEncoding.EncodeToString(iv),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
	}, nil
}

// Unseal 拆封：KEK 解包 DEK → AES-GCM 解密
func (v *CredentialVault) Unseal(ctx context.Context, env Envelope) ([]byte, error) {
	wrapped, err := base64.StdEncoding.DecodeString(env.WrappedDEK)
	if err != nil {
		return nil, err
	}
	dek, err := v.kms.Decrypt(ctx, wrapped)
	if err != nil {
		return nil, err
	}
	iv, err := base64.StdEncoding.DecodeString(env.IV)
	if err != nil {
		return nil, err
	}
	ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, iv, ct, nil)
}

// Rotate 密钥轮换：拆封 → 用新 KEK 重新包裹（明文不出函数作用域）
func (v *CredentialVault) Rotate(ctx context.Context, old Envelope) (Envelope, error) {
	pt, err := v.Unseal(ctx, old)
	if err != nil {
		return Envelope{}, err
	}
	newKEKID, err := v.kms.RotateKEK(ctx)
	if err != nil {
		return Envelope{}, err
	}
	env, err := v.Seal(ctx, pt)
	if err != nil {
		return Envelope{}, err
	}
	env.KEKID = newKEKID
	return env, nil
}

// InMemoryKms 演示用 KMS（固定 KEK；生产替换为云 KMS / Vault Transit）
type InMemoryKms struct {
	kek []byte
}

// NewInMemoryKms 构造（32 字节演示密钥）
func NewInMemoryKms() *InMemoryKms {
	return &InMemoryKms{kek: []byte("0123456789abcdef0123456789abcdef")}
}

// NewInMemoryKmsWithKEK 用外部 KEK 构造演示 KMS：任意长度字符串经 SHA-256 规整为 32 字节；
// 空串回退到内置演示密钥。生产替换为云 KMS / Vault Transit，KEK 由密钥管理服务持有（不落盘）。
func NewInMemoryKmsWithKEK(key string) *InMemoryKms {
	kek := []byte("0123456789abcdef0123456789abcdef")
	if key != "" {
		sum := sha256.Sum256([]byte(key))
		kek = sum[:]
	}
	return &InMemoryKms{kek: kek}
}

// Encrypt KEK 包裹（AES-GCM，nonce 前缀于输出）
func (k *InMemoryKms) Encrypt(_ context.Context, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(k.kek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, iv, plaintext, nil)
	out := make([]byte, 0, len(iv)+len(sealed))
	out = append(out, iv...)
	out = append(out, sealed...)
	return out, nil
}

// Decrypt KEK 解包
func (k *InMemoryKms) Decrypt(_ context.Context, blob []byte) ([]byte, error) {
	block, err := aes.NewCipher(k.kek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("ciphertext too short")
	}
	return gcm.Open(nil, blob[:ns], blob[ns:], nil)
}

// RotateKEK 演示轮换（返回新 KEK 标识）
func (k *InMemoryKms) RotateKEK(_ context.Context) (string, error) {
	return "kek-2", nil
}
