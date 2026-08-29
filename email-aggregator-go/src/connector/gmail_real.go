package connector

// RealGmailConnector 真实 Gmail 适配器（Gmail API REST + OAuth2 Bearer，纯标准库实现，零第三方依赖）。
//
// 与架构文档 §10/§12 对齐：Gmail 走官方 API（非 IMAP 复用），
//   - 鉴权：OAuth2 Bearer（XOAUTH2 产物即 access token）
//   - 全量：messages.list（分页）+ messages.get(format=raw)，游标基线取 profile.historyId
//   - 增量：history.list(startHistoryId) → messagesAdded / messagesDeleted，
//           游标 = SyncCursor.ProviderSpecific["historyId"]
//   - 推送：Gmail 官方用 Cloud Pub/Sub；本适配器 SupportsIdle=false，
//           StreamChanges 采用轮询（对齐 EWS/POP3），默认间隔可注入。
//
// 通过 model.Connector 接口与 sync 编排层（syncsvc.Orchestrator）对接；
// 测试用内嵌 mock Gmail API（StartMockGmailAPI）端到端验证。
//
// 游标说明：Gmail historyId 单调递增，history.list 每次都会返回最新 historyId。
// PoC 中该游标通过「每封新增邮件的 Cursor.ProviderSpecific["historyId"]」随 sink 回传持久化；
// 空批次/纯删除批次无新增邮件时，由调用方取 LastHistoryID() 持久化（生产环境由连接服务统一落盘）。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"email-aggregator-go/src/model"
)

// RealGmailConnector 真实 Gmail API 连接器。
type RealGmailConnector struct {
	baseURL      string
	httpClient   *http.Client
	user         string
	token        string
	pollInterval time.Duration
	cap          model.ConnectorCapabilities
	lastHistID   int64 // 最近一次同步感知到的最新 historyId（供调用方持久化空批次游标）
}

// NewRealGmailConnector 构造 Gmail 连接器。
// baseURL 缺省使用生产 Gmail API 地址；测试注入 mock 地址。
func NewRealGmailConnector(baseURL string) *RealGmailConnector {
	if baseURL == "" {
		baseURL = "https://gmail.googleapis.com/gmail/v1"
	}
	return &RealGmailConnector{
		baseURL:      strings.TrimRight(baseURL, "/"),
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		user:         "me",
		pollInterval: 30 * time.Second,
		cap: model.ConnectorCapabilities{
			Provider:            model.ProviderGmail,
			SupportsIncremental: true, // historyId 增量
			SupportsOAuth:       true, // Bearer token（XOAUTH2 产物）
			SupportsIdle:        false,
			SupportsQresync:     false,
			SupportsCondstore:   false,
			SupportsUIDL:        false,
		},
	}
}

// SetPollInterval 设置 StreamChanges 轮询间隔（测试可注入短值）。
func (c *RealGmailConnector) SetPollInterval(d time.Duration) { c.pollInterval = d }

// SetHTTPClient 注入 HTTP 客户端（测试/代理场景）。
func (c *RealGmailConnector) SetHTTPClient(hc *http.Client) {
	if hc != nil {
		c.httpClient = hc
	}
}

// LastHistoryID 返回最近一次同步感知到的最新 historyId（空批次游标持久化用）。
func (c *RealGmailConnector) LastHistoryID() string {
	if c.lastHistID == 0 {
		return ""
	}
	return strconv.FormatInt(c.lastHistID, 10)
}

// ── model.Connector 接口实现 ───────────────────────────────────────────────

// Connect 校验 OAuth2 凭据并做一次 profile 探测（401 → 鉴权失败，快速失败）。
func (c *RealGmailConnector) Connect(ctx context.Context, cred model.Credential) error {
	tok := ""
	if cred.OAuth != nil && cred.OAuth.AccessToken != "" {
		tok = cred.OAuth.AccessToken
	} else if cred.Extra != nil {
		tok = cred.Extra["accessToken"]
	}
	if tok == "" {
		return fmt.Errorf("gmail: oauth2 access token required")
	}
	if cred.Username != "" {
		c.user = cred.Username
	}
	c.token = tok

	if _, _, err := c.get(ctx, "/users/"+c.user+"/profile", ""); err != nil {
		return fmt.Errorf("gmail connect (profile probe): %w", err)
	}
	return nil
}

// Capabilities 返回能力声明。
func (c *RealGmailConnector) Capabilities() model.ConnectorCapabilities { return c.cap }

// InitialFullSync 初始全量：分页列出全部消息 → 逐封拉取 raw → 解析，按 since 过滤经 sink 回传。
// 游标基线 = profile.historyId（此后以此为断点进入增量）。
func (c *RealGmailConnector) InitialFullSync(ctx context.Context, since int64, sink model.SyncSink) error {
	// 游标基线：profile 返回当前 historyId
	baseline, err := c.profileHistoryID(ctx)
	if err != nil {
		return err
	}
	c.lastHistID = baseline

	ids, err := c.listAll(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		raw, size, internalDate, err := c.getRaw(ctx, id)
		if err != nil {
			return err
		}
		m, err := parseGmailMessage(raw, c.user, id, size, internalDate)
		if err != nil {
			return fmt.Errorf("gmail parse %s: %w", id, err)
		}
		if m.InternalDate < since {
			continue
		}
		m.Cursor = model.SyncCursor{ProviderSpecific: map[string]string{"historyId": strconv.FormatInt(baseline, 10)}}
		if err := sink.OnMessage(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// IncrementalSync 增量：history.list(startHistoryId) → added/deleted，游标 = 响应最新 historyId。
func (c *RealGmailConnector) IncrementalSync(ctx context.Context, cursor model.SyncCursor, sink model.SyncSink) error {
	start := int64(0)
	if v := cursor.ProviderSpecific["historyId"]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			start = n
		}
	}
	recs, latest, err := c.history(ctx, start)
	if err != nil {
		return err
	}
	c.lastHistID = latest

	for _, rec := range recs {
		for _, id := range rec.Added {
			raw, size, internalDate, err := c.getRaw(ctx, id)
			if err != nil {
				return err
			}
			m, err := parseGmailMessage(raw, c.user, id, size, internalDate)
			if err != nil {
				return fmt.Errorf("gmail parse %s: %w", id, err)
			}
			m.Cursor = model.SyncCursor{ProviderSpecific: map[string]string{"historyId": strconv.FormatInt(latest, 10)}}
			if err := sink.OnMessage(ctx, m); err != nil {
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

// StreamChanges 推送（轮询实现，对齐 EWS/POP3）：周期 history 增量，新增邮件回调，ctx 取消退出。
func (c *RealGmailConnector) StreamChanges(ctx context.Context, cursor model.SyncCursor, onEvent func(ctx context.Context, m model.CanonicalMail) error) error {
	start := int64(0)
	if v := cursor.ProviderSpecific["historyId"]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			start = n
		}
	}
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil // 已取消，正常退出
		}
		recs, latest, err := c.history(ctx, start)
		if err != nil {
			if ctx.Err() != nil {
				return nil // 取消竞态：ctx 恰好在请求期间过期 → 优雅退出而非报错
			}
			return err
		}
		c.lastHistID = latest
		for _, rec := range recs {
			for _, id := range rec.Added {
				raw, size, internalDate, err := c.getRaw(ctx, id)
				if err != nil {
					return err
				}
				m, err := parseGmailMessage(raw, c.user, id, size, internalDate)
				if err != nil {
					return err
				}
				m.Cursor = model.SyncCursor{ProviderSpecific: map[string]string{"historyId": strconv.FormatInt(latest, 10)}}
				if err := onEvent(ctx, m); err != nil {
					return err
				}
			}
			// 删除在流模式仅忽略（onEvent 无删除通道；删除由增量模式 OnDelete 处理）
		}
		if latest > start {
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
func (c *RealGmailConnector) Close() error { return nil }

// ── Gmail API 原语 ───────────────────────────────────────────────────────────

type gmailMessageList struct {
	Messages      []struct{ ID, ThreadID string } `json:"messages"`
	NextPageToken string                         `json:"nextPageToken"`
}

type gmailMessage struct {
	ID           string `json:"id"`
	ThreadID     string `json:"threadId"`
	InternalDate int64  `json:"internalDate"`
	SizeEstimate int    `json:"sizeEstimate"`
	Raw          string `json:"raw"`
}

type gmailProfile struct {
	EmailAddress string `json:"emailAddress"`
	HistoryID    string `json:"historyId"`
}

type gmailHistoryWire struct {
	ID              int64 `json:"id"`
	MessagesAdded   []struct {
		Message struct {
			ID       string `json:"id"`
			ThreadID string `json:"threadId"`
		} `json:"message"`
	} `json:"messagesAdded"`
	MessagesDeleted []struct {
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	} `json:"messagesDeleted"`
}

// gmailHistoryRecord 归一化后的增量记录（added/deleted 为消息 ID 列表）。
type gmailHistoryRecord struct {
	ID      int64
	Added   []string
	Deleted []string
}

type gmailHistoryList struct {
	History   []gmailHistoryWire `json:"history"`
	HistoryID string             `json:"historyId"`
}

// get 执行带 Bearer 的 GET，返回响应体与状态码（非 2xx 报错）。
func (c *RealGmailConnector) get(ctx context.Context, path, query string) ([]byte, int, error) {
	u := c.baseURL + path
	if query != "" {
		u += "?" + query
	}
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
		return body, resp.StatusCode, fmt.Errorf("gmail GET %s: status=%d body=%s", path, resp.StatusCode, truncate(body, 160))
	}
	return body, resp.StatusCode, nil
}

func (c *RealGmailConnector) profileHistoryID(ctx context.Context) (int64, error) {
	body, _, err := c.get(ctx, "/users/"+c.user+"/profile", "")
	if err != nil {
		return 0, err
	}
	var p gmailProfile
	if err := json.Unmarshal(body, &p); err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(p.HistoryID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("gmail profile historyId: %w", err)
	}
	return n, nil
}

// listAll 分页列出全部消息 ID。
func (c *RealGmailConnector) listAll(ctx context.Context) ([]string, error) {
	var out []string
	token := ""
	for {
		q := "maxResults=50"
		if token != "" {
			q += "&pageToken=" + token
		}
		body, _, err := c.get(ctx, "/users/"+c.user+"/messages", q)
		if err != nil {
			return nil, err
		}
		var l gmailMessageList
		if err := json.Unmarshal(body, &l); err != nil {
			return nil, err
		}
		for _, m := range l.Messages {
			out = append(out, m.ID)
		}
		if l.NextPageToken == "" {
			return out, nil
		}
		token = l.NextPageToken
	}
}

// getRaw 拉取一封消息（format=raw，base64url 解码为 RFC822 字节）。
func (c *RealGmailConnector) getRaw(ctx context.Context, id string) ([]byte, int64, int64, error) {
	body, _, err := c.get(ctx, "/users/"+c.user+"/messages/"+id, "format=raw")
	if err != nil {
		return nil, 0, 0, err
	}
	var m gmailMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, 0, 0, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(m.Raw)
	if err != nil {
		// 兼容带 padding 的 base64url
		raw, err = base64.URLEncoding.DecodeString(m.Raw)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("gmail decode raw %s: %w", id, err)
		}
	}
	return raw, int64(m.SizeEstimate), m.InternalDate, nil
}

// history 查询增量：返回 startHistoryId 之后的记录与最新 historyId。
func (c *RealGmailConnector) history(ctx context.Context, start int64) ([]gmailHistoryRecord, int64, error) {
	q := "startHistoryId=" + strconv.FormatInt(start, 10)
	body, _, err := c.get(ctx, "/users/"+c.user+"/history", q)
	if err != nil {
		return nil, 0, err
	}
	var h gmailHistoryList
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, 0, err
	}
	latest, err := strconv.ParseInt(h.HistoryID, 10, 64)
	if err != nil {
		return nil, 0, fmt.Errorf("gmail historyId: %w", err)
	}
	recs := make([]gmailHistoryRecord, 0, len(h.History))
	for _, w := range h.History {
		r := gmailHistoryRecord{ID: w.ID}
		for _, a := range w.MessagesAdded {
			r.Added = append(r.Added, a.Message.ID)
		}
		for _, d := range w.MessagesDeleted {
			r.Deleted = append(r.Deleted, d.Message.ID)
		}
		recs = append(recs, r)
	}
	return recs, latest, nil
}

// ── RFC 822 → CanonicalMail 解析（共享，provider 参数化）────────────────────────

// parseRFC822Message 用 net/mail 解析原始 RFC 822 邮件。
// id = 幂等键（POP3=UIDL / Gmail=messageId），provider 归一化到 CanonicalMail.Provider。
func parseRFC822Message(raw []byte, account, id string, size int64, provider model.Provider) (model.CanonicalMail, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return model.CanonicalMail{}, fmt.Errorf("parse mime: %w", err)
	}
	h := msg.Header

	m := model.CanonicalMail{
		ID:            id,
		AccountID:     account,
		Provider:      provider,
		Folder:        "INBOX",
		SizeBytes:     size,
		HasAttachment: false,
	}
	if f, err := h.AddressList("From"); err == nil && len(f) > 0 {
		m.From = pop3Address(f[0])
	}
	if t, err := h.AddressList("To"); err == nil {
		m.To = pop3Addresses(t)
	}
	if cc, err := h.AddressList("Cc"); err == nil {
		m.Cc = pop3Addresses(cc)
	}
	if d, err := h.Date(); err == nil {
		m.InternalDate = d.UnixMilli()
	}
	m.Subject = decodePOP3Header(h.Get("Subject"))
	m.BodyText = pop3Body(msg, h)
	m.Snippet = pop3Snippet(m.BodyText)
	return m, nil
}

// parseGmailMessage 解析 Gmail raw 邮件（provider=gmail，幂等键=messageId）。
func parseGmailMessage(raw []byte, account, id string, size, internalDate int64) (model.CanonicalMail, error) {
	m, err := parseRFC822Message(raw, account, id, size, model.ProviderGmail)
	if err != nil {
		return m, err
	}
	if m.InternalDate == 0 && internalDate > 0 {
		m.InternalDate = internalDate // Gmail API internalDate 兜底（头部缺失时）
	}
	return m, nil
}

// ── 复用 POP3 解析辅助 ────────────────────────────────────────────────────────
// pop3Address / pop3Addresses / pop3Body / pop3Snippet / decodePOP3Header 定义于 pop3_real.go，
// 同包共享，无需重复实现。

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
