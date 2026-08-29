//go:build integration

package integration

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"email-aggregator-go/src/model"
	"email-aggregator-go/src/tenant"
)

// PgAccountStore 真实 PG 账户注册表（连接/账户服务）：
// 账户身份 + 连接配置引用 + 生命周期状态；凭据仅存 KMS 引用（ADR-004），不落明文。
type PgAccountStore struct {
	pool *pgxpool.Pool
}

// NewPgAccountStore 连接 PG 构造账户仓储（复用 PgMetadataStore 的连接池习惯）。
func NewPgAccountStore(_ context.Context, dsn string) (*PgAccountStore, error) {
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		return nil, err
	}
	return &PgAccountStore{pool: pool}, nil
}

const accountCols = `id, tenant_id, provider, email, display_name, status, sync_folder, server_host, credentials_ref, last_sync_at, created_at, updated_at`

func scanAccount(row interface{ Scan(...interface{}) error }) (*model.Account, error) {
	var a model.Account
	var provider, tenantID, email, displayName, status, syncFolder, serverHost, credRef string
	var lastSync int64
	var createdAt, updatedAt time.Time
	if err := row.Scan(&a.ID, &tenantID, &provider, &email, &displayName, &status,
		&syncFolder, &serverHost, &credRef, &lastSync, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	a.TenantID = tenantID
	a.Provider = model.Provider(provider)
	a.Email = email
	a.DisplayName = displayName
	a.Status = model.AccountStatus(status)
	a.SyncFolder = syncFolder
	a.ServerHost = serverHost
	a.CredentialsRef = credRef
	a.LastSyncAt = lastSync
	a.CreatedAt = createdAt.UnixMilli()
	a.UpdatedAt = updatedAt.UnixMilli()
	return &a, nil
}

// Upsert 幂等写入（种子数据 / 同步回写 lastSyncAt 用），ON CONFLICT 更新非主键字段。
func (s *PgAccountStore) Upsert(tenantID string, a model.Account) error {
	tenantID = tenant.Resolve(tenantID)
	_, err := s.pool.Exec(context.Background(), `
		INSERT INTO mail_account
		  (id, tenant_id, provider, email, display_name, status, sync_folder, server_host, credentials_ref, last_sync_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (id) DO UPDATE SET
		  provider = EXCLUDED.provider, email = EXCLUDED.email,
		  display_name = EXCLUDED.display_name, status = EXCLUDED.status,
		  sync_folder = EXCLUDED.sync_folder, server_host = EXCLUDED.server_host,
		  credentials_ref = EXCLUDED.credentials_ref,
		  last_sync_at = EXCLUDED.last_sync_at, updated_at = now()`,
		a.ID, tenantID, string(a.Provider), a.Email, a.DisplayName, string(a.Status),
		a.SyncFolder, a.ServerHost, a.CredentialsRef, a.LastSyncAt)
	return err
}

// ListAccounts 列出租户内账户注册表（按 ID 排序）。
func (s *PgAccountStore) ListAccounts(tenantID string) ([]model.Account, error) {
	tenantID = tenant.Resolve(tenantID)
	rows, err := s.pool.Query(context.Background(),
		`SELECT `+accountCols+` FROM mail_account WHERE tenant_id=$1 ORDER BY id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// GetAccount 按租户+账户取注册表条目；不存在返回 nil,nil。
func (s *PgAccountStore) GetAccount(tenantID, accountID string) (*model.Account, error) {
	tenantID = tenant.Resolve(tenantID)
	row := s.pool.QueryRow(context.Background(),
		`SELECT `+accountCols+` FROM mail_account WHERE id=$1 AND tenant_id=$2`, accountID, tenantID)
	a, err := scanAccount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// CreateAccount 新建账户；已存在返回错误。
func (s *PgAccountStore) CreateAccount(tenantID string, a model.Account) error {
	tenantID = tenant.Resolve(tenantID)
	_, err := s.pool.Exec(context.Background(), `
		INSERT INTO mail_account
		  (id, tenant_id, provider, email, display_name, status, sync_folder, server_host, credentials_ref, last_sync_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		a.ID, tenantID, string(a.Provider), a.Email, a.DisplayName, string(a.Status),
		a.SyncFolder, a.ServerHost, a.CredentialsRef, a.LastSyncAt)
	return err
}

// UpdateAccount 更新账户（保留 CreatedAt）。
func (s *PgAccountStore) UpdateAccount(tenantID string, a model.Account) error {
	tenantID = tenant.Resolve(tenantID)
	_, err := s.pool.Exec(context.Background(), `
		UPDATE mail_account SET provider=$3, email=$4, display_name=$5, status=$6,
		  sync_folder=$7, server_host=$8, credentials_ref=$9, last_sync_at=$10, updated_at=now()
		WHERE id=$1 AND tenant_id=$2`,
		a.ID, tenantID, string(a.Provider), a.Email, a.DisplayName, string(a.Status),
		a.SyncFolder, a.ServerHost, a.CredentialsRef, a.LastSyncAt)
	return err
}

// DeleteAccount 删除账户。
func (s *PgAccountStore) DeleteAccount(tenantID, accountID string) error {
	tenantID = tenant.Resolve(tenantID)
	_, err := s.pool.Exec(context.Background(),
		`DELETE FROM mail_account WHERE id=$1 AND tenant_id=$2`, accountID, tenantID)
	return err
}

// TouchSync 记录最近一次同步时间（编排器每次同步后调用）。
func (s *PgAccountStore) TouchSync(tenantID, accountID string) error {
	tenantID = tenant.Resolve(tenantID)
	_, err := s.pool.Exec(context.Background(),
		`UPDATE mail_account SET last_sync_at=$3, updated_at=now() WHERE id=$1 AND tenant_id=$2`,
		accountID, tenantID, time.Now().UnixMilli())
	return err
}
