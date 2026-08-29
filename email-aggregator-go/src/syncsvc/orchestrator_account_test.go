package syncsvc

import (
	"context"
	"testing"

	"email-aggregator-go/src/events"
	"email-aggregator-go/src/model"
	"email-aggregator-go/src/store"
)

// 假连接器：记录是否被调用，验证账户状态门控。
type gateConnector struct {
	called int
}

func (c *gateConnector) Connect(ctx context.Context, cred model.Credential) error { c.called++; return nil }
func (c *gateConnector) InitialFullSync(ctx context.Context, since int64, sink model.SyncSink) error {
	c.called++
	return nil
}
func (c *gateConnector) IncrementalSync(ctx context.Context, cursor model.SyncCursor, sink model.SyncSink) error {
	c.called++
	return nil
}
func (c *gateConnector) StreamChanges(ctx context.Context, cursor model.SyncCursor, onEvent func(ctx context.Context, m model.CanonicalMail) error) error {
	c.called++
	return nil
}
func (c *gateConnector) FetchMessage(ctx context.Context, id string) (*model.CanonicalMail, error) {
	c.called++
	return nil, nil
}
func (c *gateConnector) ListFolders(ctx context.Context) ([]string, error) { c.called++; return nil, nil }
func (c *gateConnector) Capabilities() model.ConnectorCapabilities {
	return model.ConnectorCapabilities{}
}
func (c *gateConnector) Close() error { return nil }

// 账户状态门控：paused 账户跳过同步（连接器不被调用）、error 账户被拦截。
func TestOrchestrator_AccountStatusGate(t *testing.T) {
	accts := store.NewInMemoryAccountStore()
	if err := accts.CreateAccount("default", model.Account{ID: "acc_paused", Provider: model.ProviderIMAP, Status: model.AccountPaused}); err != nil {
		t.Fatal(err)
	}
	if err := accts.CreateAccount("default", model.Account{ID: "acc_error", Provider: model.ProviderIMAP, Status: model.AccountError}); err != nil {
		t.Fatal(err)
	}

	// 1) paused → 直接返回 nil，连接器零调用
	c1 := &gateConnector{}
	o1 := NewOrchestrator(OrchestratorDeps{Bus: events.NewInMemoryBus(), Connector: c1, Accounts: accts})
	if err := o1.HandleSyncTask(context.Background(), events.SyncTaskEvent{AccountID: "acc_paused", Mode: "initial"}); err != nil {
		t.Fatalf("paused should skip, got err: %v", err)
	}
	if c1.called != 0 {
		t.Fatalf("paused account should not call connector, got %d calls", c1.called)
	}

	// 2) error → 返回错误，连接器零调用
	c2 := &gateConnector{}
	o2 := NewOrchestrator(OrchestratorDeps{Bus: events.NewInMemoryBus(), Connector: c2, Accounts: accts})
	if err := o2.HandleSyncTask(context.Background(), events.SyncTaskEvent{AccountID: "acc_error", Mode: "initial"}); err == nil {
		t.Fatal("error account should be blocked")
	}
	if c2.called != 0 {
		t.Fatalf("error account should not call connector, got %d calls", c2.called)
	}

	// 3) 未挂载账户服务 → 向后兼容放行（连接器被调用）
	c3 := &gateConnector{}
	o3 := NewOrchestrator(OrchestratorDeps{Bus: events.NewInMemoryBus(), Connector: c3})
	if err := o3.HandleSyncTask(context.Background(), events.SyncTaskEvent{AccountID: "acc_unknown", Mode: "initial"}); err != nil {
		t.Fatalf("no account store should still sync, got err: %v", err)
	}
	if c3.called == 0 {
		t.Fatal("connector should be called without account store")
	}

	// 4) 注册表中未注册账户 → 放行（向后兼容）
	c4 := &gateConnector{}
	o4 := NewOrchestrator(OrchestratorDeps{Bus: events.NewInMemoryBus(), Connector: c4, Accounts: accts})
	if err := o4.HandleSyncTask(context.Background(), events.SyncTaskEvent{AccountID: "acc_not_registered", Mode: "initial"}); err != nil {
		t.Fatalf("unregistered account should pass, got err: %v", err)
	}
	if c4.called == 0 {
		t.Fatal("unregistered account should sync")
	}
}
