package main

// accountSyncer 账户真实同步执行器（实现 api.AccountSyncer）。
// 手动「立即同步」时：按账户 provider + server_host 构造真实连接器 →
// Vault 拆封凭据（KMS 信封，ADR-004）→ 编排器 HandleSyncTask（全量采集）
// → mail-ingested → 既有摄取管线（PG 落库 + MinIO 内容寻址 + OpenSearch 索引 + 实时推送）。
// 与后台 sync-tasks 调度环解耦：本执行器仅服务运维入口，二者共享同一编排器语义。

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"email-aggregator-go/src/api"
	"email-aggregator-go/src/connector"
	"email-aggregator-go/src/events"
	"email-aggregator-go/src/model"
	"email-aggregator-go/src/security"
	"email-aggregator-go/src/store"
	"email-aggregator-go/src/syncsvc"
	"email-aggregator-go/src/tenant"
)

type accountSyncer struct {
	bus      events.EventBus
	vault    *security.CredentialVault
	accounts store.AccountStore
	metadata store.CursorStore // 可选：同步游标持久化（nil 则每次全量）
	reg      *connector.ConnectorRegistry

	mu       sync.Mutex
	running  map[string]bool
	statuses map[string]api.SyncStatus // key: tenantID\x00accountID
}

func newAccountSyncer(bus events.EventBus, vault *security.CredentialVault, accts store.AccountStore, meta store.CursorStore, reg *connector.ConnectorRegistry) *accountSyncer {
	return &accountSyncer{
		bus: bus, vault: vault, accounts: accts, metadata: meta, reg: reg,
		running:  map[string]bool{},
		statuses: map[string]api.SyncStatus{},
	}
}

func (s *accountSyncer) key(tid, accountID string) string {
	return tenant.Resolve(tid) + "\x00" + accountID
}

// StartSync 后台启动一次真实同步（立即返回）；状态经 SyncStatus 查询。
func (s *accountSyncer) StartSync(tid, accountID string) error {
	acct, err := s.accounts.GetAccount(tenant.Resolve(tid), accountID)
	if err != nil {
		return err
	}
	if acct == nil {
		return fmt.Errorf("account not found: %s", accountID)
	}
	if acct.CredentialsRef == "" {
		return fmt.Errorf("account %s 未配置凭据 — 请先录入授权码", accountID)
	}
	switch acct.Status {
	case model.AccountPaused:
		return fmt.Errorf("account %s 已暂停 — 请先「恢复同步」", accountID)
	case model.AccountError:
		return fmt.Errorf("account %s 处于异常态 — 请先「恢复同步」", accountID)
	}
	k := s.key(tid, accountID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[k] {
		return fmt.Errorf("account %s 同步已在运行中", accountID)
	}
	s.running[k] = true
	s.statuses[k] = api.SyncStatus{Running: true, TS: time.Now().UnixMilli()}
	go s.run(k, acct)
	return nil
}

// SyncStatus 返回最近一次同步状态。
func (s *accountSyncer) SyncStatus(tid, accountID string) api.SyncStatus {
	k := s.key(tid, accountID)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statuses[k]
}

func (s *accountSyncer) run(k string, acct *model.Account) {
	defer func() {
		s.mu.Lock()
		s.running[k] = false
		s.mu.Unlock()
	}()

	ctx := context.Background()
	tid := tenant.Resolve(acct.TenantID)

	// 1) 拆封凭据（credentialsRef = envelope JSON，明文零落盘）
	var env security.Envelope
	if err := json.Unmarshal([]byte(acct.CredentialsRef), &env); err != nil {
		s.fail(k, tid, acct, "凭据信封解析失败: "+err.Error())
		return
	}
	pt, err := s.vault.Unseal(ctx, env)
	if err != nil {
		s.fail(k, tid, acct, "凭据拆封失败: "+err.Error())
		return
	}
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(pt, &creds); err != nil {
		s.fail(k, tid, acct, "凭据载荷解析失败: "+err.Error())
		return
	}
	cred := model.Credential{Type: "password", Username: creds.Username, Password: creds.Password}

	// 2) 按 provider + server_host 构造真实连接器
	// 注册表约定：IMAP/POP3 读 cfg["address"]+cfg["useTLS"]；Gmail/Exchange/Graph 读 cfg["endpoint"]（缺省生产基址）。
	conn, err := s.reg.Create(acct.Provider, map[string]string{
		"address":  acct.ServerHost,
		"useTLS":   "true",
		"endpoint": acct.ServerHost,
	})
	if err != nil {
		s.fail(k, tid, acct, err.Error())
		return
	}

	// 3) 编排器同步 → mail-ingested → 摄取管线（PG/MinIO/OpenSearch/通知）
	// 游标持久化：已有游标 → 增量；无游标 → 初始全量（重启后免重复全量重拉）
	mode := "initial"
	if s.metadata != nil {
		if c, cerr := s.metadata.GetCursor(tid, acct.ID, "INBOX"); cerr == nil && (c.LastUID > 0 || c.HighWaterMark != "" || len(c.ProviderSpecific) > 0) {
			mode = "incremental"
		}
	}
	orch := syncsvc.NewOrchestrator(syncsvc.OrchestratorDeps{
		Bus:       s.bus,
		Vault:     s.vault,
		Connector: conn,
		Credential: cred,
		Accounts:   s.accounts,
		Cursor:     s.metadata,
	})
	if err := orch.HandleSyncTask(ctx, events.SyncTaskEvent{
		TenantID: tid, AccountID: acct.ID, Mode: mode,
	}); err != nil {
		s.fail(k, tid, acct, err.Error())
		return
	}

	s.mu.Lock()
	st := s.statuses[k]
	st.Running = false
	st.OK = true
	st.TS = time.Now().UnixMilli()
	s.statuses[k] = st
	s.mu.Unlock()
	fmt.Printf("[syncer] account=%s 全量同步完成\n", acct.ID)
}

func (s *accountSyncer) fail(k, tid string, acct *model.Account, msg string) {
	s.mu.Lock()
	s.statuses[k] = api.SyncStatus{Running: false, Error: msg, TS: time.Now().UnixMilli()}
	s.mu.Unlock()
	if acct.Status != model.AccountError {
		acct.Status = model.AccountError
		_ = s.accounts.UpdateAccount(tid, *acct)
	}
	fmt.Printf("[syncer] account=%s 同步失败: %s\n", acct.ID, msg)
}
