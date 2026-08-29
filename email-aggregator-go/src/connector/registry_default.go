package connector

import (
	"fmt"

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
func NewDefaultRegistry() *ConnectorRegistry {
	reg := NewConnectorRegistry()

	reg.Register(model.ProviderIMAP, func(_ model.Provider, cfg map[string]string) (model.Connector, error) {
		addr := cfg["address"]
		if addr == "" {
			return nil, fmt.Errorf("imap connector requires cfg[\"address\"]")
		}
		return NewRealIMAPConnector(addr, cfg["useTLS"] == "true", nil), nil
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
		return NewRealPOP3Connector(addr, cfg["useTLS"] == "true", nil), nil
	})

	return reg
}
