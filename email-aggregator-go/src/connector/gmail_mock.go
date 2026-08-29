package connector

// 本文件为测试辅助：一个进程内、纯标准库实现的 mock Gmail API 服务端。
// 支持 Gmail API（REST）子集，用于驱动 RealGmailConnector 跑通端到端采集闭环：
//   - GET  /gmail/v1/users/{user}/profile      → emailAddress + historyId（游标基线）
//   - GET  /gmail/v1/users/{user}/messages     → 分页消息列表（maxResults/pageToken）
//   - GET  /gmail/v1/users/{user}/messages/{id}→ format=raw 返回 base64url RFC822
//   - GET  /gmail/v1/users/{user}/history      → startHistoryId 增量（added/deleted + 最新 historyId）
//
// 鉴权：设置了 token 后要求 Authorization: Bearer <token>，否则 401（验证 OAuth2 鉴权失败分支）。
// 设计为「非 _test 文件」是因为 syncsv（另一包）的集成测试也需要启动这个 server（同 StartMockPOP3）。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type mockGmailMsg struct {
	id           string
	threadID     string
	internalDate int64
	raw          string // RFC 822 原始报文
}

type mockGmailHist struct {
	id      int64
	added   []string
	deleted []string
}

// MockGmailServer 内嵌的进程内 mock Gmail API 服务端（仅用于测试）。
type MockGmailServer struct {
	ts       *httptest.Server
	token    string
	pageSize int

	mu     sync.Mutex
	msgs   map[string]*mockGmailMsg
	order  []string // 插入顺序（列表分页用）
	hist   []mockGmailHist
	nextID int
	curID  int64 // 最新 historyId
}

// StartMockGmailAPI 启动 mock Gmail API 服务端，返回实例、停止函数与错误。
func StartMockGmailAPI() (*MockGmailServer, func(), error) {
	s := &MockGmailServer{
		msgs:     map[string]*mockGmailMsg{},
		pageSize: 2, // 默认每页 2 条，便于测试分页
	}
	mux := http.NewServeMux()
	// 解析 /gmail/v1/users/{user}/... 形式的路径
	base := "/gmail/v1/users/"
	mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, base) // {user}/{subpath...}
		segs := strings.Split(rest, "/")
		if len(segs) == 0 || segs[0] == "" {
			http.Error(w, `{"error":"bad path"}`, http.StatusBadRequest)
			return
		}
		user := segs[0]
		if len(segs) < 2 {
			http.Error(w, `{"error":"bad path"}`, http.StatusBadRequest)
			return
		}
		sub := segs[1]

		if !s.authOK(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Invalid Credentials"}}`))
			return
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		switch {
		case sub == "profile":
			_ = user
			s.writeJSON(w, map[string]any{
				"emailAddress": user,
				"historyId":    strconv.FormatInt(s.curID, 10),
			})
		case sub == "messages" && len(segs) == 2:
			// 列表（分页）
			pageToken := r.URL.Query().Get("pageToken")
			max := s.pageSize
			if v := r.URL.Query().Get("maxResults"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					max = n
				}
			}
			start := 0
			if pageToken != "" {
				if n, err := strconv.Atoi(pageToken); err == nil {
					start = n
				}
			}
			end := start + max
			if end > len(s.order) {
				end = len(s.order)
			}
			var items []map[string]string
			for _, id := range s.order[start:end] {
				m := s.msgs[id]
				items = append(items, map[string]string{"id": m.id, "threadId": m.threadID})
			}
			resp := map[string]any{
				"messages":            items,
				"resultSizeEstimate":  len(s.order),
			}
			if end < len(s.order) {
				resp["nextPageToken"] = strconv.Itoa(end)
			}
			s.writeJSON(w, resp)
		case sub == "messages" && len(segs) == 3:
			// 单封（format=raw → 顶层 raw 字段）
			id := segs[2]
			m, ok := s.msgs[id]
			if !ok {
				http.Error(w, `{"error":{"code":404,"message":"Message not found"}}`, http.StatusNotFound)
				return
			}
			s.writeJSON(w, map[string]any{
				"id":            m.id,
				"threadId":      m.threadID,
				"internalDate":  m.internalDate,
				"sizeEstimate":  len(m.raw),
				"raw":           base64.RawURLEncoding.EncodeToString([]byte(m.raw)),
			})
		case sub == "history":
			start := int64(0)
			if v := r.URL.Query().Get("startHistoryId"); v != "" {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					start = n
				}
			}
			var recs []map[string]any
			for _, h := range s.hist {
				if h.id <= start {
					continue
				}
				rec := map[string]any{"id": h.id}
				if len(h.added) > 0 {
					var added []map[string]any
					for _, id := range h.added {
						added = append(added, map[string]any{"message": map[string]any{"id": id, "threadId": s.msgs[id].threadID}})
					}
					rec["messagesAdded"] = added
				}
				if len(h.deleted) > 0 {
					var del []map[string]any
					for _, id := range h.deleted {
						del = append(del, map[string]any{"message": map[string]any{"id": id}})
					}
					rec["messagesDeleted"] = del
				}
				recs = append(recs, rec)
			}
			s.writeJSON(w, map[string]any{
				"history":   recs,
				"historyId": strconv.FormatInt(s.curID, 10),
			})
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	})

	ts := httptest.NewServer(mux)
	s.ts = ts
	stop := ts.Close
	return s, stop, nil
}

func (s *MockGmailServer) authOK(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+s.token
}

func (s *MockGmailServer) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Addr 返回 mock 服务基地址（形如 http://127.0.0.1:port）。
func (s *MockGmailServer) Addr() string { return s.ts.URL }

// SetToken 设置要求客户端携带的 Bearer 令牌（空 = 不校验）。
func (s *MockGmailServer) SetToken(tok string) { s.token = tok }

// SetPageSize 设置列表分页大小（测试验证分页）。
func (s *MockGmailServer) SetPageSize(n int) { s.pageSize = n }

// Inject 注入一封邮件（RFC 822 原始报文 + 内部时间戳），返回生成的 Gmail 消息 ID，
// 并追加一条 messagesAdded 历史记录（historyId 递增）。
func (s *MockGmailServer) Inject(raw string, internalDate int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("gm-%d", s.nextID)
	s.curID++
	s.msgs[id] = &mockGmailMsg{id: id, threadID: id, internalDate: internalDate, raw: raw}
	s.order = append(s.order, id)
	s.hist = append(s.hist, mockGmailHist{id: s.curID, added: []string{id}})
	return id
}

// Delete 删除一封邮件并追加一条 messagesDeleted 历史记录（historyId 递增）。
func (s *MockGmailServer) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.msgs[id]; !ok {
		return
	}
	delete(s.msgs, id)
	s.curID++
	s.hist = append(s.hist, mockGmailHist{id: s.curID, deleted: []string{id}})
}

// Count 返回当前消息数。
func (s *MockGmailServer) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

// CurrentHistoryID 返回最新 historyId（测试断言游标）。
func (s *MockGmailServer) CurrentHistoryID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.curID
}

// IDs 返回按插入顺序的消息 ID 列表（测试断言）。
func (s *MockGmailServer) IDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.order...)
	sort.Strings(out) // 稳定输出
	return out
}
