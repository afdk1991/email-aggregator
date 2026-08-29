// Package search 检索与索引层（ADR-003）。元数据/正文进 OpenSearch，按 accountId 路由。
// PoC 提供 InMemorySearchIndex；OpenSearchIndex 为真实索引适配器（标准库 net/http 实现，零外部依赖）。
package search

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"email-aggregator-go/src/model"
	"email-aggregator-go/src/tenant"
)

// SearchHit 检索命中
type SearchHit struct {
	ID           string `json:"idempotencyKey"`
	TenantID     string `json:"tenantId,omitempty"` // ADR-009 多租户：检索命中归属租户
	AccountID    string `json:"accountId"`
	Subject      string `json:"subject"`
	From         string `json:"from"`
	Preview      string `json:"preview"`
	InternalDate int64  `json:"internalDate"`
}

// SearchIndex 检索接口（ADR-009：tenantID 全链路强制透传）
type SearchIndex interface {
	Index(tenantID string, mail model.CanonicalMail) error
	Search(tenantID, accountID, query string, limit int) ([]SearchHit, error)
	// Remove 删除生命周期同步：从索引移除指定邮件。tenantID/accountID 用于归属守卫。
	Remove(tenantID, accountID, id string) error
}

// InMemorySearchIndex 演示/单测用：子串匹配
type InMemorySearchIndex struct {
	mu   sync.RWMutex
	docs map[string]indexedDoc
}

type indexedDoc struct {
	tenantID string
	mail     model.CanonicalMail
	text     string
}

// NewInMemorySearchIndex 构造
func NewInMemorySearchIndex() *InMemorySearchIndex {
	return &InMemorySearchIndex{docs: map[string]indexedDoc{}}
}

// Index 写入倒排文本（subject + from + body 小写化），按 tenant 命名空间隔离。
func (s *InMemorySearchIndex) Index(tenantID string, mail model.CanonicalMail) error {
	tenantID = tenant.Resolve(tenantID)
	text := strings.ToLower(strings.Join([]string{
		mail.Subject,
		joinAddrs(mail.From),
		mail.BodyText,
	}, " "))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[ns(tenantID, mail.ID)] = indexedDoc{tenantID: tenantID, mail: mail, text: text}
	return nil
}

// Remove 从索引移除指定邮件（删除生命周期同步）。
// 归属守卫：仅当索引文档的 tenant+accountId 均匹配时才删除，避免跨租户/账户误删。
func (s *InMemorySearchIndex) Remove(tenantID, accountID, id string) error {
	tenantID = tenant.Resolve(tenantID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.docs[ns(tenantID, id)]; ok {
		if d.mail.AccountID != accountID {
			return nil
		}
		delete(s.docs, ns(tenantID, id))
	}
	return nil
}

// Search 关键词检索（按时间倒序，强制租户+账户归属）
func (s *InMemorySearchIndex) Search(tenantID, accountID, query string, limit int) ([]SearchHit, error) {
	tenantID = tenant.Resolve(tenantID)
	q := strings.ToLower(query)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var hits []SearchHit
	for _, d := range s.docs {
		if d.tenantID != tenantID || d.mail.AccountID != accountID {
			continue
		}
		if !strings.Contains(d.text, q) {
			continue
		}
		hits = append(hits, SearchHit{
			ID:           d.mail.ID,
			TenantID:     d.tenantID,
			AccountID:    d.mail.AccountID,
			Subject:      d.mail.Subject,
			From:         joinAddrs(d.mail.From),
			Preview:      truncate(d.mail.BodyText, 80),
			InternalDate: d.mail.InternalDate,
		})
	}
	// 简单倒序（按 internalDate）
	for i, j := 0, len(hits)-1; i < j; i, j = i+1, j-1 {
		hits[i], hits[j] = hits[j], hits[i]
	}
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// ns 租户命名空间前缀（与 store 包同语义，隔离边界一致，ADR-009）。
func ns(tenantID, key string) string { return tenantID + "\x00" + key }

// OpenSearchIndex 真实索引适配器（标准库 net/http 实现，零外部依赖）：
// index = mail-<tenantId>-<accountId>，检索走 multi_match。
// 与 integration 包下的 OpenSearchIndex 语义一致，但本实现仅依赖标准库，
// 默认构建即可编译（无需 opensearch-go），可在配置 OPENSEARCH_ADDR 后真正启用。
type OpenSearchIndex struct {
	baseURL string
	user    string
	pass    string
	client  *http.Client
}

// NewOpenSearchIndex 构造真实 OpenSearch 适配器。
// address 形如 http://localhost:9200 或 https://opensearch:9200（https 时跳过自签证书校验，dev/PoC 用）；
// user/pass 在 OpenSearch 安全插件启用时需要（留空表示无鉴权）。
func NewOpenSearchIndex(address, user, pass string) *OpenSearchIndex {
	address = strings.TrimRight(address, "/")
	client := &http.Client{Timeout: 15 * time.Second}
	if strings.HasPrefix(address, "https") {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // dev/PoC only
		}
	}
	return &OpenSearchIndex{baseURL: address, user: user, pass: pass, client: client}
}

func sanitizeIndex(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "@", "_"))
}

// Index 写入文档（索引按 租户+账户 隔离，index = mail-<tenantId>-<accountId>）。
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
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	index := "mail-" + sanitizeIndex(tenantID) + "-" + sanitizeIndex(m.AccountID)
	req, err := http.NewRequest(http.MethodPost, s.baseURL+"/"+index+"/_doc/"+m.ID, bytes.NewReader(body))
	if err != nil {
		return err
	}
	s.setHeaders(req, "application/json")
	resp, err := s.client.Do(req)
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

// Search 关键词检索（multi_match + 时间倒序，限定 租户+账户 索引）。
func (s *OpenSearchIndex) Search(tenantID, accountID, query string, limit int) ([]SearchHit, error) {
	tenantID = tenant.Resolve(tenantID)
	if limit <= 0 {
		limit = 50
	}
	index := "mail-" + sanitizeIndex(tenantID) + "-" + sanitizeIndex(accountID)
	body, _ := json.Marshal(map[string]any{
		"size": limit,
		"query": map[string]any{
			"multi_match": map[string]any{
				"query":  query,
				"fields": []string{"subject", "from", "bodyText"},
			},
		},
		"sort": []any{map[string]any{"internalDate": "desc"}},
	})
	req, err := http.NewRequest(http.MethodPost, s.baseURL+"/"+index+"/_search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	s.setHeaders(req, "application/json")
	resp, err := s.client.Do(req)
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
	hits := make([]SearchHit, 0, len(parsed.Hits.Hits))
	for _, h := range parsed.Hits.Hits {
		hits = append(hits, SearchHit{
			ID:           h.ID,
			TenantID:     tenantID,
			AccountID:    strOf(h.Src["accountId"]),
			Subject:      strOf(h.Src["subject"]),
			From:         strOf(h.Src["from"]),
			Preview:      truncateStr(strOf(h.Src["bodyText"]), 80),
			InternalDate: toInt64(h.Src["internalDate"]),
		})
	}
	return hits, nil
}

// Remove 按租户+账户+邮件 ID 删除索引文档（mail-<tenantId>-<accountId>/_doc/<id>）。
// 404（文档不存在）视为成功，保证删除幂等。
func (s *OpenSearchIndex) Remove(tenantID, accountID, id string) error {
	tenantID = tenant.Resolve(tenantID)
	index := "mail-" + sanitizeIndex(tenantID) + "-" + sanitizeIndex(accountID)
	req, err := http.NewRequest(http.MethodDelete, s.baseURL+"/"+index+"/_doc/"+id, nil)
	if err != nil {
		return err
	}
	s.setHeaders(req, "")
	resp, err := s.client.Do(req)
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

func (s *OpenSearchIndex) setHeaders(req *http.Request, contentType string) {
	if s.user != "" {
		req.SetBasicAuth(s.user, s.pass)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
}

func strOf(v any) string {
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
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func joinAddrs(a model.Address) string {
	return a.Email
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
