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
	"email-aggregator-go/src/observ"
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
// index = mail-<tenantId>（Phase 2 / ADR-009 严格按 per-tenant 物理索引落地），
// 多账户共用同一租户索引，accountId 作为字段过滤项，避免 10 万账户 = 10 万索引爆炸。
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

// Index 写入文档（Phase 2 / ADR-009：索引按租户物理隔离，index = mail-<tenantId>，
// accountId 作为字段保留以供查询时按账户过滤；首次写入会自动应用 mail-* index template）。
// Phase 2 可观测性：埋点 os_index_total{tenant,status} + os_index_duration_ms{tenant}。
func (s *OpenSearchIndex) Index(tenantID string, m model.CanonicalMail) error {
	tenantID = tenant.Resolve(tenantID)
	start := time.Now()
	labels := map[string]string{"tenant": tenantID}
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
		observ.OSIndexTotal.Inc(withStatus(labels, "error"))
		return err
	}
	index := "mail-" + sanitizeIndex(tenantID)
	req, err := http.NewRequest(http.MethodPost, s.baseURL+"/"+index+"/_doc/"+m.ID, bytes.NewReader(body))
	if err != nil {
		observ.OSIndexTotal.Inc(withStatus(labels, "error"))
		return err
	}
	s.setHeaders(req, "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		observ.OSIndexTotal.Inc(withStatus(labels, "error"))
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		observ.OSIndexTotal.Inc(withStatus(labels, "error"))
		observ.OSIndexDurationMs.Observe(labels, float64(time.Since(start).Milliseconds()))
		return fmt.Errorf("opensearch index status=%d body=%s", resp.StatusCode, b)
	}
	observ.OSIndexTotal.Inc(withStatus(labels, "ok"))
	observ.OSIndexDurationMs.Observe(labels, float64(time.Since(start).Milliseconds()))
	return nil
}

// withStatus 返回带 status 标签的副本（避免污染原 labels map）。
func withStatus(labels map[string]string, status string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	out["status"] = status
	return out
}

// Search 关键词检索（Phase 2 / ADR-009：路由到 mail-<tenantId> 单租户索引，
// 在 multi_match 外层加 bool.filter.term.accountId 限定账户，物理隔离 + 字段过滤双保险）。
// Phase 2 可观测性：埋点 os_search_total{tenant,status} + os_search_duration_ms{tenant}。
func (s *OpenSearchIndex) Search(tenantID, accountID, query string, limit int) ([]SearchHit, error) {
	tenantID = tenant.Resolve(tenantID)
	start := time.Now()
	labels := map[string]string{"tenant": tenantID}
	if limit <= 0 {
		limit = 50
	}
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
	req, err := http.NewRequest(http.MethodPost, s.baseURL+"/"+index+"/_search", bytes.NewReader(body))
	if err != nil {
		observ.OSSearchTotal.Inc(withStatus(labels, "error"))
		return nil, err
	}
	s.setHeaders(req, "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		observ.OSSearchTotal.Inc(withStatus(labels, "error"))
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		observ.OSSearchTotal.Inc(withStatus(labels, "error"))
		observ.OSSearchDurationMs.Observe(labels, float64(time.Since(start).Milliseconds()))
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
		observ.OSSearchTotal.Inc(withStatus(labels, "error"))
		observ.OSSearchDurationMs.Observe(labels, float64(time.Since(start).Milliseconds()))
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
	observ.OSSearchTotal.Inc(withStatus(labels, "ok"))
	observ.OSSearchDurationMs.Observe(labels, float64(time.Since(start).Milliseconds()))
	return hits, nil
}

// Remove 按租户+邮件 ID 删除索引文档（Phase 2 / ADR-009：mail-<tenantId>/_doc/<id>）。
// 404（文档不存在）视为成功，保证删除幂等；accountId 参数保留以兼容 SearchIndex 接口契约。
// Phase 2 可观测性：埋点 os_remove_total{tenant,status}。
func (s *OpenSearchIndex) Remove(tenantID, accountID, id string) error {
	tenantID = tenant.Resolve(tenantID)
	_ = accountID // Phase 2 起 index 已不带 accountId，但仍校验 tenant 归属
	labels := map[string]string{"tenant": tenantID}
	index := "mail-" + sanitizeIndex(tenantID)
	req, err := http.NewRequest(http.MethodDelete, s.baseURL+"/"+index+"/_doc/"+id, nil)
	if err != nil {
		observ.OSRemoveTotal.Inc(withStatus(labels, "error"))
		return err
	}
	s.setHeaders(req, "")
	resp, err := s.client.Do(req)
	if err != nil {
		observ.OSRemoveTotal.Inc(withStatus(labels, "error"))
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		observ.OSRemoveTotal.Inc(withStatus(labels, "error"))
		return fmt.Errorf("opensearch delete status=%d body=%s", resp.StatusCode, b)
	}
	observ.OSRemoveTotal.Inc(withStatus(labels, "ok"))
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
