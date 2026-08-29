// Package store 存储层：账户注册表（连接/账户服务）。
// 生产按 tenant_id 隔离；真实 PG 实现见 integration.PgAccountStore（//go:build integration）。
package store

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"email-aggregator-go/src/model"
	"email-aggregator-go/src/tenant"
)

// AccountStore 账户注册表仓储接口（ADR-009：tenantID 全链路首参强制透传）。
type AccountStore interface {
	ListAccounts(tenantID string) ([]model.Account, error)
	GetAccount(tenantID, accountID string) (*model.Account, error)
	CreateAccount(tenantID string, a model.Account) error
	UpdateAccount(tenantID string, a model.Account) error
	DeleteAccount(tenantID, accountID string) error
	// Upsert 幂等写入（种子数据 / 同步回写 lastSyncAt 用）。
	Upsert(tenantID string, a model.Account) error
}

// InMemoryAccountStore 演示/单测用内存实现。
type InMemoryAccountStore struct {
	mu sync.RWMutex
	// key: tenantID\x00accountID
	m map[string]model.Account
}

// NewInMemoryAccountStore 构造。
func NewInMemoryAccountStore() *InMemoryAccountStore {
	return &InMemoryAccountStore{m: map[string]model.Account{}}
}

// Upsert 幂等写入（供种子数据/同步回写 lastSyncAt 使用）。
func (s *InMemoryAccountStore) Upsert(tenantID string, a model.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tenantID = tenant.Resolve(tenantID)
	a.TenantID = tenantID
	now := time.Now().UnixMilli()
	if a.CreatedAt == 0 {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	s.m[ns(tenantID, a.ID)] = a
	return nil
}

// ListAccounts 返回租户内全部账户（按 ID 排序，稳定输出）。
func (s *InMemoryAccountStore) ListAccounts(tenantID string) ([]model.Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tenantID = tenant.Resolve(tenantID)
	var out []model.Account
	for _, a := range s.m {
		if a.TenantID == tenantID {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// GetAccount 按租户+账户取注册表条目；不存在返回 nil,nil。
func (s *InMemoryAccountStore) GetAccount(tenantID, accountID string) (*model.Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tenantID = tenant.Resolve(tenantID)
	a, ok := s.m[ns(tenantID, accountID)]
	if !ok {
		return nil, nil
	}
	return &a, nil
}

// CreateAccount 新建账户；已存在返回错误（幂等语义由调用方决定）。
func (s *InMemoryAccountStore) CreateAccount(tenantID string, a model.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tenantID = tenant.Resolve(tenantID)
	if _, ok := s.m[ns(tenantID, a.ID)]; ok {
		return fmt.Errorf("account %q already exists", a.ID)
	}
	a.TenantID = tenantID
	now := time.Now().UnixMilli()
	a.CreatedAt = now
	a.UpdatedAt = now
	s.m[ns(tenantID, a.ID)] = a
	return nil
}

// UpdateAccount 更新账户（保留 CreatedAt，刷新 UpdatedAt）。
func (s *InMemoryAccountStore) UpdateAccount(tenantID string, a model.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tenantID = tenant.Resolve(tenantID)
	key := ns(tenantID, a.ID)
	old, ok := s.m[key]
	if !ok {
		return fmt.Errorf("account %q not found", a.ID)
	}
	a.TenantID = tenantID
	a.CreatedAt = old.CreatedAt
	a.UpdatedAt = time.Now().UnixMilli()
	s.m[key] = a
	return nil
}

// DeleteAccount 删除账户；不存在返回错误。
func (s *InMemoryAccountStore) DeleteAccount(tenantID, accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tenantID = tenant.Resolve(tenantID)
	key := ns(tenantID, accountID)
	if _, ok := s.m[key]; !ok {
		return fmt.Errorf("account %q not found", accountID)
	}
	delete(s.m, key)
	return nil
}
