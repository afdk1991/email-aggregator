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
	gw       *aigateway.Router // 可选：AI 能力路由网关（ADR-010）
	wsHub    *notify.Hub        // 可选：WebSocket 实时推送（零依赖 Hub）
	port     int
}

// NewApiServer 构造（注入元数据仓储、检索索引、通知器）
func NewApiServer(metadata store.MetadataStore, idx search.SearchIndex, notifier notify.Notifier, port int) *ApiServer {
	return &ApiServer{metadata: metadata, search: idx, notifier: notifier, port: port}
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
func (s *ApiServer) handleAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
		return
	}
	tid := tenant.FromRequest(r).TenantID
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
