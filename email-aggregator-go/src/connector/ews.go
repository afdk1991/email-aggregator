package connector

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"email-aggregator-go/src/model"
)

// EWSConnector 真实 Exchange / Exchange Web Services 适配器（ADR-008 例外区落地）。
//
// 纯标准库实现（net/http + encoding/xml），零第三方依赖。通过 SOAP 1.1 调用 EWS：
//   - FindItem：列出收件箱邮件（IdOnly + 关键元数据）
//   - GetItem：按 ItemId 拉取邮件正文
//
// 认证支持 Basic（用户名/密码或应用密码）与 OAuth2（Bearer 令牌，供 XOAUTH2 流程产物使用）。
// 长连接推送采用 EWS 标准做法——轮询 FindItem 检测新邮件（EWS 无 IMAP IDLE 等价物），
// 发现新 ItemId 即经回调上送，ctx 取消即退出。
type EWSConnector struct {
	endpoint    string
	httpClient  *http.Client
	cred        model.Credential
	cap         model.ConnectorCapabilities
	user        string
	pollInterval time.Duration
}

// NewEWSConnector 构造 Exchange/EWS 连接器。endpoint 缺省使用 Office365 公共 EWS 地址。
func NewEWSConnector(endpoint string) *EWSConnector {
	if endpoint == "" {
		endpoint = "https://outlook.office365.com/EWS/Exchange.asmx"
	}
	return &EWSConnector{
		endpoint:     endpoint,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		pollInterval: 30 * time.Second,
		cap: model.ConnectorCapabilities{
			Provider:            model.ProviderExchange,
			SupportsIncremental: true,
			SupportsOAuth:       true,
		},
	}
}

// SetPollInterval 设置 StreamChanges 轮询间隔（测试可注入短值）。
func (c *EWSConnector) SetPollInterval(d time.Duration) { c.pollInterval = d }

// ── model.Connector 接口实现 ───────────────────────────────────────────────

// Connect 保存凭据（真实环境据此设置 Basic / Bearer 认证头）。
func (c *EWSConnector) Connect(_ context.Context, cred model.Credential) error {
	c.cred = cred
	c.user = cred.Username
	return nil
}

// Capabilities 返回能力声明
func (c *EWSConnector) Capabilities() model.ConnectorCapabilities { return c.cap }

// InitialFullSync 初始全量：FindItem 收件箱 → 逐封 GetItem 取正文 → 经 sink 回传。
func (c *EWSConnector) InitialFullSync(ctx context.Context, since int64, sink model.SyncSink) error {
	ids, err := c.findItems(ctx, since)
	if err != nil {
		return err
	}
	return c.fetchAndSink(ctx, ids, sink)
}

// IncrementalSync 增量：以 HighWaterMark（接收时间 Unix 秒，字符串）为下界 FindItem，
// 拉取新邮件。EWS 为基于时间的协议，游标高位水位即最近接收时间；
// HighWaterMark 为空或非法时回退为 0（拉取全部，由上层去重幂等）。
func (c *EWSConnector) IncrementalSync(ctx context.Context, cursor model.SyncCursor, sink model.SyncSink) error {
	since := int64(0)
	if cursor.HighWaterMark != "" {
		if v, err := strconv.ParseInt(cursor.HighWaterMark, 10, 64); err == nil {
			since = v
		}
	}
	ids, err := c.findItems(ctx, since)
	if err != nil {
		return err
	}
	return c.fetchAndSink(ctx, ids, sink)
}

// StreamChanges 长连接推送（EWS 轮询实现）：周期 FindItem，发现新邮件即回调，ctx 取消退出。
func (c *EWSConnector) StreamChanges(ctx context.Context, _ model.SyncCursor, onEvent func(ctx context.Context, m model.CanonicalMail) error) error {
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	seen := map[string]bool{}
	// 初始拉全量标记已见，避免首轮把历史全推一遍
	if initial, err := c.findItems(ctx, 0); err == nil {
		for _, it := range initial {
			seen[it.id] = true
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			items, err := c.findItems(ctx, 0)
			if err != nil {
				continue
			}
			for _, it := range items {
				if seen[it.id] {
					continue
				}
				seen[it.id] = true
				body, _ := c.getItem(ctx, it.id, it.changeKey)
				m := it.toMail(c.user, body)
				if err := onEvent(ctx, m); err != nil {
					return err
				}
			}
		}
	}
}

// Close 释放连接（HTTP 无状态，预留钩子）。
func (c *EWSConnector) Close() error { return nil }

// ── 内部：EWS SOAP 调用 ────────────────────────────────────────────────────

type ewsItem struct {
	id        string
	changeKey string
	subject   string
	fromName  string
	fromEmail string
	received  int64
	hasAttach bool
	size      int64
	isRead    bool
}

func (e ewsItem) toMail(account, body string) model.CanonicalMail {
	return model.CanonicalMail{
		ID:           e.id,
		AccountID:    account,
		Folder:       "INBOX",
		Provider:     model.ProviderExchange,
		From:         model.Address{Name: e.fromName, Email: e.fromEmail},
		Subject:      e.subject,
		BodyText:     body,
		InternalDate: e.received,
		SizeBytes:    e.size,
		HasAttachment: e.hasAttach,
		Read:         e.isRead,
	}
}

func (c *EWSConnector) authHeaders() map[string]string {
	h := map[string]string{"Content-Type": "text/xml; charset=utf-8"}
	switch {
	case c.cred.OAuth != nil && c.cred.OAuth.AccessToken != "":
		h["Authorization"] = "Bearer " + c.cred.OAuth.AccessToken
	case c.cred.Username != "":
		raw := c.cred.Username + ":" + c.cred.Password
		h["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
	}
	return h
}

func (c *EWSConnector) findItems(ctx context.Context, since int64) ([]ewsItem, error) {
	body := ewsFindItemRequest()
	resp, err := c.soap(ctx, body)
	if err != nil {
		return nil, err
	}
	var env findItemEnvelope
	if err := xml.Unmarshal(resp, &env); err != nil {
		return nil, fmt.Errorf("ews finditem unmarshal: %w", err)
	}
	out := make([]ewsItem, 0, len(env.Body.FindItemResp.ResponseMessages.Msg.RootFolder.Items.Messages))
	for _, m := range env.Body.FindItemResp.ResponseMessages.Msg.RootFolder.Items.Messages {
		it := ewsItem{
			id:        m.ItemID.ID,
			changeKey: m.ItemID.ChangeKey,
			subject:   m.Subject,
			fromName:  m.From.Mailbox.Name,
			fromEmail: m.From.Mailbox.Email,
			received:  parseEWSTime(m.DateTimeReceived),
			hasAttach: strings.EqualFold(m.HasAttachments, "true"),
			size:      atoi64xml(m.Size),
			isRead:    strings.EqualFold(m.IsRead, "true"),
		}
		if since > 0 && it.received < since {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

func (c *EWSConnector) getItem(ctx context.Context, id, changeKey string) (string, error) {
	body := ewsGetItemRequest(id, changeKey)
	resp, err := c.soap(ctx, body)
	if err != nil {
		return "", err
	}
	var env getItemEnvelope
	if err := xml.Unmarshal(resp, &env); err != nil {
		return "", fmt.Errorf("ews getitem unmarshal: %w", err)
	}
	if len(env.Body.GetItemResp.ResponseMessages.Msg.Items.Messages) == 0 {
		return "", nil
	}
	return env.Body.GetItemResp.ResponseMessages.Msg.Items.Messages[0].Body, nil
}

func (c *EWSConnector) fetchAndSink(ctx context.Context, ids []ewsItem, sink model.SyncSink) error {
	for _, it := range ids {
		body, _ := c.getItem(ctx, it.id, it.changeKey)
		if err := sink.OnMessage(ctx, it.toMail(c.user, body)); err != nil {
			return err
		}
	}
	return nil
}

func (c *EWSConnector) soap(ctx context.Context, body string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range c.authHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ews http status=%d body=%s", resp.StatusCode, truncateBytes(data, 200))
	}
	return data, nil
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n])
}

// ── SOAP 请求模板 ──────────────────────────────────────────────────────────

func ewsFindItemRequest() string {
	return `<?xml version="1.0" encoding="utf-8"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/" xmlns:t="http://schemas.microsoft.com/exchange/services/2006/types" xmlns:m="http://schemas.microsoft.com/exchange/services/2006/messages">
  <soap:Header><t:RequestServerVersion Version="Exchange2013"/></soap:Header>
  <soap:Body>
    <m:FindItem Traversal="Shallow">
      <m:ItemShape>
        <t:BaseShape>IdOnly</t:BaseShape>
        <t:AdditionalProperties>
          <t:FieldURI FieldURI="item:Subject"/>
          <t:FieldURI FieldURI="message:From"/>
          <t:FieldURI FieldURI="item:DateTimeReceived"/>
          <t:FieldURI FieldURI="item:HasAttachments"/>
          <t:FieldURI FieldURI="item:Size"/>
          <t:FieldURI FieldURI="item:IsRead"/>
        </t:AdditionalProperties>
      </m:ItemShape>
      <m:ParentFolderIds><t:DistinguishedFolderId Id="inbox"/></m:ParentFolderIds>
    </m:FindItem>
  </soap:Body>
</soap:Envelope>`
}

func ewsGetItemRequest(id, changeKey string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/" xmlns:t="http://schemas.microsoft.com/exchange/services/2006/types" xmlns:m="http://schemas.microsoft.com/exchange/services/2006/messages">
  <soap:Header><t:RequestServerVersion Version="Exchange2013"/></soap:Header>
  <soap:Body>
    <m:GetItem>
      <m:ItemShape><t:BaseShape>Default</t:BaseShape></m:ItemShape>
      <m:ItemIds><t:ItemId Id="%s" ChangeKey="%s"/></m:ItemIds>
    </m:GetItem>
  </soap:Body>
</soap:Envelope>`, id, changeKey)
}

// ── SOAP 响应 XML 解析结构（按局部名匹配，忽略命名空间前缀）─────────────────

type findItemEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    struct {
		FindItemResp struct {
			ResponseMessages struct {
				Msg struct {
					RootFolder struct {
						Items struct {
							Messages []ewsRespMessage `xml:"Message"`
						} `xml:"Items"`
					} `xml:"RootFolder"`
				} `xml:"FindItemResponseMessage"`
			} `xml:"ResponseMessages"`
		} `xml:"FindItemResponse"`
	} `xml:"Body"`
}

type getItemEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    struct {
		GetItemResp struct {
			ResponseMessages struct {
				Msg struct {
					Items struct {
						Messages []struct {
							ItemID  xmlItemID `xml:"ItemId"`
							Body    string    `xml:"Body"`
							Subject string    `xml:"Subject"`
						} `xml:"Message"`
					} `xml:"Items"`
				} `xml:"GetItemResponseMessage"`
			} `xml:"ResponseMessages"`
		} `xml:"GetItemResponse"`
	} `xml:"Body"`
}

type ewsRespMessage struct {
	ItemID         xmlItemID `xml:"ItemId"`
	Subject        string    `xml:"Subject"`
	From           struct {
		Mailbox struct {
			Name  string `xml:"Name"`
			Email string `xml:"EmailAddress"`
		} `xml:"Mailbox"`
	} `xml:"From"`
	DateTimeReceived string `xml:"DateTimeReceived"`
	HasAttachments   string `xml:"HasAttachments"`
	Size             string `xml:"Size"`
	IsRead           string `xml:"IsRead"`
}

type xmlItemID struct {
	ID        string `xml:"Id,attr"`
	ChangeKey string `xml:"ChangeKey,attr"`
}

func parseEWSTime(s string) int64 {
	s = strings.TrimSpace(s)
	for _, l := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04:05Z07:00"} {
		if t, err := time.Parse(l, s); err == nil {
			return t.Unix()
		}
	}
	if t, err := time.Parse("2006-01-02T15:04:05Z", s); err == nil {
		return t.Unix()
	}
	return 0
}

func atoi64xml(s string) int64 {
	var n int64
	fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n
}
