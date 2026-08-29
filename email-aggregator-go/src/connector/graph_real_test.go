package connector

import (
	"context"
	"sync"
	"testing"
	"time"

	"email-aggregator-go/src/model"
)

// collectSinkGraph 收集 sink（与 EWS 测试共用模式，独立定义避免跨文件耦合）。
type collectSinkGraph struct {
	mu      sync.Mutex
	got     []model.CanonicalMail
	deleted []string
}

func (s *collectSinkGraph) OnMessage(_ context.Context, m model.CanonicalMail) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, m)
	return nil
}

func (s *collectSinkGraph) OnDelete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, id)
	return nil
}

func (s *collectSinkGraph) messages() []model.CanonicalMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.CanonicalMail, len(s.got))
	copy(out, s.got)
	return out
}

func (s *collectSinkGraph) deletes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.deleted))
	copy(out, s.deleted)
	return out
}

// TestGraphConnector_InitialFullSync 端到端：Connect 探测 + 全量分页拉取 + 归一化 + 游标基线。
func TestGraphConnector_InitialFullSync(t *testing.T) {
	srv, stop, err := StartMockGraphAPI()
	if err != nil {
		t.Fatalf("start mock: %v", err)
	}
	defer stop()
	srv.SetPageSize(2) // 强制翻页

	// 注入 3 封
	base := time.Now().UnixMilli()
	srv.Inject("Graph 邮件一", "alice@m365.example.com", "hello graph one", base)
	srv.Inject("Graph 邮件二", "bob@m365.example.com", "hello graph two", base+1)
	srv.Inject("Graph 邮件三", "carol@m365.example.com", "hello graph three", base+2)

	c := NewRealGraphConnector(srv.Addr())
	cred := model.Credential{OAuth: &model.OAuthToken{AccessToken: "test-token"}}
	if err := c.Connect(context.Background(), cred); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if caps := c.Capabilities(); caps.Provider != model.ProviderGraph || !caps.SupportsIncremental || !caps.SupportsOAuth {
		t.Fatalf("capabilities: %+v", caps)
	}

	sink := &collectSinkGraph{}
	if err := c.InitialFullSync(context.Background(), 0, sink); err != nil {
		t.Fatalf("initial full sync: %v", err)
	}

	got := sink.messages()
	if len(got) != 3 {
		t.Fatalf("collected %d, want 3: %+v", len(got), got)
	}
	// 幂等键 = Graph 消息 id，Provider=graph，游标携带 deltaLink
	for _, m := range got {
		if m.Provider != model.ProviderGraph {
			t.Fatalf("provider = %s, want graph", m.Provider)
		}
		if m.From.Email == "" || m.Subject == "" {
			t.Fatalf("mail missing from/subject: %+v", m)
		}
		if m.Cursor.ProviderSpecific["deltaLink"] == "" {
			t.Fatalf("deltaLink cursor missing: %+v", m)
		}
	}
	// 基线 deltaLink 应已记录
	if c.LastDeltaLink() == "" {
		t.Fatal("LastDeltaLink should be set after baseline")
	}
}

// TestGraphConnector_IncrementalSync 端到端：基线后注入+删除 → 增量只回传新增与 @removed 删除。
func TestGraphConnector_IncrementalSync(t *testing.T) {
	srv, stop, err := StartMockGraphAPI()
	if err != nil {
		t.Fatalf("start mock: %v", err)
	}
	defer stop()

	base := time.Now().UnixMilli()
	id1 := srv.Inject("初始一", "a@m365.example.com", "one", base)
	srv.Inject("初始二", "b@m365.example.com", "two", base+1)

	c := NewRealGraphConnector(srv.Addr())
	if err := c.Connect(context.Background(), model.Credential{OAuth: &model.OAuthToken{AccessToken: "test-token"}}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// 基线全量
	sink0 := &collectSinkGraph{}
	if err := c.InitialFullSync(context.Background(), 0, sink0); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if got := len(sink0.messages()); got != 2 {
		t.Fatalf("baseline collected %d, want 2", got)
	}
	baseCursor := sink0.messages()[0].Cursor

	// 注入一封新 + 删除 id1
	srv.Inject("增量三", "c@m365.example.com", "three", base+3)
	srv.Delete(id1)

	// 增量同步
	sink1 := &collectSinkGraph{}
	if err := c.IncrementalSync(context.Background(), baseCursor, sink1); err != nil {
		t.Fatalf("incremental: %v", err)
	}

	msgs := sink1.messages()
	if len(msgs) != 1 || msgs[0].Subject != "增量三" {
		t.Fatalf("incremental added = %+v, want [增量三]", msgs)
	}
	dels := sink1.deletes()
	if len(dels) != 1 || dels[0] != id1 {
		t.Fatalf("incremental deleted = %v, want [%s]", dels, id1)
	}
}

// TestGraphConnector_StreamChangesCancel 端到端：轮询流在 ctx 取消后优雅退出。
func TestGraphConnector_StreamChangesCancel(t *testing.T) {
	srv, stop, err := StartMockGraphAPI()
	if err != nil {
		t.Fatalf("start mock: %v", err)
	}
	defer stop()

	srv.Inject("流一", "a@m365.example.com", "one", time.Now().UnixMilli())
	c := NewRealGraphConnector(srv.Addr())
	c.SetPollInterval(20 * time.Millisecond)
	if err := c.Connect(context.Background(), model.Credential{OAuth: &model.OAuthToken{AccessToken: "test-token"}}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	cursor := model.SyncCursor{ProviderSpecific: map[string]string{"deltaLink": ""}}

	var got int
	var mu sync.Mutex
	err = c.StreamChanges(ctx, cursor, func(_ context.Context, m model.CanonicalMail) error {
		mu.Lock()
		got++
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("stream should exit gracefully on cancel, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got < 1 {
		t.Fatal("stream should have delivered the baseline message before cancel")
	}
}

// TestGraphConnector_ConnectRejectsNoToken：无 token 快速失败。
func TestGraphConnector_ConnectRejectsNoToken(t *testing.T) {
	srv, stop, err := StartMockGraphAPI()
	if err != nil {
		t.Fatalf("start mock: %v", err)
	}
	defer stop()

	c := NewRealGraphConnector(srv.Addr())
	if err := c.Connect(context.Background(), model.Credential{}); err == nil {
		t.Fatal("connect without token should fail")
	}
}
