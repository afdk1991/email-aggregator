// Package store 存储层：元数据仓储接口与 PoC 内存实现（ADR-003）。
// 生产按 account_id 分片；真实 PG 实现见 integration.PgMetadataStore（//go:build integration）。
package store

import (
	"fmt"
	"sync"

	"email-aggregator-go/src/model"
	"email-aggregator-go/src/tenant"
)

// ns 租户命名空间前缀：把所有数据面键（邮件幂等键 / 账户 / 游标）都收敛到 tenant 之下，
// 实现「逻辑多租户」隔离边界（ADR-009）。单租户下 tenantID=default，等价于改造前的 account 隔离。
func ns(tenantID, key string) string { return tenantID + "\x00" + key }

// MetadataStore 元数据仓储接口（ADR-009：tenantID 全链路首参强制透传）
type MetadataStore interface {
	UpsertMail(tenantID string, m model.CanonicalMail) error
	GetMail(tenantID, accountID, idempotencyKey string) (*model.CanonicalMail, error)
	ListMails(tenantID, accountID, folder string, limit int) ([]model.CanonicalMail, error)
	ListAccounts(tenantID string) ([]string, error)
	Count(tenantID, accountID string) (int, error)
	SetRead(tenantID, accountID, id string, read bool) error
	DeleteMail(tenantID, accountID, id string) error
	UnreadCount(tenantID, accountID string) (int, error)
	CursorStore // 同步游标持久化：重启后增量续拉，避免重复全量
}

// CursorStore 同步游标持久化（按 tenant+account+folder）
type CursorStore interface {
	GetCursor(tenantID, accountID, folder string) (model.SyncCursor, error)
	PutCursor(tenantID, accountID, folder string, cursor model.SyncCursor) error
}

// InMemoryMetadataStore 演示/单测用内存实现（幂等去重）
type InMemoryMetadataStore struct {
	mu      sync.RWMutex
	byKey   map[string]model.CanonicalMail
	byAcct  map[string][]model.CanonicalMail
	cursors map[string]model.SyncCursor
}

// NewInMemoryMetadataStore 构造
func NewInMemoryMetadataStore() *InMemoryMetadataStore {
	return &InMemoryMetadataStore{
		byKey:   map[string]model.CanonicalMail{},
		byAcct:  map[string][]model.CanonicalMail{},
		cursors: map[string]model.SyncCursor{},
	}
}

// UpsertMail 幂等写入（按 tenant+id 去重；强制盖上租户归属，保证隔离边界）
func (s *InMemoryMetadataStore) UpsertMail(tenantID string, m model.CanonicalMail) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tenantID = tenant.Resolve(tenantID)
	m.TenantID = tenantID
	key := ns(tenantID, m.ID) // 简化：以邮件 ID 作幂等键（真实用 tenant:accountId:mailId 或 provider 稳定 ID）
	if _, ok := s.byKey[key]; ok {
		return nil // 已存在，幂等跳过
	}
	s.byKey[key] = m
	s.byAcct[ns(tenantID, m.AccountID)] = append(s.byAcct[ns(tenantID, m.AccountID)], m)
	return nil
}

// GetMail 按租户+账户+幂等键取邮件
func (s *InMemoryMetadataStore) GetMail(tenantID, accountID, key string) (*model.CanonicalMail, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tenantID = tenant.Resolve(tenantID)
	m, ok := s.byKey[ns(tenantID, key)]
	if !ok || m.AccountID != accountID {
		return nil, nil
	}
	return &m, nil
}

// ListMails 列出租户内某账户某文件夹邮件（按时间倒序，截取 limit）
func (s *InMemoryMetadataStore) ListMails(tenantID, accountID, folder string, limit int) ([]model.CanonicalMail, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tenantID = tenant.Resolve(tenantID)
	all := s.byAcct[ns(tenantID, accountID)]
	out := make([]model.CanonicalMail, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Folder == folder {
			out = append(out, all[i])
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Count 租户内账户邮件数
func (s *InMemoryMetadataStore) Count(tenantID, accountID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tenantID = tenant.Resolve(tenantID)
	return len(s.byAcct[ns(tenantID, accountID)]), nil
}

// ListAccounts 列出租户内当前已写入邮件的所有账户 ID（按首次出现顺序，稳定可演示）。
func (s *InMemoryMetadataStore) ListAccounts(tenantID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tenantID = tenant.Resolve(tenantID)
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, m := range s.byKey {
		if m.TenantID != tenantID {
			continue
		}
		if _, ok := seen[m.AccountID]; ok {
			continue
		}
		seen[m.AccountID] = struct{}{}
		out = append(out, m.AccountID)
	}
	return out, nil
}

// SetRead 设置邮件已读/未读状态（CRUD 生命周期）。
func (s *InMemoryMetadataStore) SetRead(tenantID, accountID, id string, read bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tenantID = tenant.Resolve(tenantID)
	m, ok := s.byKey[ns(tenantID, id)]
	if !ok || m.AccountID != accountID {
		return fmt.Errorf("mail not found")
	}
	m.Read = read
	s.byKey[ns(tenantID, id)] = m
	acct := s.byAcct[ns(tenantID, accountID)]
	for i := range acct {
		if acct[i].ID == id {
			acct[i].Read = read
			break
		}
	}
	return nil
}

// DeleteMail 删除邮件（软删除演示：从内存索引移除）。
func (s *InMemoryMetadataStore) DeleteMail(tenantID, accountID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tenantID = tenant.Resolve(tenantID)
	m, ok := s.byKey[ns(tenantID, id)]
	if !ok || m.AccountID != accountID {
		return fmt.Errorf("mail not found")
	}
	delete(s.byKey, ns(tenantID, id))
	acct := s.byAcct[ns(tenantID, accountID)]
	out := make([]model.CanonicalMail, 0, len(acct))
	for _, x := range acct {
		if x.ID != id {
			out = append(out, x)
		}
	}
	s.byAcct[ns(tenantID, accountID)] = out
	return nil
}

// UnreadCount 租户内账户未读邮件数。
func (s *InMemoryMetadataStore) UnreadCount(tenantID, accountID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tenantID = tenant.Resolve(tenantID)
	n := 0
	for _, m := range s.byAcct[ns(tenantID, accountID)] {
		if !m.Read {
			n++
		}
	}
	return n, nil
}

// GetCursor 取游标
func (s *InMemoryMetadataStore) GetCursor(tenantID, accountID, folder string) (model.SyncCursor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tenantID = tenant.Resolve(tenantID)
	c, ok := s.cursors[ns(tenantID, accountID)+"|"+folder]
	if !ok {
		return model.SyncCursor{}, nil
	}
	return c, nil
}

// PutCursor 存游标
func (s *InMemoryMetadataStore) PutCursor(tenantID, accountID, folder string, cursor model.SyncCursor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tenantID = tenant.Resolve(tenantID)
	s.cursors[ns(tenantID, accountID)+"|"+folder] = cursor
	return nil
}

// 真实 PG 实现见 integration/integration.go 中的 PgMetadataStore（//go:build integration）。
// 替换要点（与 TS 版一致）：
//
//	INSERT ... ON CONFLICT (idempotency_key) DO NOTHING;
//	游标表 account_sync_cursor(account_id, folder, cursor_json)；按 account_id 分片（Citus）。
