// Package connector 实现适配器注册表（ADR-005）与 IMAP 适配器骨架。
// 新增协议（POP3/Exchange/Gmail/企业）只需注册一个 factory，零侵入。
package connector

import (
	"fmt"

	"email-aggregator-go/src/model"
)

// ConnectorRegistry 适配器注册表
type ConnectorRegistry struct {
	factories map[model.Provider]model.ConnectorFactory
}

// NewConnectorRegistry 构造
func NewConnectorRegistry() *ConnectorRegistry {
	return &ConnectorRegistry{factories: map[model.Provider]model.ConnectorFactory{}}
}

// Register 注册某协议的构造器
func (r *ConnectorRegistry) Register(p model.Provider, f model.ConnectorFactory) {
	r.factories[p] = f
}

// Create 按协议创建适配器（找不到即报错，避免静默降级到错误实现）
func (r *ConnectorRegistry) Create(p model.Provider, cfg map[string]string) (model.Connector, error) {
	f, ok := r.factories[p]
	if !ok {
		return nil, fmt.Errorf("no connector registered for provider %s", p)
	}
	return f(p, cfg)
}
