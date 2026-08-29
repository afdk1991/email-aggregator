package connector

import (
	"crypto/tls"
	"fmt"
	"strings"

	"email-aggregator-go/src/model"
)

// NewDefaultRegistry 构造「默认注册表」：把项目已落地的真实连接器按协议注册，
// 使 Orchestrator / 调度层无需感知具体适配器实现即可按 Provider 创建连接器（ADR-005）。
//
// 注册映射：
//   - imap      → RealIMAPConnector（真实 IMAP，纯标准库实现，零第三方依赖）
//   - gmail     → RealGmailConnector（Gmail API REST + OAuth2 Bearer + historyId 增量，
//               架构 §10/§12「走官方 API」；cfg["endpoint"] 可覆盖基址，默认生产 Gmail API）
//   - exchange  → EWSConnector（Exchange / EWS，SOAP，纯标准库实现）
//   - pop3      → RealPOP3Connector（真实 POP3，RFC 1939，纯标准库实现；默认 110 端口可 STLS，995 隐式 TLS 用 useTLS=true）
//
// 约定：factory 从 cfg 读取连接参数（address / useTLS / endpoint）。
// IMAP/POP3 必须携带 cfg["address"]，否则报错（避免静默降级到错误的默认主机）。
// Gmail 走 OAuth2 Bearer，由 Connect(cred.OAuth) 提供；endpoint 缺省为生产 Gmail API。
// 未在此注册的协议（如 enterprise）由调用方按需 Register，保持零侵入。
// tlsVersionHint 解析 tlsMaxVersion 配置（tls10/tls11/tls12/tls13），缺省 TLS1.2。
func tlsVersionHint(v string) uint16 {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "tls10", "tls1.0":
		return tls.VersionTLS10
	case "tls11", "tls1.1":
		return tls.VersionTLS11
	case "tls13", "tls1.3":
		return tls.VersionTLS13
	default:
		return tls.VersionTLS12
	}
}

// tlsCfgFrom 依据 cfg 构造 TLS 配置。默认 Min=TLS1.2、Max=TLS1.2：
// 兼容国内大量 TLS1.2-only 的邮件服务器（139/163 等实测拒绝 TLS1.3 ClientHello）。
// 需要 TLS1.3 时传 cfg["tlsMaxVersion"]="tls13" 显式提升。ServerName 由连接器构造器兜底。
//
// 显式 CipherSuites：Go 模块默认（go.mod go 1.22.0 语义）禁用 RSA 密钥交换
// （GODEBUG tlsrsakex=0），ClientHello 不含任何 TLS_RSA_WITH_* 套件；而
// imap.139.com:993 等服务器实测仅支持 RSA 密钥交换（协商出
// TLS_RSA_WITH_AES_128_GCM_SHA256），无 RSA 套件时直接 tls: handshake failure。
// 故此处显式列出：ECDHE（现代服务器首选，无前向机密性损失）+ RSA kex（139/163 兼容必需，
// 已排除 3DES 弱套件）。TLS1.3 套件由 Go 内部统一处理，不受本列表影响。
func tlsCfgFrom(cfg map[string]string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		MaxVersion: tlsVersionHint(cfg["tlsMaxVersion"]),
		CipherSuites: []uint16{
			// ECDHE（优先）
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			// RSA 密钥交换（139/163 等仅支持 RSA kex 的服务器必需）
			tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		},
	}
}

func NewDefaultRegistry() *ConnectorRegistry {
	reg := NewConnectorRegistry()

	reg.Register(model.ProviderIMAP, func(_ model.Provider, cfg map[string]string) (model.Connector, error) {
		addr := cfg["address"]
		if addr == "" {
			return nil, fmt.Errorf("imap connector requires cfg[\"address\"]")
		}
		return NewRealIMAPConnector(addr, cfg["useTLS"] == "true", tlsCfgFrom(cfg)), nil
	})

	reg.Register(model.ProviderGmail, func(_ model.Provider, cfg map[string]string) (model.Connector, error) {
		// Gmail API REST（非 IMAP 复用）；endpoint 缺省走生产 gmail.googleapis.com
		return NewRealGmailConnector(cfg["endpoint"]), nil
	})

	reg.Register(model.ProviderExchange, func(_ model.Provider, cfg map[string]string) (model.Connector, error) {
		return NewEWSConnector(cfg["endpoint"]), nil
	})

	// Exchange/Graph 增强：Exchange Online / Microsoft 365 走官方 Graph API（现代增量 delta），
	// 与 EWS（本地 Exchange on-prem）双模并存；endpoint 缺省走生产 graph.microsoft.com/v1.0。
	reg.Register(model.ProviderGraph, func(_ model.Provider, cfg map[string]string) (model.Connector, error) {
		return NewRealGraphConnector(cfg["endpoint"]), nil
	})

	reg.Register(model.ProviderPOP3, func(_ model.Provider, cfg map[string]string) (model.Connector, error) {
		addr := cfg["address"]
		if addr == "" {
			return nil, fmt.Errorf("pop3 connector requires cfg[\"address\"]")
		}
		return NewRealPOP3Connector(addr, cfg["useTLS"] == "true", tlsCfgFrom(cfg)), nil
	})

	return reg
}
