package security

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// LocalKeyring 持久化密钥库（多 KEK 版本，JSON 文件）。
type LocalKeyring struct {
	Active string            `json:"active"`        // 当前 active KEK 标识
	KEKs   map[string]string `json:"keks"`           // id -> base64(KEK)
}

// LocalKms 本地真实密钥管理服务：主密钥（KEK）可持久化到文件并支持多版本轮换。
// 实现 Kms 接口（Encrypt/Decrypt/RotateKEK），与 InMemoryKms 行为一致但具备真实持久化与轮换；
// 仅依赖标准库，适用于无云 KMS / Vault 的离线等价场景（ADR-004 运行期落地）。
//
// 多版本兼容：Decrypt 遍历所有历史 KEK 尝试解密，因此密钥轮换后历史信封（用旧 KEK 加密）仍能拆封。
type LocalKms struct {
	mu       sync.Mutex
	keks     map[string][]byte
	activeID string
	file     string // 持久化路径，空则不落盘（仅内存）
}

// NewLocalKmsInMemory 构造仅内存的本地 KMS（随机 KEK，进程退出即丢，演示/单测用）。
func NewLocalKmsInMemory() *LocalKms {
	k := &LocalKms{keks: map[string][]byte{}}
	k.addKEK()
	return k
}

// NewLocalKmsWithKey 用外部主密钥（passphrase/密钥串）构造单 KEK 本地 KMS（兼容旧 InMemoryKmsWithKEK 语义）。
func NewLocalKmsWithKey(key string) *LocalKms {
	kek := []byte("0123456789abcdef0123456789abcdef")
	if key != "" {
		sum := sha256.Sum256([]byte(key))
		kek = sum[:]
	}
	return &LocalKms{keks: map[string][]byte{"kek-1": kek}, activeID: "kek-1"}
}

// NewLocalKmsFromFile 从密钥库文件构造；文件不存在则生成初始密钥并创建文件（权限 0600）。
// 支持多 KEK 版本：轮换后新 KEK 持久化，旧 KEK 保留以解密历史信封。
func NewLocalKmsFromFile(path string) (*LocalKms, error) {
	k := &LocalKms{keks: map[string][]byte{}, file: path}
	if data, err := os.ReadFile(path); err == nil {
		var ring LocalKeyring
		if err := json.Unmarshal(data, &ring); err != nil {
			return nil, fmt.Errorf("local_kms: parse keyring %s: %w", path, err)
		}
		for id, b64 := range ring.KEKs {
			raw, derr := base64.StdEncoding.DecodeString(b64)
			if derr != nil {
				return nil, fmt.Errorf("local_kms: decode kek %s: %w", id, derr)
			}
			k.keks[id] = raw
		}
		k.activeID = ring.Active
	} else {
		k.addKEK() // 生成初始 kek-1
		if err := k.persist(); err != nil {
			return nil, err
		}
	}
	if k.activeID == "" {
		k.activeID = "kek-1"
	}
	return k, nil
}

func (k *LocalKms) addKEK() string {
	kek := make([]byte, 32)
	_, _ = io.ReadFull(rand.Reader, kek)
	id := fmt.Sprintf("kek-%d", len(k.keks)+1)
	k.keks[id] = kek
	k.activeID = id
	return id
}

func (k *LocalKms) persist() error {
	if k.file == "" {
		return nil
	}
	ring := LocalKeyring{Active: k.activeID, KEKs: map[string]string{}}
	for id, raw := range k.keks {
		ring.KEKs[id] = base64.StdEncoding.EncodeToString(raw)
	}
	data, err := json.MarshalIndent(ring, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(k.file, data, 0o600)
}

// Encrypt KEK 包裹（用当前 active KEK，AES-GCM，nonce 前缀于输出）。
func (k *LocalKms) Encrypt(_ context.Context, plaintext []byte) ([]byte, error) {
	k.mu.Lock()
	kek, ok := k.keks[k.activeID]
	k.mu.Unlock()
	if !ok {
		return nil, errors.New("local_kms: active kek missing")
	}
	return aesGCMSeal(kek, plaintext)
}

// Decrypt KEK 解包：遍历所有历史 KEK 尝试（兼容轮换前的历史信封），首个成功即返回。
func (k *LocalKms) Decrypt(_ context.Context, blob []byte) ([]byte, error) {
	k.mu.Lock()
	keks := make([][]byte, 0, len(k.keks))
	for _, raw := range k.keks {
		keks = append(keks, raw)
	}
	k.mu.Unlock()
	var lastErr error
	for _, kek := range keks {
		if pt, err := aesGCMOpen(kek, blob); err == nil {
			return pt, nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("local_kms: no kek available")
	}
	return nil, lastErr
}

// RotateKEK 真实轮换：生成新 KEK 设为 active 并持久化；返回新 KEK 标识。
func (k *LocalKms) RotateKEK(_ context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	id := k.addKEK()
	if err := k.persist(); err != nil {
		return "", err
	}
	return id, nil
}

// aesGCMSeal AES-GCM 加密，nonce 前置到输出（标准库封装）。
func aesGCMSeal(kek, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(kek)
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
	// 把 nonce 前置到输出（nonce || ciphertext || tag），解密端据此还原 nonce。
	return gcm.Seal(iv, iv, plaintext, nil), nil
}

// aesGCMOpen AES-GCM 解密，nonce 从输入前置段取出。
func aesGCMOpen(kek, blob []byte) ([]byte, error) {
	block, err := aes.NewCipher(kek)
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
