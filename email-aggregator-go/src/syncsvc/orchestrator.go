package syncsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"email-aggregator-go/src/events"
	"email-aggregator-go/src/model"
	"email-aggregator-go/src/security"
	"email-aggregator-go/src/store"
	"email-aggregator-go/src/tenant"
)

// OrchestratorDeps 编排器依赖（接口注入，便于测试与替换总线）
type OrchestratorDeps struct {
	Bus          events.EventBus
	Vault        *security.CredentialVault
	BackoffMaxMs int
	Connector    model.Connector   // 协议适配器（IMAP/POP3/Exchange/Gmail）
	Credential   model.Credential  // 连接凭据（运行时内存态；生产环境由 Vault 解密注入）
	Accounts     store.AccountStore // 可选：连接/账户服务（未挂载则不做状态门控，向后兼容）
}

// Orchestrator 同步编排器：消费 sync-tasks，推进状态机，驱动 Connector 采集，发布 mail-ingested
type Orchestrator struct {
	deps         OrchestratorDeps
	state        SyncState
	backoffMaxMs int
}

// NewOrchestrator 构造
func NewOrchestrator(deps OrchestratorDeps) *Orchestrator {
	max := deps.BackoffMaxMs
	if max == 0 {
		max = 60000
	}
	return &Orchestrator{deps: deps, state: StateUnconnected, backoffMaxMs: max}
}

// State 当前状态
func (o *Orchestrator) State() SyncState { return o.state }

// busSink 采集回传适配器：将 Connector 产出的 CanonicalMail 转为 mail-ingested 事件并发布到总线。
// 实现 model.SyncSink 接口，替换原先散落在 cmd/demo.go 的 demoSink。
//
// ADR-009 多租户：sink 在构造时绑定 tenantID（来自 SyncTaskEvent.TenantID），
// 采集回传每封邮件强制盖上租户归属，确保下游 store/index/notify 的隔离边界
// 不依赖 Connector 是否填充 TenantID（协议适配层无租户语义）。
type busSink struct {
	bus       events.EventBus
	tenantID  string // ADR-009：绑定租户，回传邮件强制归属
	accountID string
}

func newBusSink(bus events.EventBus, tenantID, accountID string) *busSink {
	return &busSink{bus: bus, tenantID: tenant.Resolve(tenantID), accountID: accountID}
}

// OnMessage 收到一封邮件 → 盖租户戳 → 构造 mail-ingested 事件 → 总线发布
// 幂等去重键采用 tenantId:accountId:mailId（与 contracts.go 注释一致），防止跨租户碰撞。
func (s *busSink) OnMessage(ctx context.Context, m model.CanonicalMail) error {
	// 强制盖租户戳：Connector 无租户语义，sink 是数据面隔离的入口
	m.TenantID = s.tenantID
	m.AccountID = s.accountID

	key, payload, err := buildMailIngested(m)
	if err != nil {
		return fmt.Errorf("build mail-ingested: %w", err)
	}
	return s.bus.Publish(ctx, events.TopicMailIngested, key, payload)
}

// OnDelete 收到删除通知（当前仅日志，后续可扩展为 delete 事件）
func (s *busSink) OnDelete(_ context.Context, id string) error {
	fmt.Printf("  [sink] delete tenant=%s account=%s mail=%s\n", s.tenantID, s.accountID, id)
	return nil
}

// HandleSyncTask 处理一条 sync-tasks 事件：驱动状态机 + 连接 + 采集 + 发布事件。
//
// 完整闭环：
//  1. UNCONNECTED --AUTHORIZE--> AUTHORIZING（调用 Connector.Connect 建立连接）
//  2. AUTHORIZING --FULL_DONE--> INITIAL_FULL（初始全量同步开始）
//  3. Connector.InitialFullSync → 经 busSink 发布 mail-ingested
//  4. INITIAL_FULL --FULL_DONE--> INCREMENTAL（全量完成，进入增量模式）
//  5. 增量模式下 Connector.IncrementalSync → 经 busSink 发布新增邮件
//
// 任意步骤失败 → 状态机迁移至 ERROR，返回错误（调用方可使用 Backoff 重试）。
func (o *Orchestrator) HandleSyncTask(ctx context.Context, task events.SyncTaskEvent) error {
	// ── 连接/账户服务门控：paused 跳过，error 拦截，未注册账户向后兼容放行 ──
	if o.deps.Accounts != nil {
		acct, err := o.deps.Accounts.GetAccount(tenant.Resolve(task.TenantID), task.AccountID)
		if err != nil {
			return fmt.Errorf("account registry lookup (account=%s): %w", task.AccountID, err)
		}
		if acct != nil {
			switch acct.Status {
			case model.AccountPaused:
				fmt.Printf("[orchestrator] account=%s status=paused -> skip sync\n", task.AccountID)
				return nil
			case model.AccountError:
				return fmt.Errorf("account=%s status=error -> sync blocked, needs manual recovery", task.AccountID)
			}
		}
	}

	// ── Phase 1: UNCONNECTED → AUTHORIZING ──
	next, err := Advance(o.state, EvAuthorize)
	if err != nil {
		return fmt.Errorf("state transition UNCONNECTED→AUTHORIZING: %w", err)
	}
	o.state = next

	// ── Phase 2: 连接协议适配器（AUTHORIZING 阶段核心动作）──
	if o.deps.Connector != nil {
		if err := o.deps.Connector.Connect(ctx, o.deps.Credential); err != nil {
			o.transitionError()
			return fmt.Errorf("connector connect failed (account=%s): %w", task.AccountID, err)
		}
	}
	// Vault 存在时标记凭据已解密使用（真实环境从 Vault.Unseal 取出并喂给 Connector）
	_ = o.deps.Vault

	// ── Phase 3: AUTHORIZING → INITIAL_FULL（授权完成，进入初始全量阶段）──
	next, err = Advance(o.state, EvFullDone)
	if err != nil {
		return fmt.Errorf("state transition AUTHORIZING→INITIAL_FULL: %w", err)
	}
	o.state = next

	// ── Phase 4: 执行同步采集 ──
	// ADR-009：sink 绑定 task.TenantID，采集回传邮件强制盖上租户戳，
	// 确保 mail-ingested 事件携带 TenantID 透传至 store/index/notify。
	sink := newBusSink(o.deps.Bus, task.TenantID, task.AccountID)

	if task.Mode == "initial" {
		// 全量采集
		if o.deps.Connector != nil {
			since := int64(0) // 全量从最早开始；生产环境可从 task.Cursor 读取
			if err := o.deps.Connector.InitialFullSync(ctx, since, sink); err != nil {
				o.transitionError()
				return fmt.Errorf("initial full sync failed (account=%s): %w", task.AccountID, err)
			}
		}
		// 全量完成 → INITIAL_FULL → INCREMENTAL
		next, err = Advance(o.state, EvFullDone)
		if err != nil {
			return fmt.Errorf("state transition INITIAL_FULL→INCREMENTAL: %w", err)
		}
		o.state = next
	} else if task.Mode == "incremental" {
		// 先跳过 INITIAL_FULL → INCREMENTAL（增量任务隐含全量已完成）
		next, err = Advance(o.state, EvFullDone)
		if err != nil {
			return fmt.Errorf("state transition INITIAL_FULL→INCREMENTAL: %w", err)
		}
		o.state = next
		// 增量采集
		if o.deps.Connector != nil {
			if err := o.deps.Connector.IncrementalSync(ctx, task.Cursor, sink); err != nil {
				o.transitionError()
				return fmt.Errorf("incremental sync failed (account=%s): %w", task.AccountID, err)
			}
		}
	}

	fmt.Printf("[orchestrator] tenant=%s account=%s mode=%s -> state=%s\n",
		tenant.Resolve(task.TenantID), task.AccountID, task.Mode, o.state)

	// 同步成功后回写账户最近同步时间（连接/账户服务，未挂载则跳过）
	if o.deps.Accounts != nil {
		if acct, err := o.deps.Accounts.GetAccount(tenant.Resolve(task.TenantID), task.AccountID); err == nil && acct != nil {
			_ = o.deps.Accounts.Upsert(tenant.Resolve(task.TenantID), model.Account{
				ID: task.AccountID, Provider: acct.Provider, Email: acct.Email,
				DisplayName: acct.DisplayName, Status: acct.Status, SyncFolder: acct.SyncFolder,
				ServerHost: acct.ServerHost, CredentialsRef: acct.CredentialsRef, LastSyncAt: time.Now().UnixMilli(),
			})
		}
	}
	return nil
}

// transitionError 将状态机迁移至 ERROR（容错：若迁移失败则仅记录）
func (o *Orchestrator) transitionError() {
	if next, err := Advance(o.state, EvFail); err == nil {
		o.state = next
	}
}

// Backoff 指数退避（上限由 BackoffMaxMs 约束）
func (o *Orchestrator) Backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt)) * 100 * time.Millisecond
	cap := time.Duration(o.backoffMaxMs) * time.Millisecond
	if d > cap {
		d = cap
	}
	return d
}

// buildMailIngested 由 CanonicalMail 构造并序列化 mail-ingested 事件。
// ADR-009：写入 m.TenantID，幂等去重键采用 tenantId:accountId:mailId
// （与 contracts.go EventEnvelope.Key 注释一致），避免跨租户同 accountId+mailId 碰撞。
func buildMailIngested(m model.CanonicalMail) (string, []byte, error) {
	tid := tenant.Resolve(m.TenantID)
	ev := events.MailIngestedEvent{
		TenantID:      tid,
		AccountID:     m.AccountID,
		MailID:        m.ID,
		Folder:        m.Folder,
		From:          m.From.Email,
		Subject:       m.Subject,
		BodyText:      m.BodyText,
		InternalDate:  m.InternalDate,
		RawObjectKey:  m.RawObjectKey,
		SizeBytes:     m.SizeBytes,
		HasAttachment: m.HasAttachment,
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return "", nil, err
	}
	// 幂等键：tenantId:accountId:mailId —— 跨租户同 accountId+mailId 不会碰撞
	return tid + ":" + m.AccountID + ":" + m.ID, payload, nil
}
