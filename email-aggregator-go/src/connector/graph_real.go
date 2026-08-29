package connector

// RealGraphConnector 真实 Microsoft Graph 适配器（Graph API REST + OAuth2 Bearer，纯标准库实现，零第三方依赖）。
//
// 与架构文档 §10/§12 对齐：Exchange Online / Microsoft 365 走官方 Graph API（非 EWS SOAP 复用），
// 是本项目「Exchange/Graph 增强」的落地实现（Exchange 增强 = 云上走 Graph、本地走 EWS，双模并存）：
//   - 鉴权：OAuth2 Bearer（Microsoft 身份平台 v2 产物即 access token）
//   - 全量：GET /me/messages（@odata.nextLink 分页），游标基线 = delta 端点首次调用返回的 deltaLink
//   - 增量：GET /me/messages/delta（@odata.nextLink 翻页 / @odata.deltaLink 断点续传），
//           游标 = SyncCursor.ProviderSpecific["deltaLink"]（deltaToken 蕴含于链接内）
//   - 变更：delta 响应含 @removed 条目 → sink.OnDelete（对齐 Gmail history messagesDeleted 语义）
//   - 推送：Graph 官方用 subscription + change notifications；本适配器 SupportsIdle=false，
//           StreamChanges 采用轮询（对齐 Gmail/POP3/EWS），默认间隔可注入。
//
// 通过 model.Connector 接口与 sync 编排层（syncsvc.Orchestrator）对接；
// 测试用内嵌 mock Graph API（StartMockGraphAPI）端到端验证（对齐 MockGmailServer 模式）。
//
// 游标说明：deltaLink 单调推进，每次增量响应都会返回最新 deltaLink。
// PoC 中游标通过「每封新增邮件的 Cursor.ProviderSpecific["deltaLink"]」随 sink 回传持久化；
// 空批次/纯删除批次无新增邮件时，由调用方取 LastDeltaLink() 持久化（生产由连接服务统一落盘）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"email-aggregator-go/src/model"
)

// RealGraphConnector 真实 Microsoft Graph 连接器。
type RealGraphConnector struct {
	baseURL      string
	httpClient   *http.Client
	user         string
	token        string
	pollInterval time.Duration
	cap          model.ConnectorCapabilities
	lastDelta    string // 最近一次同步感知到的最新 deltaLink（供调用方持久化空批次游标）
}

// NewRealGraphConnector 构造 Graph 连接器。
// baseURL 缺省使用生产 Graph API（v1.0）；测试注入 mock 地址。
func NewRealGraphConnector(baseURL string) *RealGraphConnector {
	if baseURL == "" {
		baseURL = "https://graph.microsoft.com/v1.0"
	}
	return &RealGraphConnector{
		baseURL:      strings.TrimRight(baseURL, "/"),
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		user:         "me",
		pollInterval: 30 * time.Second,
		cap: model.ConnectorCapabilities{
			Provider:            model.ProviderGraph,
			SupportsIncremental: true, // delta 增量
			SupportsOAuth:       true, // Bearer token
			SupportsIdle:        false,
			SupportsQresync:     false,
			SupportsCondstore:   false,
			SupportsUIDL:        false,
		},
	}
}

// SetPollInterval 设置 StreamChanges 轮询间隔（测试可注入短值）。
func (c *RealGraphConnector) SetPollInterval(d time.Duration) { c.pollInterval = d }

// SetHTTPClient 注入 HTTP 客户端（测试/代理场景）。
func (c *RealGraphConnector) SetHTTPClient(hc *http.Client) {
	if hc != nil {
		c.httpClient = hc
	}
}

// LastDeltaLink 返回最近一次同步感知到的 deltaLink（空批次游标持久化用）。
func (c *RealGraphConnector) LastDeltaLink() string { return c.lastDelta }

// ── model.Connector 接口实现 ───────────────────────────────────────────────

// Connect 校验 OAuth2 凭据并做一次 /me 探测（401 → 鉴权失败，快速失败）。
func (c *RealGraphConnector) Connect(ctx context.Context, cred model.Credential) error {
	tok := ""
	if cred.OAuth != nil && cred.OAuth.AccessToken != "" {
		tok = cred.OAuth.AccessToken
	} else if cred.Extra != nil {
		tok = cred.Extra["accessToken"]
	}
	if tok == "" {
		return fmt.Errorf("graph: oauth2 access token required")
	}
	if cred.Username != "" {
		c.user = cred.Username
	}
	c.token = tok

	if _, _, err := c.get(ctx, "/me"); err != nil {
		return fmt.Errorf("graph connect (/me probe): %w", err)
	}
	return nil
}

// Capabilities 返回能力声明。
func (c *RealGraphConnector) Capabilities() model.ConnectorCapabilities { return c.cap }

// InitialFullSync 初始全量：分页列出 /me/messages → 归一化 → 按 since 过滤经 sink 回传。
// 游标基线 = delta 首次调用返回的 deltaLink（此后以此为断点进入增量）。
func (c *RealGraphConnector) InitialFullSync(ctx context.Context, since int64, sink model.SyncSink) error {
	baseline, err := c.initialDelta(ctx)
	if err != nil {
		return err
	}
	c.lastDelta = baseline

	msgs, err := c.listAll(ctx)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if m.ReceivedAt < since {
			continue
		}
		cm, err := m.toMail(c.user, baseline)
		if err != nil {
			return fmt.Errorf("graph parse %s: %w", m.ID, err)
		}
		if err := sink.OnMessage(ctx, cm); err != nil {
			return err
		}
	}
	return nil
}

// IncrementalSync 增量：delta 端点 → added（含更新）与 @removed（删除），游标 = 响应 deltaLink。
func (c *RealGraphConnector) IncrementalSync(ctx context.Context, cursor model.SyncCursor, sink model.SyncSink) error {
	start := cursor.ProviderSpecific["deltaLink"]
	recs, latest, err := c.delta(ctx, start)
	if err != nil {
		return err
	}
	c.lastDelta = latest

	for _, rec := range recs {
		for _, m := range rec.Added {
			cm, err := m.toMail(c.user, latest)
			if err != nil {
				return fmt.Errorf("graph parse %s: %w", m.ID, err)
			}
			if err := sink.OnMessage(ctx, cm); err != nil {
				return err
			}
		}
		for _, id := range rec.Deleted {
			if err := sink.OnDelete(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// StreamChanges 推送（轮询实现，对齐 Gmail/POP3/EWS）：周期 delta 增量，新增邮件回调，ctx 取消退出。
func (c *RealGraphConnector) StreamChanges(ctx context.Context, cursor model.SyncCursor, onEvent func(ctx context.Context, m model.CanonicalMail) error) error {
	start := cursor.ProviderSpecific["deltaLink"]
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil // 已取消，正常退出
		}
		recs, latest, err := c.delta(ctx, start)
		if err != nil {
			if ctx.Err() != nil {
				return nil // 取消竞态：ctx 恰好在请求期间过期 → 优雅退出而非报错
			}
			return err
		}
		c.lastDelta = latest
		for _, rec := range recs {
			for _, m := range rec.Added {
				cm, err := m.toMail(c.user, latest)
				if err != nil {
					return err
				}
				if err := onEvent(ctx, cm); err != nil {
					return err
				}
			}
			// 删除在流模式仅忽略（onEvent 无删除通道；删除由增量模式 OnDelete 处理）
		}
		if latest != "" {
			start = latest
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Close 无状态 HTTP 适配器，无连接资源需释放。
func (c *RealGraphConnector) Close() error { return nil }

// ── Graph API 原语 ───────────────────────────────────────────────────────────

// graphMessage 归一化后的消息（wire 层 JSON 解析产物）。
type graphMessage struct {
	ID             string
	Subject        string
	From           model.Address
	To             []model.Address
	Cc             []model.Address
	Bcc            []model.Address
	BodyText       string
	BodyHTML       string
	BodyPreview    string
	ReceivedAt     int64
	SizeBytes      int64
	HasAttachments bool
	IsRead         bool
}

// graphDeltaRecord 归一化后的增量记录（added 含更新 / deleted 为消息 ID 列表）。
type graphDeltaRecord struct {
	Added   []graphMessage
	Deleted []string
}

// ── wire 层 JSON 结构 ──────────────────────────────────────────────────────

type graphWireMessage struct {
	ID              string `json:"id"`
	Subject         string `json:"subject"`
	IsRead          bool   `json:"isRead"`
	HasAttachments  bool   `json:"hasAttachments"`
	Size            int64  `json:"size"`
	BodyPreview     string `json:"bodyPreview"`
	Received        string `json:"receivedDateTime"`
	Body            struct {
		ContentType string `json:"contentType"` // text | html
		Content     string `json:"content"`
	} `json:"body"`
	From struct {
		EmailAddress struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"from"`
	ToRecipients  []graphRecipient `json:"toRecipients"`
	CcRecipients  []graphRecipient `json:"ccRecipients"`
	BccRecipients []graphRecipient `json:"bccRecipients"`
	Removed       *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`
}

type graphRecipient struct {
	EmailAddress struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

type graphMessagePage struct {
	Value      []graphWireMessage `json:"value"`
	NextLink   string             `json:"@odata.nextLink"`
	DeltaLink  string             `json:"@odata.deltaLink"`
	DeltaToken string             `json:"@odata.deltaToken"` // 部分端点直接给 token
}

// get 执行带 Bearer 的 GET，返回响应体与状态码（非 2xx 报错）。
func (c *RealGraphConnector) get(ctx context.Context, path string) ([]byte, int, error) {
	u := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, resp.StatusCode, fmt.Errorf("graph GET %s: status=%d body=%s", path, resp.StatusCode, truncate(body, 160))
	}
	return body, resp.StatusCode, nil
}

// initialDelta 获取 delta 基线链接（首次 delta 调用不带 token → 返回全量 + 末页 deltaLink）。
func (c *RealGraphConnector) initialDelta(ctx context.Context) (string, error) {
	path := "/me/messages/delta"
	deltaLink := ""
	for {
		page, dl, err := c.deltaPage(ctx, path, "")
		if err != nil {
			return "", err
		}
		if dl != "" {
			deltaLink = dl // deltaLink 只在末页出现，需翻完所有分页
		}
		if page.NextLink == "" {
			break
		}
		path = c.relPath(page.NextLink)
	}
	if deltaLink == "" {
		return "", fmt.Errorf("graph delta: no deltaLink in baseline response")
	}
	return deltaLink, nil
}

// listAll 分页列出全部消息（GET /me/messages）。
func (c *RealGraphConnector) listAll(ctx context.Context) ([]graphMessage, error) {
	var out []graphMessage
	path := "/me/messages"
	for {
		body, _, err := c.get(ctx, path)
		if err != nil {
			return nil, err
		}
		var page graphMessagePage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, err
		}
		for _, w := range page.Value {
			m, err := wireToGraphMessage(w)
			if err != nil {
				return nil, err
			}
			out = append(out, m)
		}
		if page.NextLink == "" {
			return out, nil
		}
		path = c.relPath(page.NextLink)
	}
}

// delta 查询增量：start=deltaLink 空则走初始基线；返回归一化记录与最新 deltaLink。
func (c *RealGraphConnector) delta(ctx context.Context, start string) ([]graphDeltaRecord, string, error) {
	path := "/me/messages/delta"
	if start != "" {
		path = c.relPath(start)
	}
	var records []graphDeltaRecord
	latest := start
	for {
		page, deltaLink, err := c.deltaPage(ctx, path, latest)
		if err != nil {
			return nil, "", err
		}
		rec := graphDeltaRecord{}
		for _, w := range page.Value {
			if w.Removed != nil {
				rec.Deleted = append(rec.Deleted, w.ID)
				continue
			}
			m, err := wireToGraphMessage(w)
			if err != nil {
				return nil, "", err
			}
			rec.Added = append(rec.Added, m)
		}
		if len(rec.Added) > 0 || len(rec.Deleted) > 0 {
			records = append(records, rec)
		}
		if deltaLink != "" {
			latest = deltaLink
		}
		if page.NextLink == "" {
			break
		}
		path = c.relPath(page.NextLink)
	}
	return records, latest, nil
}

// deltaPage 单次 delta 请求：返回该页消息与（如到页尾）deltaLink。
func (c *RealGraphConnector) deltaPage(ctx context.Context, path, fallback string) (graphMessagePage, string, error) {
	body, _, err := c.get(ctx, path)
	if err != nil {
		return graphMessagePage{}, "", err
	}
	var page graphMessagePage
	if err := json.Unmarshal(body, &page); err != nil {
		return graphMessagePage{}, "", err
	}
	deltaLink := page.DeltaLink
	if deltaLink == "" && page.DeltaToken != "" {
		// 部分实现只给 deltaToken → 拼回 delta 端点 query
		deltaLink = c.baseURL + "/me/messages/delta?$deltatoken=" + page.DeltaToken
	}
	if deltaLink == "" {
		deltaLink = fallback // 页尾未给新 deltaLink → 沿用上一轮
	}
	return page, deltaLink, nil
}

// relPath 从绝对 URL 提取相对路径（mock 返回完整 URL 时用；生产返回相对 @odata.nextLink）。
func (c *RealGraphConnector) relPath(u string) string {
	if strings.HasPrefix(u, c.baseURL) {
		return strings.TrimPrefix(u, c.baseURL)
	}
	return u
}

// ── wire → 归一化 ──────────────────────────────────────────────────────────

func wireToGraphMessage(w graphWireMessage) (graphMessage, error) {
	m := graphMessage{
		ID:             w.ID,
		Subject:        w.Subject,
		IsRead:         w.IsRead,
		HasAttachments: w.HasAttachments,
		SizeBytes:      w.Size,
		BodyPreview:    w.BodyPreview,
	}
	m.From = model.Address{Name: w.From.EmailAddress.Name, Email: w.From.EmailAddress.Address}
	m.To = graphRecipients(w.ToRecipients)
	m.Cc = graphRecipients(w.CcRecipients)
	m.Bcc = graphRecipients(w.BccRecipients)
	if w.Body.ContentType == "html" {
		m.BodyHTML = w.Body.Content
	} else {
		m.BodyText = w.Body.Content
	}
	if ts, err := parseGraphTime(w.Received); err == nil {
		m.ReceivedAt = ts
	}
	return m, nil
}

func graphRecipients(rs []graphRecipient) []model.Address {
	out := make([]model.Address, 0, len(rs))
	for _, r := range rs {
		out = append(out, model.Address{Name: r.EmailAddress.Name, Email: r.EmailAddress.Address})
	}
	return out
}

func parseGraphTime(s string) (int64, error) {
	// 2026-08-29T12:34:56Z
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, err
	}
	return t.UnixMilli(), nil
}

// toMail 归一化到 CanonicalMail（provider=graph，幂等键=消息 id，游标含 deltaLink）。
func (m graphMessage) toMail(account, deltaLink string) (model.CanonicalMail, error) {
	cm := model.CanonicalMail{
		ID:            m.ID,
		AccountID:     account,
		Provider:      model.ProviderGraph,
		Folder:        "INBOX",
		From:          m.From,
		To:            m.To,
		Cc:            m.Cc,
		Bcc:           m.Bcc,
		Subject:       m.Subject,
		BodyText:      m.BodyText,
		BodyHTML:      m.BodyHTML,
		Snippet:       m.BodyPreview,
		HasAttachment: m.HasAttachments,
		InternalDate:  m.ReceivedAt,
		SizeBytes:     m.SizeBytes,
		Read:          m.IsRead,
		Cursor:        model.SyncCursor{ProviderSpecific: map[string]string{"deltaLink": deltaLink}},
	}
	// 纯邮件正文（无 From 的异常消息）给个安全兜底
	if cm.From.Email == "" && m.Subject == "" {
		return cm, fmt.Errorf("graph message %s: empty from and subject", m.ID)
	}
	if cm.Snippet == "" {
		cm.Snippet = pop3Snippet(cm.BodyText)
	}
	return cm, nil
}
