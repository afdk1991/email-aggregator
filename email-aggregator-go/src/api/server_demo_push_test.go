package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"email-aggregator-go/src/notify"
	"email-aggregator-go/src/search"
	"email-aggregator-go/src/store"
)

// 演示端点门控回归测试。
//
// 背景：/api/demo/push 原先**无条件注册、无鉴权、无 demo 开关** —— 任何请求都能注入一封
// 伪造邮件，写入元数据与检索索引并推送给在线客户端（本用例第二段即证明「确实会写入」）。
//
// 修复后契约：
//   - 默认**不注册**（404）—— 防止生产/集成形态裸奔；
//   - 显式 `WithDemoPush()` 才开放（200）—— 保证本地联调（cmd/server、start-local.ps1）可用。
//
// 调用方以 ENABLE_DEMO 门控，与 EdgeOne 云函数版同名同语义。
func TestDemoPushRequiresExplicitOptIn(t *testing.T) {
	const accountID = "acc_demo"

	newServer := func() (*ApiServer, store.MetadataStore) {
		metadata := store.NewInMemoryMetadataStore()
		idx := search.NewInMemorySearchIndex()
		notifier := notify.NewInMemoryNotifier()
		return NewApiServer(metadata, idx, notifier, 0), metadata
	}

	// ---------- 1. 默认形态：端点不应存在 ----------
	srvOff, _ := newServer()
	recOff := httptest.NewRecorder()
	srvOff.Handler().ServeHTTP(recOff,
		httptest.NewRequest(http.MethodPost, "/api/demo/push?accountId="+accountID, nil))
	if recOff.Code != http.StatusNotFound {
		t.Fatalf("默认不应注册 /api/demo/push：期望 404，实际 %d（body=%s）",
			recOff.Code, recOff.Body.String())
	}

	// ---------- 2. 显式开启：可用，且证明其会写入存储 ----------
	srvOn, mdOn := newServer()
	srvOn = srvOn.WithDemoPush()
	recOn := httptest.NewRecorder()
	srvOn.Handler().ServeHTTP(recOn,
		httptest.NewRequest(http.MethodPost, "/api/demo/push?accountId="+accountID, nil))
	if recOn.Code != http.StatusOK {
		t.Fatalf("WithDemoPush() 后应可访问：期望 200，实际 %d（body=%s）",
			recOn.Code, recOn.Body.String())
	}

	var resp struct {
		TenantID string `json:"tenantId"`
		OK       bool   `json:"ok"`
		ID       string `json:"id"`
		Subject  string `json:"subject"`
	}
	if err := json.Unmarshal(recOn.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应非合法 JSON：%v（body=%s）", err, recOn.Body.String())
	}
	if !resp.OK || resp.ID == "" {
		t.Fatalf("响应缺少 ok/id 字段：%+v", resp)
	}

	// 关键断言：该端点真的往存储里写了一封邮件 —— 这正是它必须被门控的原因。
	n, err := mdOn.Count(resp.TenantID, accountID)
	if err != nil {
		t.Fatalf("Count(%q, %q): %v", resp.TenantID, accountID, err)
	}
	if n != 1 {
		t.Fatalf("演示端点应写入 1 封邮件，实际 count=%d（若为 0，说明写入路径已变更，请复核门控必要性）", n)
	}
}
