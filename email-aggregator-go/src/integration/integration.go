//go:build integration

// Package integration 真实基础设施适配器（生产骨架）。
//
// 本文件用构建标签 `integration` 保护：默认 `go build ./...` / `go test ./...`
// 不会编译它，因此仓库在零外部依赖（仅标准库）时即可构建与测试——这正是本
// 开发机（无 Go 工具链、无外网拉包）能保持 `go.mod` 干净的原因。
//
// 启用步骤见仓库根 INTEGRATION.md：
//   1) go get github.com/segmentio/kafka-go@latest \
//           github.com/jackc/pgx/v5@latest \
//           github.com/opensearch-project/opensearch-go/v2@latest \
//           github.com/minio/minio-go/v7@latest \
//           github.com/gorilla/websocket@latest
//   2) go build -tags integration ./...
//   3) 在 cmd 中以 integration.Wire 替换 InMemory 实现（见 cmd/server_integration.go）
//
// 所有适配器都实现与 stub 完全相同的接口（store.MetadataStore / store.ContentStore /
// search.SearchIndex / events.EventBus / notify.Notifier），故替换编排/采集逻辑零改动。
package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/opensearch-project/opensearch-go/v2"
	kafkago "github.com/segmentio/kafka-go"

	"email-aggregator-go/src/events"
	"email-aggregator-go/src/model"
	"email-aggregator-go/src/notify"
	"email-aggregator-go/src/observ"
	"email-aggregator-go/src/search"
	"email-aggregator-go/src/store"
	"email-aggregator-go/src/tenant"
)

// ───────────────────────────────────────────────────────────────────────────
// 配置
// ───────────────────────────────────────────────────────────────────────────

// Config 真实基础设施连接配置（通常由环境变量注入）。
type Config struct {
	PGDSN           string // postgres://user:pass@host:5432/db
	ObjectEndpoint  string // MinIO/S3 endpoint，如 localhost:9000
	ObjectBucket    string
	ObjectAccessKey string
	ObjectSecretKey string
	ObjectSecure    bool   // 是否 HTTPS
	OpenSearchAddr  string // http://localhost:9200
	OpenSearchUser  string // OpenSearch 安全插件启用时需基础鉴权（如 admin）
	OpenSearchPass  string
	KafkaBrokers    []string
	// KafkaDialAddr 可选拨号重写目标（如 "127.0.0.1:9092"）：broker advertised 通告容器名/内网名、
	// 而本进程在宿主机/跨网络无法解析时使用（生产级能力，同 Kafka e2e WithDial 机制）。
	KafkaDialAddr string
}

// RealAdapters 装配后的真实适配器集合（接口形态与 InMemory 版一致）。
type RealAdapters struct {
	Bus      events.EventBus
	Metadata store.MetadataStore
	Cursor   store.CursorStore
	Content  store.ContentStore
	Index    search.SearchIndex
	Notifier notify.Notifier
	Accounts store.AccountStore
}

// Wire 按配置装配真实适配器。返回后即可与编排器 / API 服务器 / 采集器对接。
func Wire(ctx context.Context, cfg Config) (*RealAdapters, error) {
	pg, err := NewPgMetadataStore(ctx, cfg.PGDSN)
	if err != nil {
		return nil, fmt.Errorf("pg: %w", err)
	}
	accounts, err := NewPgAccountStore(ctx, cfg.PGDSN)
	if err != nil {
		return nil, fmt.Errorf("pg accounts: %w", err)
	}
	obj, err := NewObjectContentStore(ctx, cfg.ObjectEndpoint, cfg.ObjectBucket,
		cfg.ObjectAccessKey, cfg.ObjectSecretKey, cfg.ObjectSecure)
	if err != nil {
		return nil, fmt.Errorf("object: %w", err)
	}
	os, err := NewOpenSearchIndex(cfg.OpenSearchAddr, cfg.OpenSearchUser, cfg.OpenSearchPass)
	if err != nil {
		return nil, fmt.Errorf("opensearch: %w", err)
	}
	hub := NewWsHub()
	kafkaBus := NewKafkaAdapter(cfg.KafkaBrokers)
	if cfg.KafkaDialAddr != "" {
		// advertised 通告容器名而本进程在宿主机无法解析时，把任意目标地址重写为宿主可达地址（生产级能力）。
		dialTarget := cfg.KafkaDialAddr
		kafkaBus = kafkaBus.WithDial(func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, dialTarget)
		})
	}
	return &RealAdapters{
		Bus:      kafkaBus,
		Metadata: pg,
		Cursor:   pg,
		Content:  obj,
		Index:    os,
		Notifier: hub,
		Accounts: accounts,
	}, nil
}

// ───────────────────────────────────────────────────────────────────────────
// Phase 2 / 蓝图 §11 服务拆分：按服务边界裁剪装配
// -------------------------------------------------------------------
// sync_worker  仅需 Kafka bus + PG accounts/cursor（读账户注册表 + 游标持久化），
//              不接 metadata/content/index/notifier（避免与 ingest_worker 双写竞争）。
// ingest_worker 仅需 Kafka bus + PG metadata/content + OpenSearch index + notifier（落库 + 索引 + 通知），
//              不接 accounts/cursor（账户状态由 sync_worker 维护）。
// search_service 仅需 OpenSearch index + PG metadata + observ（检索 + 指标端点），
//              不接 bus/content/notifier（无需事件订阅/通知/对象存储）。
// ───────────────────────────────────────────────────────────────────────────

// SyncAdapters sync_worker 进程所需的最小依赖集合。
// 注：Cursor 与 Accounts 共享同一 PG 连接池（PgMetadataStore 实现同时满足两接口）。
type SyncAdapters struct {
	Bus      events.EventBus
	Accounts store.AccountStore
	Cursor   store.CursorStore
}

// WireSync 装配 sync_worker 专用依赖（仅 PG accounts + cursor + Kafka）。
// 不建立 MinIO/OpenSearch/WS Hub 连接，启动更快、资源占用更少。
func WireSync(ctx context.Context, cfg Config) (*SyncAdapters, error) {
	pg, err := NewPgMetadataStore(ctx, cfg.PGDSN)
	if err != nil {
		return nil, fmt.Errorf("pg: %w", err)
	}
	accounts, err := NewPgAccountStore(ctx, cfg.PGDSN)
	if err != nil {
		return nil, fmt.Errorf("pg accounts: %w", err)
	}
	kafkaBus := NewKafkaAdapter(cfg.KafkaBrokers)
	if cfg.KafkaDialAddr != "" {
		dialTarget := cfg.KafkaDialAddr
		kafkaBus = kafkaBus.WithDial(func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, dialTarget)
		})
	}
	return &SyncAdapters{
		Bus:      kafkaBus,
		Accounts: accounts,
		Cursor:   pg, // PgMetadataStore 同时实现 store.CursorStore
	}, nil
}

// IngestAdapters ingest_worker 进程所需的最小依赖集合。
type IngestAdapters struct {
	Bus      events.EventBus
	Metadata store.MetadataStore
	Cursor   store.CursorStore
	Content  store.ContentStore
	Index    search.SearchIndex
	Notifier notify.Notifier
}

// WireIngest 装配 ingest_worker 专用依赖（PG metadata + MinIO content + OpenSearch index + WS notifier + Kafka）。
// 不建立 accounts 连接（账户状态由 sync_worker 维护，避免跨进程双写）。
func WireIngest(ctx context.Context, cfg Config) (*IngestAdapters, error) {
	pg, err := NewPgMetadataStore(ctx, cfg.PGDSN)
	if err != nil {
		return nil, fmt.Errorf("pg: %w", err)
	}
	obj, err := NewObjectContentStore(ctx, cfg.ObjectEndpoint, cfg.ObjectBucket,
		cfg.ObjectAccessKey, cfg.ObjectSecretKey, cfg.ObjectSecure)
	if err != nil {
		return nil, fmt.Errorf("object: %w", err)
	}
	osIdx, err := NewOpenSearchIndex(cfg.OpenSearchAddr, cfg.OpenSearchUser, cfg.OpenSearchPass)
	if err != nil {
		return nil, fmt.Errorf("opensearch: %w", err)
	}
	hub := NewWsHub()
	kafkaBus := NewKafkaAdapter(cfg.KafkaBrokers)
	if cfg.KafkaDialAddr != "" {
		dialTarget := cfg.KafkaDialAddr
		kafkaBus = kafkaBus.WithDial(func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, dialTarget)
		})
	}
	return &IngestAdapters{
		Bus:      kafkaBus,
		Metadata: pg,
		Cursor:   pg,
		Content:  obj,
		Index:    osIdx,
		Notifier: hub,
	}, nil
}

// SearchAdapters search_service 进程所需的最小依赖集合。
type SearchAdapters struct {
	Metadata store.MetadataStore
	Index    search.SearchIndex
}

// WireSearch 装配 search_service 专用依赖（PG metadata + OpenSearch index）。
// 不订阅 Kafka（search 是同步拉模型，无需事件循环）；不建立 MinIO/notifier。
func WireSearch(ctx context.Context, cfg Config) (*SearchAdapters, error) {
	pg, err := NewPgMetadataStore(ctx, cfg.PGDSN)
	if err != nil {
		return nil, fmt.Errorf("pg: %w", err)
	}
	osIdx, err := NewOpenSearchIndex(cfg.OpenSearchAddr, cfg.OpenSearchUser, cfg.OpenSearchPass)
	if err != nil {
		return nil, fmt.Errorf("opensearch: %w", err)
	}
	return &SearchAdapters{
		Metadata: pg,
		Index:    osIdx,
	}, nil
}

// ───────────────────────────────────────────────────────────────────────────
// PostgreSQL 元数据 + 游标仓储（store.MetadataStore + store.CursorStore）
// ───────────────────────────────────────────────────────────────────────────

// PgMetadataStore 真实 PG 适配器：Phase 2 / ADR-009 起元数据按 tenant_id 分片（Citus），
// 跨租户查询必须带 WHERE tenant_id = ? 才能命中 colocation shard；单账户查询同样需要 tenant_id 前置过滤。
type PgMetadataStore struct {
	pool *pgxpool.Pool
}

// NewPgMetadataStore 连接 PG（建议前置 PgBouncer 连接池）。
func NewPgMetadataStore(ctx context.Context, dsn string) (*PgMetadataStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &PgMetadataStore{pool: pool}, nil
}

func cursorJSON(c model.SyncCursor) string {
	b, _ := json.Marshal(c)
	return string(b)
}

// pgObserve 记录 PG 查询指标（Phase 2 / ADR-009：Citus 分布键 = tenant_id，
// shard 路由延迟观测）。op 标签区分 upsert_mail/get_mail/list_mails/count/
// list_accounts/set_read/delete_mail/unread_count/get_cursor/put_cursor。
// status 标签：ok / error，便于在 Grafana 计算成功率与错误率。
func pgObserve(tenantID, op string, start time.Time, err error) {
	status := "ok"
	if err != nil {
		status = "error"
	}
	observ.PgQueryTotal.Inc(map[string]string{"tenant": tenantID, "op": op, "status": status})
	observ.PgQueryDurationMs.Observe(
		map[string]string{"tenant": tenantID, "op": op},
		float64(time.Since(start).Milliseconds()),
	)
}

// UpsertMail 幂等写入（按主键 id 去重；Phase 2 / ADR-009 起 tenant_id 为 Citus 分布键，
// 跨租户同 id 极罕见，若需严格隔离可将唯一约束改为 (tenant_id, account_id, id)）。
// 注意：Citus 模式下 INSERT 自动按 tenant_id 路由到对应 shard，无需 app 层显式选路。
func (s *PgMetadataStore) UpsertMail(tenantID string, m model.CanonicalMail) error {
	tenantID = tenant.Resolve(tenantID)
	start := time.Now()
	_, err := s.pool.Exec(context.Background(), `
		INSERT INTO mail_metadata
		  (id, tenant_id, account_id, provider, folder, subject, from_addr, body_text, internal_date, size_bytes, raw_object_key, cursor_json, read)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (id) DO UPDATE SET subject = EXCLUDED.subject, from_addr = EXCLUDED.from_addr, body_text = EXCLUDED.body_text, internal_date = EXCLUDED.internal_date, size_bytes = EXCLUDED.size_bytes, provider = EXCLUDED.provider, folder = EXCLUDED.folder, raw_object_key = EXCLUDED.raw_object_key, read = EXCLUDED.read`,
		m.ID, tenantID, m.AccountID, string(m.Provider), m.Folder, m.Subject,
		m.From.Email, m.BodyText, m.InternalDate, m.SizeBytes, m.RawObjectKey, cursorJSON(m.Cursor), m.Read)
	pgObserve(tenantID, "upsert_mail", start, err)
	return err
}

// GetMail 按租户+主键取邮件（校验 tenant+account 归属）。
func (s *PgMetadataStore) GetMail(tenantID, accountID, id string) (*model.CanonicalMail, error) {
	tenantID = tenant.Resolve(tenantID)
	start := time.Now()
	row := s.pool.QueryRow(context.Background(),
		`SELECT id,tenant_id,account_id,provider,folder,subject,from_addr,body_text,internal_date,size_bytes,raw_object_key,cursor_json,read
		 FROM mail_metadata WHERE id=$1 AND account_id=$2 AND tenant_id=$3`, id, accountID, tenantID)
	m, err := scanMail(row)
	pgObserve(tenantID, "get_mail", start, err)
	return m, err
}

// ListMails 列出租户内账户某文件夹邮件（按时间倒序，截取 limit）。
func (s *PgMetadataStore) ListMails(tenantID, accountID, folder string, limit int) ([]model.CanonicalMail, error) {
	tenantID = tenant.Resolve(tenantID)
	start := time.Now()
	rows, err := s.pool.Query(context.Background(),
		`SELECT id,tenant_id,account_id,provider,folder,subject,from_addr,body_text,internal_date,size_bytes,raw_object_key,cursor_json,read
		 FROM mail_metadata WHERE account_id=$1 AND folder=$2 AND tenant_id=$3
		 ORDER BY internal_date DESC LIMIT $4`, accountID, folder, tenantID, limit)
	if err != nil {
		pgObserve(tenantID, "list_mails", start, err)
		return nil, err
	}
	defer rows.Close()
	var out []model.CanonicalMail
	for rows.Next() {
		m, err := scanMail(rows)
		if err != nil {
			pgObserve(tenantID, "list_mails", start, err)
			return nil, err
		}
		out = append(out, *m)
	}
	err = rows.Err()
	pgObserve(tenantID, "list_mails", start, err)
	return out, err
}

// Count 租户内账户邮件数。
func (s *PgMetadataStore) Count(tenantID, accountID string) (int, error) {
	tenantID = tenant.Resolve(tenantID)
	start := time.Now()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM mail_metadata WHERE account_id=$1 AND tenant_id=$2`, accountID, tenantID).Scan(&n); err != nil {
		pgObserve(tenantID, "count", start, err)
		return 0, err
	}
	pgObserve(tenantID, "count", start, nil)
	return n, nil
}

// ListAccounts 列出租户内所有出现过的 account_id（演示用；生产可加 WHERE active 过滤）。
func (s *PgMetadataStore) ListAccounts(tenantID string) ([]string, error) {
	tenantID = tenant.Resolve(tenantID)
	start := time.Now()
	rows, err := s.pool.Query(context.Background(),
		`SELECT DISTINCT account_id FROM mail_metadata WHERE tenant_id=$1 ORDER BY account_id`, tenantID)
	if err != nil {
		pgObserve(tenantID, "list_accounts", start, err)
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			pgObserve(tenantID, "list_accounts", start, err)
			return nil, err
		}
		out = append(out, a)
	}
	err = rows.Err()
	pgObserve(tenantID, "list_accounts", start, err)
	return out, err
}

// SetRead 设置邮件已读/未读（依赖 mail_metadata.read 列）。
func (s *PgMetadataStore) SetRead(tenantID, accountID, id string, read bool) error {
	tenantID = tenant.Resolve(tenantID)
	start := time.Now()
	_, err := s.pool.Exec(context.Background(),
		`UPDATE mail_metadata SET read=$4 WHERE id=$1 AND account_id=$2 AND tenant_id=$3`, id, accountID, tenantID, read)
	pgObserve(tenantID, "set_read", start, err)
	return err
}

// DeleteMail 物理删除邮件（演示用；生产建议软删除 + 审计）。
func (s *PgMetadataStore) DeleteMail(tenantID, accountID, id string) error {
	tenantID = tenant.Resolve(tenantID)
	start := time.Now()
	_, err := s.pool.Exec(context.Background(),
		`DELETE FROM mail_metadata WHERE id=$1 AND account_id=$2 AND tenant_id=$3`, id, accountID, tenantID)
	pgObserve(tenantID, "delete_mail", start, err)
	return err
}

// UnreadCount 租户内账户未读邮件数（read 为 NULL 视为未读）。
func (s *PgMetadataStore) UnreadCount(tenantID, accountID string) (int, error) {
	tenantID = tenant.Resolve(tenantID)
	start := time.Now()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM mail_metadata WHERE account_id=$1 AND tenant_id=$2 AND (read IS NULL OR read=false)`,
		accountID, tenantID).Scan(&n); err != nil {
		pgObserve(tenantID, "unread_count", start, err)
		return 0, err
	}
	pgObserve(tenantID, "unread_count", start, nil)
	return n, nil
}

// GetCursor 取同步游标。
func (s *PgMetadataStore) GetCursor(tenantID, accountID, folder string) (model.SyncCursor, error) {
	tenantID = tenant.Resolve(tenantID)
	start := time.Now()
	var raw string
	err := s.pool.QueryRow(context.Background(),
		`SELECT cursor_json FROM account_sync_cursor WHERE account_id=$1 AND folder=$2 AND tenant_id=$3`,
		accountID, folder, tenantID).Scan(&raw)
	if err != nil {
		// 无游标视为首次同步，记 ok（业务正常路径，非 PG 错误）
		pgObserve(tenantID, "get_cursor", start, nil)
		return model.SyncCursor{}, nil
	}
	var c model.SyncCursor
	if uerr := json.Unmarshal([]byte(raw), &c); uerr != nil {
		pgObserve(tenantID, "get_cursor", start, uerr)
		return model.SyncCursor{}, uerr
	}
	pgObserve(tenantID, "get_cursor", start, nil)
	return c, nil
}

// PutCursor 存同步游标（按 tenant+account+folder upsert）。
func (s *PgMetadataStore) PutCursor(tenantID, accountID, folder string, c model.SyncCursor) error {
	tenantID = tenant.Resolve(tenantID)
	start := time.Now()
	_, err := s.pool.Exec(context.Background(), `
		INSERT INTO account_sync_cursor (tenant_id, account_id, folder, cursor_json)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (tenant_id, account_id, folder) DO UPDATE SET cursor_json = EXCLUDED.cursor_json`,
		tenantID, accountID, folder, cursorJSON(c))
	pgObserve(tenantID, "put_cursor", start, err)
	return err
}

// scanMail 从行/行集读取一封邮件（适配 pgx 的 Row 与 Rows 都实现了 Scan）。
func scanMail(row interface {
	Scan(...interface{}) error
}) (*model.CanonicalMail, error) {
	var m model.CanonicalMail
	var provider, tenantID, fromAddr, rawCursor string
	if err := row.Scan(&m.ID, &tenantID, &m.AccountID, &provider, &m.Folder, &m.Subject,
		&fromAddr, &m.BodyText, &m.InternalDate, &m.SizeBytes, &m.RawObjectKey, &rawCursor, &m.Read); err != nil {
		return nil, err
	}
	m.TenantID = tenantID
	m.Provider = model.Provider(provider)
	m.From = model.Address{Email: fromAddr}
	_ = json.Unmarshal([]byte(rawCursor), &m.Cursor)
	return &m, nil
}

// ───────────────────────────────────────────────────────────────────────────
// 对象存储内容仓储（store.ContentStore，MinIO/S3 兼容，内容寻址去重）
// ───────────────────────────────────────────────────────────────────────────

// ObjectContentStore 真实对象存储适配器：key = mail/<sha256>，相同内容只存一份。
type ObjectContentStore struct {
	client *minio.Client
	bucket string
}

// NewObjectContentStore 连接 MinIO/S3。
func NewObjectContentStore(_ context.Context, endpoint, bucket, accessKey, secretKey string, secure bool) (*ObjectContentStore, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
	})
	if err != nil {
		return nil, err
	}
	return &ObjectContentStore{client: client, bucket: bucket}, nil
}

// Put 内容寻址写入（已存在则跳过）。
func (s *ObjectContentStore) Put(content []byte) (store.StoredContent, error) {
	sum := sha256.Sum256(content)
	hash := fmt.Sprintf("%x", sum)
	key := "mail/" + hash
	if _, err := s.client.StatObject(context.Background(), s.bucket, key, minio.StatObjectOptions{}); err == nil {
		return store.StoredContent{ObjectKey: key, ContentHash: hash, Size: len(content)}, nil
	}
	_, err := s.client.PutObject(context.Background(), s.bucket, key,
		bytes.NewReader(content), int64(len(content)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return store.StoredContent{}, err
	}
	return store.StoredContent{ObjectKey: key, ContentHash: hash, Size: len(content)}, nil
}

// Get 按对象键取回。
func (s *ObjectContentStore) Get(objectKey string) ([]byte, error) {
	obj, err := s.client.GetObject(context.Background(), s.bucket, objectKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(obj)
}

// ───────────────────────────────────────────────────────────────────────────
// OpenSearch 检索索引（search.SearchIndex，Phase 2 / ADR-009：按 tenantId 路由索引）
// ───────────────────────────────────────────────────────────────────────────

// OpenSearchIndex 真实索引适配器：index = mail-<tenantId>，多账户共用同一租户索引，
// accountId 作为字段过滤项（避免索引爆炸）；首次写入会自动应用 mail-* index template。
type OpenSearchIndex struct {
	client *opensearch.Client
}

// NewOpenSearchIndex 连接 OpenSearch。
// 开发/PoC：OpenSearch 镜像默认在 REST 端口启用自签 TLS，故 https 地址下跳过证书校验；
// 同时镜像启用了 security 插件，写入（索引/检索）需基础鉴权（admin 凭据），否则返回 401。
// 生产应使用受信 CA（opensearch.Config.Transport 配置正确根证书）并配合最小权限账号。
func NewOpenSearchIndex(address, user, pass string) (*OpenSearchIndex, error) {
	cfg := opensearch.Config{
		Addresses: []string{address},
		Username:  user,
		Password:  pass,
	}
	if strings.HasPrefix(address, "https") {
		cfg.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // dev/PoC only
		}
	}
	client, err := opensearch.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &OpenSearchIndex{client: client}, nil
}

func sanitizeIndex(accountID string) string {
	return strings.ToLower(strings.ReplaceAll(accountID, "@", "_"))
}

// Index 写入文档（Phase 2 / ADR-009：索引按租户物理隔离，index = mail-<tenantId>，
// accountId 作为字段保留以供查询时按账户过滤；index template 见 deploy/migrations/003_opensearch_templates.json）。
func (s *OpenSearchIndex) Index(tenantID string, m model.CanonicalMail) error {
	tenantID = tenant.Resolve(tenantID)
	doc := map[string]any{
		"tenantId":     tenantID,
		"accountId":    m.AccountID,
		"subject":      m.Subject,
		"from":         m.From.Email,
		"bodyText":     m.BodyText,
		"internalDate": m.InternalDate,
	}
	body, _ := json.Marshal(doc)
	index := "mail-" + sanitizeIndex(tenantID)
	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("/%s/_doc/%s", index, m.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Perform(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("opensearch index status=%d body=%s", resp.StatusCode, b)
	}
	return nil
}

// Search 关键词检索（Phase 2 / ADR-009：路由到 mail-<tenantId> 单租户索引，
// multi_match 外层加 bool.filter.term.accountId 限定账户，物理隔离 + 字段过滤双保险）。
func (s *OpenSearchIndex) Search(tenantID, accountID, query string, limit int) ([]search.SearchHit, error) {
	tenantID = tenant.Resolve(tenantID)
	index := "mail-" + sanitizeIndex(tenantID)
	body, _ := json.Marshal(map[string]any{
		"size": limit,
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []map[string]any{
					{"term": map[string]any{"accountId": accountID}},
				},
				"must": []map[string]any{
					{"multi_match": map[string]any{
						"query":  query,
						"fields": []string{"subject", "from", "bodyText"},
					}},
				},
			},
		},
		"sort": []any{map[string]any{"internalDate": "desc"}},
	})
	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("/%s/_search", index), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Perform(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("opensearch search status=%d body=%s", resp.StatusCode, b)
	}
	var parsed struct {
		Hits struct {
			Hits []struct {
				ID  string         `json:"_id"`
				Src map[string]any `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, err
	}
	hits := make([]search.SearchHit, 0, len(parsed.Hits.Hits))
	for _, h := range parsed.Hits.Hits {
		hits = append(hits, search.SearchHit{
			ID:           h.ID,
			TenantID:     tenantID,
			AccountID:    str(h.Src["accountId"]),
			Subject:      str(h.Src["subject"]),
			From:         str(h.Src["from"]),
			Preview:      truncateStr(str(h.Src["bodyText"]), 80),
			InternalDate: toInt64(h.Src["internalDate"]),
		})
	}
	return hits, nil
}

// Remove 按租户+邮件 ID 删除索引文档（Phase 2 / ADR-009：mail-<tenantId>/_doc/<id>）。
// 404（文档不存在）视为成功，保证删除幂等；accountId 参数保留以兼容 SearchIndex 接口契约。
func (s *OpenSearchIndex) Remove(tenantID, accountID, id string) error {
	tenantID = tenant.Resolve(tenantID)
	_ = accountID // Phase 2 起 index 已不带 accountId，但仍校验 tenant 归属
	index := "mail-" + sanitizeIndex(tenantID)
	req, _ := http.NewRequest(http.MethodDelete,
		fmt.Sprintf("/%s/_doc/%s", index, id), nil)
	resp, err := s.client.Perform(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("opensearch delete status=%d body=%s", resp.StatusCode, b)
	}
	return nil
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ───────────────────────────────────────────────────────────────────────────
// Kafka 事件总线（events.EventBus，segmentio/kafka-go）
// ───────────────────────────────────────────────────────────────────────────

// KafkaAdapter 真实 Kafka 适配器：生产者用 Writer，消费者按 group 起 Reader。
type KafkaAdapter struct {
	writer     *kafkago.Writer
	brokers    []string
	mu         sync.Mutex
	readers    map[string]*kafkago.Reader
	retryCfg   events.RetryConfig                                                   // 演进：有界重试（默认 3 次/200ms 起/封顶 2s）
	dlqEnabled bool                                                                 // 演进：最终失败转 <topic>-dlq
	dialFunc   func(ctx context.Context, network, address string) (net.Conn, error) // 可选拨号重写（advertised 与客户端网络不一致时）
}

// NewKafkaAdapter 构造（brokers 形如 []string{"localhost:9092"}）。
func NewKafkaAdapter(brokers []string) *KafkaAdapter {
	k := &KafkaAdapter{
		brokers:    brokers,
		readers:    map[string]*kafkago.Reader{},
		retryCfg:   events.DefaultRetryConfig,
		dlqEnabled: true,
	}
	// 统一拨号通路：dialFunc 非空时重写目标地址；否则回退系统默认 net.Dialer。
	// 覆盖场景：broker advertised 通告容器名/内网名，而客户端在宿主机/跨网络。
	k.writer = &kafkago.Writer{
		Addr: kafkago.TCP(brokers...),
		// 吞吐优先：Async 入队即返（fetch 不被逐封生产阻塞），writer 后台批量聚合发送；
		// RequiredAcks=RequireOne（leader 确认）+ kafka-go 内部重试保证 at-least-once，
		// 最终失败经 Errors() 由 drainErrors 排空打日志，不静默丢失。
		// 实测：单机 broker 同步生产 ~1s/封；Async 后 fetch 不阻塞。
		Async:        true,
		RequiredAcks: kafkago.RequireOne,
		BatchTimeout: 50 * time.Millisecond,
		// 该版本 kafka-go (v0.4.47) 无 Errors() 方法：Async 发送失败经 ErrorLogger 上报，避免静默丢失。
		ErrorLogger: log.New(os.Stderr, "[kafka] ", 0),
		Transport: &kafkago.Transport{Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if k.dialFunc != nil {
				return k.dialFunc(ctx, network, address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}},
	}
	return k
}

// WithRetry 覆盖重试配置（演进：调用方可调优退避/次数）。
func (k *KafkaAdapter) WithRetry(cfg events.RetryConfig) *KafkaAdapter {
	k.retryCfg = cfg
	return k
}

// WithDial 覆盖拨号函数（可选）：broker 通告地址与客户端可达网络不一致时
// （如 docker 容器名），可把任意目标地址重写为宿主机可达地址。nil 时用默认 net.Dialer。
func (k *KafkaAdapter) WithDial(fn func(ctx context.Context, network, address string) (net.Conn, error)) *KafkaAdapter {
	k.dialFunc = fn
	return k
}

// Publish 投递事件（topic 由参数指定；生产建议按 key 分区保证同账户有序）。
func (k *KafkaAdapter) Publish(ctx context.Context, topic, key string, payload []byte) error {
	return k.writer.WriteMessages(ctx, kafkago.Message{
		Topic: topic,
		Key:   []byte(key),
		Value: payload,
	})
}

// Subscribe 注册消费者组（后台 goroutine 持续拉取，经 handler 回调）。
func (k *KafkaAdapter) Subscribe(ctx context.Context, topic, group string, handler func(ctx context.Context, env events.EventEnvelope) error) error {
	r := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers: k.brokers,
		Topic:   topic,
		GroupID: group,
		Dialer: &kafkago.Dialer{DialFunc: func(ctx context.Context, network, address string) (net.Conn, error) {
			if k.dialFunc != nil {
				return k.dialFunc(ctx, network, address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}},
	})
	k.mu.Lock()
	k.readers[topic+":"+group] = r
	k.mu.Unlock()
	go func() {
		for {
			m, err := r.ReadMessage(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				continue // 生产环境应接 DLQ + 退避
			}
			env := events.EventEnvelope{
				Topic:   m.Topic,
				Key:     string(m.Key),
				TS:      m.Time.UnixMilli(),
				Type:    events.TypeOfTopic(m.Topic),
				Payload: m.Value,
			}
			// 演进：有界重试（指数退避）→ 最终失败转 DLQ，避免毒丸消息阻塞消费组；
			// 消费端自行以 env.Key 幂等（可包 events.WithIdempotency 包装器）。
			if err := events.Retry(ctx, k.retryCfg, func() error { return handler(ctx, env) }); err != nil {
				k.dlq(ctx, topic, env, err)
			}
		}
	}()
	return nil
}

// dlq 把处理失败的事件转投 <topic>-dlq（值为信封 JSON，携带原始 payload 供重放/审计）。
func (k *KafkaAdapter) dlq(ctx context.Context, topic string, env events.EventEnvelope, cause error) {
	if !k.dlqEnabled {
		fmt.Printf("[kafka] %s key=%s dropped after retries: %v\n", topic, env.Key, cause)
		return
	}
	raw, err := json.Marshal(env)
	if err != nil {
		fmt.Printf("[kafka] dlq marshal %s key=%s: %v\n", topic, env.Key, err)
		return
	}
	dlqTopic := events.TopicDLQ(topic)
	if err := k.writer.WriteMessages(ctx, kafkago.Message{Topic: dlqTopic, Key: []byte(env.Key), Value: raw}); err != nil {
		fmt.Printf("[kafka] dlq write %s key=%s: %v (orig: %v)\n", dlqTopic, env.Key, err, cause)
		return
	}
	fmt.Printf("[kafka] dlq %s -> %s key=%s cause=%v\n", topic, dlqTopic, env.Key, cause)
}

// Close 释放生产/消费者。
func (k *KafkaAdapter) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, r := range k.readers {
		_ = r.Close()
	}
	return k.writer.Close()
}

// ───────────────────────────────────────────────────────────────────────────
// WebSocket 实时通知（notify.Notifier，gorilla/websocket）
// ───────────────────────────────────────────────────────────────────────────

// WsHub 真实 WebSocket 通知中心：HTTP 握手升级后按 accountId 注册 sink，
// 连接关闭自动注销。挂载到已有 http.Server 使 REST 与 WS 同端口。
type WsHub struct {
	upgrader websocket.Upgrader
	mu       sync.RWMutex
	sinks    map[string]map[notify.PushSink]struct{}
}

// NewWsHub 构造。
func NewWsHub() *WsHub {
	return &WsHub{
		upgrader: websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }},
		sinks:    map[string]map[notify.PushSink]struct{}{},
	}
}

// Upgrade 处理 WS 握手，按 accountID 注册推送目标；连接关闭由 wsSink 自动注销。
func (h *WsHub) Upgrade(w http.ResponseWriter, r *http.Request, accountID string) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	sink := &wsSink{conn: conn, hub: h, accountID: accountID}
	h.AddSink(accountID, sink)
	go sink.readLoop()
}

// AddSink 注册（notify.Notifier）。
func (h *WsHub) AddSink(accountID string, sink notify.PushSink) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sinks[accountID] == nil {
		h.sinks[accountID] = map[notify.PushSink]struct{}{}
	}
	h.sinks[accountID][sink] = struct{}{}
}

// RemoveSink 注销（notify.Notifier）。
func (h *WsHub) RemoveSink(accountID string, sink notify.PushSink) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sinks[accountID], sink)
}

// Publish 广播给某账户的所有订阅者（notify.Notifier；tenantID 写入载荷供隔离/观测）。
func (h *WsHub) Publish(tenantID, accountID string, payload notify.NotificationPayload) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	payload.TenantID = tenant.Resolve(tenantID)
	for s := range h.sinks[accountID] {
		s.Send(payload)
	}
}

// wsSink 单连接推送目标。
type wsSink struct {
	conn      *websocket.Conn
	hub       *WsHub
	accountID string
	mu        sync.Mutex
}

func (s *wsSink) Send(p notify.NotificationPayload) {
	b, err := notify.MarshalPayload(p)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.conn.WriteMessage(websocket.TextMessage, b)
}

func (s *wsSink) Close() {
	s.hub.RemoveSink(s.accountID, s)
	_ = s.conn.Close()
}

// readLoop 读循环：仅用于侦测断连，收到任意消息即视为连接存活；出错则注销。
func (s *wsSink) readLoop() {
	for {
		if _, _, err := s.conn.ReadMessage(); err != nil {
			s.Close()
			return
		}
	}
}
