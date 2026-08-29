// Package api BFF / API 网关（PoC）：REST（健康/邮件列表/检索）+ WebSocket 实时推送同端口。
// 真实环境前置 L7 网关 + 鉴权 + 限流；本文件聚焦接口契约与事件打通。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"email-aggregator-go/src/aigateway"
	"email-aggregator-go/src/model"
	"email-aggregator-go/src/notify"
	"email-aggregator-go/src/search"
	"email-aggregator-go/src/store"
	"email-aggregator-go/src/tenant"
)

// ApiServer 依赖装配
type ApiServer struct {
	metadata store.MetadataStore
	search   search.SearchIndex
	notifier notify.Notifier
	gw       *aigateway.Router   // 可选：AI 能力路由网关（ADR-010）
	wsHub    *notify.Hub         // 可选：WebSocket 实时推送（零依赖 Hub）
	accounts store.AccountStore  // 可选：连接/账户服务（未挂载时 /api/accounts 回退元数据推导）
	port     int
}

// NewApiServer 构造（注入元数据仓储、检索索引、通知器）
func NewApiServer(metadata store.MetadataStore, idx search.SearchIndex, notifier notify.Notifier, port int) *ApiServer {
	return &ApiServer{metadata: metadata, search: idx, notifier: notifier, port: port}
}

// WithAccounts 可选挂载连接/账户服务（账户注册表 CRUD）。
func (s *ApiServer) WithAccounts(a store.AccountStore) *ApiServer {
	s.accounts = a
	return s
}

// WithAIGateway 可选挂载 AI 能力路由网关（ADR-010）。非破坏性：未调用则无 /api/ai/chat 路由。
func (s *ApiServer) WithAIGateway(r *aigateway.Router) *ApiServer {
	s.gw = r
	return s
}

// WithWSHub 可选挂载 WebSocket 实时推送 Hub。非破坏性：未调用则无 /ws 路由。
func (s *ApiServer) WithWSHub(h *notify.Hub) *ApiServer {
	s.wsHub = h
	return s
}

// Handler 返回 http.Handler（可挂载到任意 mux / 网关之后；WS 升级由 notify.Notifier 在真实版处理）
func (s *ApiServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/mails", s.handleMails)
	mux.HandleFunc("/api/mails/{id}", s.handleMailByID)
	mux.HandleFunc("/api/mails/{id}/read", s.handleSetRead)
	mux.HandleFunc("/api/accounts", s.handleAccounts)
	mux.HandleFunc("/api/accounts/{id}", s.handleAccountByID)
	mux.HandleFunc("/api/search", s.handleSearch)
	if s.gw != nil {
		mux.HandleFunc("/api/ai/chat", s.handleAIChat)
	}
	if s.wsHub != nil {
		mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
			accountID := r.URL.Query().Get("accountId")
			if accountID == "" {
				http.Error(w, "accountId required", http.StatusBadRequest)
				return
			}
			s.wsHub.Upgrade(w, r, accountID)
		})
	}
	// 演示用：触发一封新邮件并实时推送（生产由真实同步流水线驱动）
	mux.HandleFunc("/api/demo/push", s.handleDemoPush)
	// 自包含发布形态：在 webui 构建下托管内嵌 SPA；开发态为空操作。
	s.mountStatic(mux)
	return mux
}

// handleAIChat ADR-010 的 REST 入口：解析 ChatRequest → Router 路由+脱敏+审计 → 写响应/错误。
func (s *ApiServer) handleAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
		return
	}
	var req aigateway.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	// 兜底：未携带租户上下文时默认公共租户且已同意（demo/联调用，ADR-009 公开路径）。
	if req.TenantTier == "" {
		req.TenantTier = aigateway.TierPublic
		req.UserConsented = true
	}
	resp, err := s.gw.RouteAndChat(r.Context(), req)
	if err != nil {
		writeJSON(w, 422, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, resp)
}

func (s *ApiServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleMailByID 按 HTTP 方法分派：GET 取单封权威资源，DELETE 删除邮件并广播事件。
func (s *ApiServer) handleMailByID(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getMailByID(w, r)
	case http.MethodDelete:
		s.deleteMailByID(w, r)
	default:
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
	}
}

// getMailByID 返回单封邮件的权威资源（按 租户+账户+ID 取），供前端详情走服务端真实数据
// 而非列表副本。缺失 accountId 返回 400；邮件不存在返回 404。
func (s *ApiServer) getMailByID(w http.ResponseWriter, r *http.Request) {
	tid := tenant.FromRequest(r).TenantID
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]any{"error": "missing mail id"})
		return
	}
	accountID := r.URL.Query().Get("accountId")
	if accountID == "" {
		writeJSON(w, 400, map[string]any{"error": "accountId required"})
		return
	}
	mail, err := s.metadata.GetMail(tid, accountID, id)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	if mail == nil {
		writeJSON(w, 404, map[string]any{"error": "mail not found", "tenantId": tid, "accountId": accountID, "id": id})
		return
	}
	writeJSON(w, 200, mail)
}

// deleteMailByID 删除邮件（CRUD 生命周期），成功后广播 mail-deleted 事件驱动 WS 实时刷新。
func (s *ApiServer) deleteMailByID(w http.ResponseWriter, r *http.Request) {
	tid := tenant.FromRequest(r).TenantID
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]any{"error": "missing mail id"})
		return
	}
	accountID := r.URL.Query().Get("accountId")
	if accountID == "" {
		writeJSON(w, 400, map[string]any{"error": "accountId required"})
		return
	}
	if err := s.metadata.DeleteMail(tid, accountID, id); err != nil {
		writeJSON(w, 404, map[string]any{"error": err.Error()})
		return
	}
	// 同步清理检索索引，避免删除后仍能搜到 stale 命中（存储/检索边界分离，服务层编排）。
	_ = s.search.Remove(tid, accountID, id)
	s.notifier.Publish(tid, accountID, notify.NotificationPayload{
		Kind:      notify.KindMailDeleted,
		AccountID: accountID,
		MailID:    id,
		TS:        time.Now().Unix(),
	})
	writeJSON(w, 200, map[string]any{"ok": true, "id": id})
}

// handleSetRead 设置邮件已读/未读（POST/PUT，body: {read: bool}），广播 mail-updated 事件。
func (s *ApiServer) handleSetRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
		return
	}
	tid := tenant.FromRequest(r).TenantID
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]any{"error": "missing mail id"})
		return
	}
	accountID := r.URL.Query().Get("accountId")
	if accountID == "" {
		writeJSON(w, 400, map[string]any{"error": "accountId required"})
		return
	}
	var body struct {
		Read bool `json:"read"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	if err := s.metadata.SetRead(tid, accountID, id, body.Read); err != nil {
		writeJSON(w, 404, map[string]any{"error": err.Error()})
		return
	}
	s.notifier.Publish(tid, accountID, notify.NotificationPayload{
		Kind:      notify.KindMailUpdated,
		AccountID: accountID,
		MailID:    id,
		Read:      body.Read,
		TS:        time.Now().Unix(),
	})
	writeJSON(w, 200, map[string]any{"ok": true, "id": id, "read": body.Read})
}

// handleAccounts 返回当前租户内已种子/已写入邮件的账户列表（含未读数），
// 供前端渲染账户切换 chips 与未读徽标，避免前后端账户集合漂移。
// handleAccounts 账户列表：
//   GET /api/accounts —— 挂载账户服务时返回注册表（含 provider/email/status + 未读数）；
//   否则回退「按 mail_metadata 去重推导」的兼容路径。
//   POST /api/accounts —— 新建账户（连接/账户服务），body: {id, provider, email, displayName, status, syncFolder, credentialsRef}
func (s *ApiServer) handleAccounts(w http.ResponseWriter, r *http.Request) {
	tid := tenant.FromRequest(r).TenantID
	if r.Method == http.MethodPost {
		if s.accounts == nil {
			writeJSON(w, 501, map[string]any{"error": "account service not mounted"})
			return
		}
		var a model.Account
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			writeJSON(w, 400, map[string]any{"error": "bad request: " + err.Error()})
			return
		}
		if msg := a.Valid(); msg != "" {
			writeJSON(w, 400, map[string]any{"error": msg})
			return
		}
		if err := s.accounts.CreateAccount(tid, a); err != nil {
			writeJSON(w, 409, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 201, a)
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
		return
	}

	if s.accounts != nil {
		list, err := s.accounts.ListAccounts(tid)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		accounts := make([]map[string]any, 0, len(list))
		for _, a := range list {
			c, _ := s.metadata.UnreadCount(tid, a.ID)
			accounts = append(accounts, map[string]any{
				"id": a.ID, "unread": c, "provider": a.Provider, "email": a.Email,
				"displayName": a.DisplayName, "status": a.Status, "syncFolder": a.SyncFolder,
				"lastSyncAt": a.LastSyncAt,
			})
		}
		writeJSON(w, 200, map[string]any{"tenantId": tid, "accounts": accounts})
		return
	}

	// 兼容回退：元数据去重推导账户列表
	ids, err := s.metadata.ListAccounts(tid)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	if ids == nil {
		ids = []string{}
	}
	accounts := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		c, _ := s.metadata.UnreadCount(tid, id)
		accounts = append(accounts, map[string]any{"id": id, "unread": c})
	}
	writeJSON(w, 200, map[string]any{"tenantId": tid, "accounts": accounts})
}

// handleAccountByID 单账户 CRUD（连接/账户服务）：
//   GET    /api/accounts/{id} —— 详情
//   PUT    /api/accounts/{id} —— 更新（status/displayName/syncFolder 等）
//   DELETE /api/accounts/{id} —— 删除
func (s *ApiServer) handleAccountByID(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil {
		writeJSON(w, 501, map[string]any{"error": "account service not mounted"})
		return
	}
	tid := tenant.FromRequest(r).TenantID
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]any{"error": "account id required"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		a, err := s.accounts.GetAccount(tid, id)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		if a == nil {
			writeJSON(w, 404, map[string]any{"error": "account not found"})
			return
		}
		writeJSON(w, 200, a)
	case http.MethodPut:
		var a model.Account
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			writeJSON(w, 400, map[string]any{"error": "bad request: " + err.Error()})
			return
		}
		a.ID = id
		existing, err := s.accounts.GetAccount(tid, id)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		if existing == nil {
			writeJSON(w, 404, map[string]any{"error": "account not found"})
			return
		}
		// 部分更新语义：未显式提供的字段保留原值（先合并再校验，避免只改 status 时被 provider 必填拦截）
		if a.Provider == "" {
			a.Provider = existing.Provider
		}
		if a.Email == "" {
			a.Email = existing.Email
		}
		if a.DisplayName == "" {
			a.DisplayName = existing.DisplayName
		}
		if a.SyncFolder == "" {
			a.SyncFolder = existing.SyncFolder
		}
		if a.CredentialsRef == "" {
			a.CredentialsRef = existing.CredentialsRef
		}
		if a.Status == "" {
			a.Status = existing.Status
		}
		if msg := a.Valid(); msg != "" {
			writeJSON(w, 400, map[string]any{"error": msg})
			return
		}
		if err := s.accounts.UpdateAccount(tid, a); err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, a)
	case http.MethodDelete:
		if err := s.accounts.DeleteAccount(tid, id); err != nil {
			writeJSON(w, 404, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "id": id})
	default:
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
	}
}

func (s *ApiServer) handleMails(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
		return
	}
	tid := tenant.FromRequest(r).TenantID
	q := r.URL.Query()
	accountID := q.Get("accountId")
	folder := q.Get("folder")
	if folder == "" {
		folder = "INBOX"
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	mails, err := s.metadata.ListMails(tid, accountID, folder, limit)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"tenantId": tid, "accountId": accountID, "count": len(mails), "mails": mails})
}

func (s *ApiServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
		return
	}
	tid := tenant.FromRequest(r).TenantID
	q := r.URL.Query()
	accountID := q.Get("accountId")
	query := q.Get("q")
	if s.search == nil {
		writeJSON(w, 200, map[string]any{"tenantId": tid, "accountId": accountID, "query": query, "hits": []any{}})
		return
	}
	hits, err := s.search.Search(tid, accountID, query, 50)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"tenantId": tid, "accountId": accountID, "query": query, "hits": hits})
}

// handleDemoPush 演示用：生成一封新邮件、写入存储与索引、并发布 new-mail 通知驱动 WS 实时推送。
// 生产环境由真实同步流水线（connector → orchestrator → ingest → notifier）触发，此处仅用于前端联调。
func (s *ApiServer) handleDemoPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
		return
	}
	accountID := r.URL.Query().Get("accountId")
	if accountID == "" {
		accountID = "acc_demo"
	}
	tid := tenant.FromRequest(r).TenantID
	id := fmt.Sprintf("m%d", time.Now().UnixNano())
	mail := model.CanonicalMail{
		TenantID:     tid,
		ID:           id,
		AccountID:    accountID,
		Provider:     model.ProviderIMAP,
		Folder:       "INBOX",
		From:         model.Address{Email: "noreply@system.com", Name: "系统通知"},
		Subject:      "实时推送演示邮件",
		BodyText:     "这是一封由 /api/demo/push 触发的演示邮件，用于验证 WebSocket 实时推送。",
		InternalDate: time.Now().Unix(),
		SizeBytes:    128,
		Cursor:       model.SyncCursor{LastUID: uint32(time.Now().Unix())},
	}
	if err := s.metadata.UpsertMail(tid, mail); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	if s.search != nil {
		_ = s.search.Index(tid, mail)
	}
	s.notifier.Publish(tid, accountID, notify.NotificationPayload{
		Kind:      notify.KindNewMail,
		AccountID: accountID,
		Preview:   mail.Subject,
		TS:        time.Now().Unix(),
	})
	writeJSON(w, 200, map[string]any{"tenantId": tid, "ok": true, "id": id, "subject": mail.Subject})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		w.WriteHeader(500)
		return
	}
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// Addr 返回监听地址（便于测试）
func (s *ApiServer) Addr() string {
	return fmt.Sprintf(":%d", s.port)
}
