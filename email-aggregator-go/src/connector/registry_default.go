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
//   - gmail     → 复用 RealIMAPConnector（默认指向 imap.gmail.com:993，cfg["address"] 可覆盖）
//   - exchange  → EWSConnector（Exchange / EWS，SOAP，纯标准库实现）
//
// 约定：factory 从 cfg 读取连接参数（address / useTLS / endpoint）。
// IMAP 必须携带 cfg["address"]，否则报错（避免静默降级到错误的默认主机）。
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
		addr := cfg["address"]
		if addr == "" {
			addr = "imap.gmail.com:993"
		}
		// Gmail 走 IMAP 协议（IMAP4 + STARTTLS / 993 隐式 TLS），复用真实 IMAP 适配器。
		return NewRealIMAPConnector(addr, true, nil), nil
	})

	reg.Register(model.ProviderExchange, func(_ model.Provider, cfg map[string]string) (model.Connector, error) {
		return NewEWSConnector(cfg["endpoint"]), nil
	})

	return reg
}
