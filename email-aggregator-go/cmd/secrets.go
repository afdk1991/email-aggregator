package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"email-aggregator-go/src/security"
)

// newCredentialVault 基于环境构造信封加密保险库（ADR-004 运行期落地）。
// 优先级：KMS_KEYRING_FILE（持久化多版本本地 KMS）> KMS_KEYRING（单 KEK 本地 KMS）>
// 内存演示 KMS（随机 KEK，进程退出即丢）。生产环境 KEK 应由真实云 KMS / Vault Transit 注入，绝不落盘。
func newCredentialVault() *security.CredentialVault {
	if f := os.Getenv("KMS_KEYRING_FILE"); f != "" {
		if k, err := security.NewLocalKmsFromFile(f); err == nil {
			return security.NewCredentialVault(k)
		}
	}
	if k := os.Getenv("KMS_KEYRING"); k != "" {
		return security.NewCredentialVault(security.NewLocalKmsWithKey(k))
	}
	return security.NewCredentialVault(security.NewLocalKmsInMemory())
}

// resolveSecret 解析敏感凭据，打通 ADR-004 的运行期落地：
//   - 若值为信封加密 JSON（以 "{" 开头且含 kekId 字段），用保险库拆封；
//   - 否则当作明文（向后兼容 PoC 形态）。
//
// 返回运行时明文，供下游适配器（OpenSearch / MinIO 等）建立连接使用。
// 静态配置/环境变量中可只保留信封，明文仅在进程内存中短暂存在、零落盘。
func resolveSecret(ctx context.Context, vault *security.CredentialVault, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		return raw, nil
	}
	var env security.Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return "", fmt.Errorf("secret 不是合法信封: %w", err)
	}
	pt, err := vault.Unseal(ctx, env)
	if err != nil {
		return "", fmt.Errorf("secret 拆封失败: %w", err)
	}
	return string(pt), nil
}
