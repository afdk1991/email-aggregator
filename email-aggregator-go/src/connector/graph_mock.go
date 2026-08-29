package connector

// MockGraphServer 内嵌 mock Microsoft Graph API（httptest），用于 connector 端到端测试。
// 对齐 MockGmailServer 模式：
//   - /me                → 用户档案（Connect 探测）
//   - /me/messages       → 全量分页（@odata.nextLink）
//   - /me/messages/delta → 首次（无 token）返回全量 + deltaLink；带 token 返回增量
//                          （新增 + @removed 删除条目），支持翻页
//   - 鉴权：Authorization: Bearer <token>（SetToken 设置，默认 "test-token"）
//
// 增量模型（确定性、可测）：
//   - deltaSeq 单调递增的「窗口号」；Inject 把 msg.Seq=当前窗口、Delete 把 msg.DelSeq=当前窗口
//   - delta 带 token wN 时：added = 未删且 Seq>N 的消息；deleted = DelSeq>N 的消息
//   - 基线（无 token）返回全量 live 消息 + deltaLink=w{当前窗口}，然后窗口自增
//
// 线程安全：内部加锁；适合测试注入 baseURL。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type mockGraphMsg struct {
	ID         string
	Subject    string
	From       string
	FromName   string
	Body       string
	ReceivedAt int64
	Size       int64
	Seq        int64 // 注入时的窗口号
	DelSeq     int64 // 删除时的窗口号（0=未删除）
	HasAttach  bool
	IsRead     bool
}

// MockGraphServer mock Graph API 服务器。
type MockGraphServer struct {
	ts       *httptest.Server
	mu       sync.Mutex
	token    string
	msgs     []mockGraphMsg
	pageSize int
	deltaSeq int64
	nextID   int
}

// StartMockGraphAPI 启动 mock Graph 服务器。
func StartMockGraphAPI() (*MockGraphServer, func(), error) {
	s := &MockGraphServer{
		token:    "test-token",
		pageSize: 2,
		deltaSeq: 1,
		nextID:   1,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	ts := httptest.NewServer(mux)
	s.ts = ts
	return s, func() { ts.Close() }, nil
}

// Addr 返回 mock 服务器基址。
func (s *MockGraphServer) Addr() string { return s.ts.URL }

// SetToken 设置期望的 Bearer token。
func (s *MockGraphServer) SetToken(tok string) { s.mu.Lock(); defer s.mu.Unlock(); s.token = tok }

// SetPageSize 设置分页大小。
func (s *MockGraphServer) SetPageSize(n int) { s.mu.Lock(); defer s.mu.Unlock(); s.pageSize = n }

// Inject 注入一封邮件（窗口号=当前 deltaSeq），返回 id。
func (s *MockGraphServer) Inject(subject, from, body string, receivedAt int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := "msg" + strconv.Itoa(s.nextID)
	s.nextID++
	s.msgs = append(s.msgs, mockGraphMsg{
		ID: id, Subject: subject, From: from, FromName: from,
		Body: body, ReceivedAt: receivedAt, Size: int64(len(body) + 100),
		Seq: s.deltaSeq,
	})
	return id
}

// Delete 删除一封邮件（DelSeq=当前窗口；下次 delta 窗口内以 @removed 呈现）。
func (s *MockGraphServer) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.msgs {
		if s.msgs[i].ID == id {
			s.msgs[i].DelSeq = s.deltaSeq
			return
		}
	}
}

// Count 当前消息数。
func (s *MockGraphServer) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

// IDs 返回全部消息 id（排序稳定）。
func (s *MockGraphServer) IDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.msgs))
	for _, m := range s.msgs {
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out
}

// ── HTTP handler ──────────────────────────────────────────────────────────────

func (s *MockGraphServer) handle(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"AuthenticationError"}}`))
		return
	}
	switch r.URL.Path {
	case "/me":
		s.writeJSON(w, map[string]any{"displayName": "Mock User", "mail": "mock@example.com", "id": "me"})
	case "/me/messages":
		s.handleList(w, r)
	case "/me/messages/delta":
		s.handleDelta(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"NotFound"}}`))
	}
}

func (s *MockGraphServer) authOK(r *http.Request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return r.Header.Get("Authorization") == "Bearer "+s.token
}

func (s *MockGraphServer) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *MockGraphServer) baseURL() string { return s.ts.URL }

// handleList GET /me/messages：全量分页（仅 live 消息）。
func (s *MockGraphServer) handleList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	pageSize := s.pageSize
	base := s.baseURL()
	live := make([]mockGraphMsg, 0, len(s.msgs))
	for _, m := range s.msgs {
		if m.DelSeq == 0 {
			live = append(live, m)
		}
	}
	s.mu.Unlock()

	skip := 0
	if q := r.URL.Query().Get("$skiptoken"); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			skip = n
		}
	}
	end := skip + pageSize
	if end > len(live) {
		end = len(live)
	}
	resp := graphMessagePage{Value: s.wireMessages(live[skip:end])}
	if end < len(live) {
		resp.NextLink = base + "/me/messages?$skiptoken=" + strconv.Itoa(end)
	}
	s.writeJSON(w, resp)
}

// handleDelta GET /me/messages/delta：无 token → 全量基线 + deltaLink；有 token → 增量（added+@removed）。
func (s *MockGraphServer) handleDelta(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	pageSize := s.pageSize
	base := s.baseURL()
	msgs := make([]mockGraphMsg, len(s.msgs))
	copy(msgs, s.msgs)
	seq := s.deltaSeq
	if r.URL.Query().Get("$deltatoken") == "" {
		// 基线调用推进窗口号，供后续增量比较
		s.deltaSeq++
	}
	s.mu.Unlock()

	tok := r.URL.Query().Get("$deltatoken")
	skip := 0
	if st := r.URL.Query().Get("$skiptoken"); st != "" {
		if n, err := strconv.Atoi(st); err == nil {
			skip = n
		}
	}

	var resp graphMessagePage
	if tok == "" {
		// ── 基线：全量 live + deltaLink=w{seq} ──
		live := make([]mockGraphMsg, 0, len(msgs))
		for _, m := range msgs {
			if m.DelSeq == 0 {
				live = append(live, m)
			}
		}
		end := skip + pageSize
		if end > len(live) {
			end = len(live)
		}
		resp.Value = s.wireMessages(live[skip:end])
		if end < len(live) {
			resp.NextLink = base + "/me/messages/delta?$deltatoken=w" + strconv.FormatInt(seq, 10) + "&$skiptoken=" + strconv.Itoa(end)
		} else {
			resp.DeltaLink = base + "/me/messages/delta?$deltatoken=w" + strconv.FormatInt(seq, 10)
		}
	} else {
		// ── 增量：added = Seq>prev 的 live；deleted = DelSeq>prev ──
		prev := int64(0)
		if t := strings.TrimPrefix(tok, "w"); t != "" {
			if n, err := strconv.ParseInt(t, 10, 64); err == nil {
				prev = n
			}
		}
		var added []graphWireMessage
		var deleted []string
		for _, m := range msgs {
			if m.DelSeq > prev {
				deleted = append(deleted, m.ID)
				continue
			}
			if m.Seq > prev && m.DelSeq == 0 {
				added = append(added, s.wire(m))
			}
		}
		// 分页
		page := added
		if skip > 0 {
			if skip < len(page) {
				page = page[skip:]
			} else {
				page = nil
			}
		}
		if len(page) > pageSize {
			resp.Value = page[:pageSize]
			resp.NextLink = base + "/me/messages/delta?$deltatoken=" + tok + "&$skiptoken=" + strconv.Itoa(skip+pageSize)
		} else {
			resp.Value = page
			resp.NextLink = ""
			resp.DeltaLink = base + "/me/messages/delta?$deltatoken=w" + strconv.FormatInt(seq, 10)
		}
		// @removed 条目附于 value 尾部
		for _, id := range deleted {
			resp.Value = append(resp.Value, graphWireMessage{ID: id, Removed: &struct {
				Reason string `json:"reason"`
			}{Reason: "deleted"}})
		}
	}
	s.writeJSON(w, resp)
}

func (s *MockGraphServer) wireMessages(msgs []mockGraphMsg) []graphWireMessage {
	out := make([]graphWireMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, s.wire(m))
	}
	return out
}

func (s *MockGraphServer) wire(m mockGraphMsg) graphWireMessage {
	w := graphWireMessage{
		ID:             m.ID,
		Subject:        m.Subject,
		IsRead:         m.IsRead,
		HasAttachments: m.HasAttach,
		Size:           m.Size,
		BodyPreview:    truncate([]byte(m.Body), 100),
	}
	w.From.EmailAddress.Address = m.From
	w.From.EmailAddress.Name = m.FromName
	w.Body.ContentType = "text"
	w.Body.Content = m.Body
	w.Received = time.UnixMilli(m.ReceivedAt).UTC().Format(time.RFC3339)
	w.ToRecipients = []graphRecipient{{}}
	w.ToRecipients[0].EmailAddress.Address = "mock@example.com"
	return w
}
