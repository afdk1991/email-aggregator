//go:build !integration

// Command server 零依赖常驻 REST 服务，供前端（email-aggregator-web）联调。
// 使用内存存储预加载 demo 数据，暴露 /api/health、/api/mails、/api/search。
// 与 cmd/demo.go（一次性场景演示）互补：demo 跑通核心契约，server 提供常驻 API。
//
// 运行：go run ./cmd/server   （默认监听 :8080，可用 HTTP_PORT 覆盖）
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"email-aggregator-go/src/aigateway"
	"email-aggregator-go/src/api"
	"email-aggregator-go/src/model"
	"email-aggregator-go/src/notify"
	"email-aggregator-go/src/search"
	"email-aggregator-go/src/store"
	"email-aggregator-go/src/tenant"
)

// envPort 读取端口型环境变量；为空或非法时回退默认值，避免启动因配置笔误而失败。
func envPort(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 65535 {
		log.Printf("[warn] %s=%q 非法，回退默认端口 %d", key, v, def)
		return def
	}
	return n
}

func main() {
	metadata := store.NewInMemoryMetadataStore()
	index := search.NewInMemorySearchIndex()
	notifier := notify.NewInMemoryNotifier()

	// 预加载 demo 邮件（与 cmd/demo.go 主题一致，便于联调核对）。
	// 多账户种子：acc_demo / acc_work / acc_personal，账户按 AccountID 隔离，
	// 让前端账户切换 chips 真正可用（ListMails/Search/WS 本就按账户隔离）。
	demoMails := []model.CanonicalMail{
		{
			ID: "m1", AccountID: "acc_demo", Folder: "INBOX", Provider: model.ProviderIMAP,
			From:          model.Address{Email: "alice@example.com"},
			Subject:       "Welcome onboard",
			BodyText:      "Welcome to the platform, your account is ready.",
			InternalDate:  1700000000,
			SizeBytes:     1024,
			Cursor:        model.SyncCursor{LastUID: 1},
		},
		{
			ID: "m2", AccountID: "acc_demo", Folder: "INBOX", Provider: model.ProviderIMAP,
			From:          model.Address{Email: "bob@vendor.com"},
			Subject:       "Invoice #2024-03",
			BodyText:      "Please find attached the invoice for March services.",
			InternalDate:  1700000100,
			SizeBytes:     2048,
			HasAttachment: true,
			Cursor:        model.SyncCursor{LastUID: 2},
		},
		{
			ID: "m3", AccountID: "acc_demo", Folder: "INBOX", Provider: model.ProviderIMAP,
			From:          model.Address{Email: "carol@meet.com"},
			Subject:       "Team sync meeting",
			BodyText:      "Reminder: weekly team sync at 10am tomorrow.",
			InternalDate:  1700000200,
			SizeBytes:     1536,
			Cursor:        model.SyncCursor{LastUID: 3},
		},
		// acc_work（Gmail 工作邮箱）
		{
			ID: "w1", AccountID: "acc_work", Folder: "INBOX", Provider: model.ProviderGmail,
			From:          model.Address{Email: "pm@corp.com", Name: "项目 PM"},
			Subject:       "Sprint 复盘纪要",
			BodyText:      "本周 Sprint 已收尾，复盘结论：接口契约对齐完成，下周进入联调。",
			InternalDate:  1700001000,
			SizeBytes:     1800,
			Cursor:        model.SyncCursor{LastUID: 1},
		},
		{
			ID: "w2", AccountID: "acc_work", Folder: "INBOX", Provider: model.ProviderGmail,
			From:          model.Address{Email: "legal@corp.com"},
			Subject:       "客户 A 合同待签署",
			BodyText:      "客户 A 的年度框架合同已生成，请在周五前完成电子签署。",
			InternalDate:  1700001100,
			SizeBytes:     2200,
			HasAttachment: true,
			Cursor:        model.SyncCursor{LastUID: 2},
		},
		// acc_personal（IMAP 个人邮箱）
		{
			ID: "p1", AccountID: "acc_personal", Folder: "INBOX", Provider: model.ProviderIMAP,
			From:          model.Address{Email: "family@home.com", Name: "家人"},
			Subject:       "周末家庭聚餐邀约",
			BodyText:      "这周六老家聚餐，记得提前安排时间，爸妈准备了你爱吃的菜。",
			InternalDate:  1700002000,
			SizeBytes:     900,
			Cursor:        model.SyncCursor{LastUID: 1},
		},
		{
			ID: "p2", AccountID: "acc_personal", Folder: "INBOX", Provider: model.ProviderIMAP,
			From:          model.Address{Email: "billing@utility.com"},
			Subject:       "水电费账单已出",
			BodyText:      "本月水电气账单已生成，合计 ¥328.50，请于月底前缴费。",
			InternalDate:  1700002100,
			SizeBytes:     760,
			Cursor:        model.SyncCursor{LastUID: 2},
		},
	}
	for _, m := range demoMails {
		tid := tenant.DefaultTenantID
		m.TenantID = tid
		if err := metadata.UpsertMail(tid, m); err != nil {
			log.Fatalf("upsert mail: %v", err)
		}
		if err := index.Index(tid, m); err != nil {
			log.Fatalf("index mail: %v", err)
		}
	}

	// 监听端口：默认 8080，可用环境变量 HTTP_PORT 覆盖（本机多项目并存时避免端口冲突）。
	srv := api.NewApiServer(metadata, index, notifier, envPort("HTTP_PORT", 8080))

	// 挂载 WebSocket 实时推送 Hub（零依赖 RFC6455）：让前端通过 /ws 接收 new-mail 实时通知。
	hub := notify.NewHub(notifier)
	srv.WithWSHub(hub)

	// 演示用：每 30s 若前端已建立 WS 连接，自动生成一封新邮件并实时推送，
	// 让实时能力持续可见（生产由真实同步流水线驱动）。按"有 WS 连接的活跃账户"定向发布，
	// 使多账户场景下实时推送与当前查看的账户一致。
	go func() {
		samples := []string{
			"季度财报已生成", "新的客户工单 #%d", "系统维护通知", "您有一笔待审批报销", "周报提醒：请于周五前提交",
		}
		var i int
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			active := hub.ActiveAccounts()
			if len(active) == 0 {
				continue
			}
			i++
			for _, accountID := range active {
				subject := fmt.Sprintf(samples[i%len(samples)], i)
				id := fmt.Sprintf("auto-%d", time.Now().UnixNano())
			mail := model.CanonicalMail{
				TenantID:     tenant.DefaultTenantID,
				ID:           id,
				AccountID:    accountID,
				Provider:     model.ProviderIMAP,
				Folder:       "INBOX",
				From:         model.Address{Email: "noreply@system.com", Name: "系统通知"},
				Subject:      subject,
				BodyText:     "这是一封由后台定时器自动生成的演示邮件，用于持续展示 WebSocket 实时推送能力。",
				InternalDate: time.Now().Unix(),
				SizeBytes:    96,
				Cursor:       model.SyncCursor{LastUID: uint32(time.Now().Unix())},
			}
			if err := metadata.UpsertMail(mail.TenantID, mail); err != nil {
				log.Printf("auto mail upsert: %v", err)
				continue
			}
			if err := index.Index(mail.TenantID, mail); err != nil {
				log.Printf("auto mail index: %v", err)
			}
			notifier.Publish(mail.TenantID, accountID, notify.NotificationPayload{
				Kind:      notify.KindNewMail,
				AccountID: accountID,
				Preview:   subject,
				TS:        time.Now().Unix(),
			})
			}
		}
	}()

	// 挂载 AI 能力路由网关（ADR-010）：默认有 AI_SELF_HOSTED_URL / AI_THIRD_PARTY_URL 则真连
	// OpenAI 兼容后端（HTTPProvider），否则降级本地 demo 回环（LocalDemoProvider，[demo] 标注非伪造），
	// 使前端 AI 面板开箱可用且网关全链路（路由/脱敏/审计）可端到端验证，不再使用占位 StubProvider。
	selfHosted, thirdParty := aigateway.ProvidersFromEnv(os.Getenv)
	router := aigateway.NewRouter(selfHosted, thirdParty, aigateway.NewRegexRedactor(),
		func(_ context.Context, ev aigateway.AuditEvent) {
			log.Printf("ai-audit: tenant=%s backend=%s allowed=%v redacted=%v reason=%q",
				ev.TenantID, ev.Backend, ev.Allowed, ev.Redacted, ev.Reason)
		},
	)
	srv.WithAIGateway(router)

	log.Printf("email-aggregator server listening on %s", srv.Addr())
	if err := http.ListenAndServe(srv.Addr(), srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
