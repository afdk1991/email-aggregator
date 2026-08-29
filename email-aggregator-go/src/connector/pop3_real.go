package connector

// RealPOP3Connector 真实 POP3 适配器（纯标准库实现，零第三方依赖）。
//
// 实现 RFC 1939 子集：USER/PASS 鉴权、STAT/LIST/UIDL 元数据、RETR 全文拉取、
// QUIT 断开（可隐式 TLS）；并将 RFC 822 原始邮件解析为 model.CanonicalMail（net/mail）。
//
// 能力声明与约束：
//   - 增量：POP3 无服务端游标，以 UIDL 高水位（HighWaterMark）近似；新邮件 UIDL 通常单调递增，
//     但顺序并不受协议保证——因此**正确性由摄取层以 UIDL 幂等去重兜底**（架构 §12「幂等事件 + 以元数据为准」）。
//   - 推送：POP3 无 IDLE/推送；StreamChanges 采用轮询（对齐 EWSConnector），默认间隔可注入。
//   - 多文件夹 / 旗帜：不支持（POP3 只有单一 INBOX，无 FLAGS）。
//
// 通过 model.Connector 接口与 sync 编排层（syncsvc.Orchestrator）对接；
// 测试用内嵌 mock POP3 server（StartMockPOP3）端到端验证。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"

	"email-aggregator-go/src/model"
)

// RealPOP3Connector 真实 POP3 连接器（RFC 1939）。
type RealPOP3Connector struct {
	addr   string
	useTLS bool
	tlsCfg *tls.Config

	mu   sync.Mutex // 保护 conn/r（单 goroutine 会话，避免并发读写同一连接）
	conn net.Conn
	r    *bufio.Reader
	cred model.Credential

	pollInterval time.Duration
	cap          model.ConnectorCapabilities
}

// NewRealPOP3Connector 构造真实 POP3 连接器。
// addr 形如 "pop3.example.com:110"；useTLS=true 时建立隐式 TLS 连接（如 995 端口）。
func NewRealPOP3Connector(addr string, useTLS bool, tlsCfg *tls.Config) *RealPOP3Connector {
	return &RealPOP3Connector{
		addr:         addr,
		useTLS:       useTLS,
		tlsCfg:       tlsCfg,
		pollInterval: 30 * time.Second,
		cap: model.ConnectorCapabilities{
			Provider:            model.ProviderPOP3,
			SupportsIncremental: true, // UIDL 高水位近似
			SupportsUIDL:        true,
			SupportsOAuth:       false,
			SupportsIdle:        false,
			SupportsQresync:     false,
			SupportsCondstore:   false,
		},
	}
}

// SetPollInterval 设置 StreamChanges 轮询间隔（测试可注入短值）。
func (c *RealPOP3Connector) SetPollInterval(d time.Duration) { c.pollInterval = d }

// ── model.Connector 接口实现 ───────────────────────────────────────────────

// Connect 建立 TCP（可 TLS）连接并完成 USER/PASS 鉴权。
func (c *RealPOP3Connector) Connect(ctx context.Context, cred model.Credential) error {
	if cred.Username == "" {
		return fmt.Errorf("pop3: username required")
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("pop3 dial %s: %w", c.addr, err)
	}
	if c.useTLS {
		tc := tls.Client(conn, c.tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return fmt.Errorf("pop3 tls handshake: %w", err)
		}
		conn = tc
	}
	c.mu.Lock()
	c.conn = conn
	c.r = bufio.NewReader(conn)
	c.mu.Unlock()

	g, err := c.readLine()
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("pop3 greeting: %w", err)
	}
	if !strings.HasPrefix(g, "+OK") {
		_ = conn.Close()
		return fmt.Errorf("pop3 server greeting: %s", g)
	}
	if err := c.cmdAuth(cred); err != nil {
		_ = conn.Close()
		return err
	}
	c.cred = cred
	return nil
}

// Capabilities 返回能力声明。
func (c *RealPOP3Connector) Capabilities() model.ConnectorCapabilities { return c.cap }

// InitialFullSync 初始全量：UIDL 全列 → 逐封 RETR 拉取并解析，按 since 过滤经 sink 回传。
func (c *RealPOP3Connector) InitialFullSync(ctx context.Context, since int64, sink model.SyncSink) error {
	return c.syncRange(ctx, "", since, sink)
}

// IncrementalSync 增量：以 UIDL 高水位为断点，仅拉取 UIDL 大于水位的邮件。
// 说明：近似游标，正确性由摄取层 UIDL 幂等去重兜底。
func (c *RealPOP3Connector) IncrementalSync(ctx context.Context, cursor model.SyncCursor, sink model.SyncSink) error {
	return c.syncRange(ctx, cursor.HighWaterMark, 0, sink)
}

// StreamChanges 推送（轮询实现，对齐 EWS）：周期 UIDL，发现新 UIDL 即 RETR 并回调，ctx 取消退出。
func (c *RealPOP3Connector) StreamChanges(ctx context.Context, cursor model.SyncCursor, onEvent func(ctx context.Context, m model.CanonicalMail) error) error {
	hw := cursor.HighWaterMark
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		uidls, err := c.cmdUIDL()
		if err != nil {
			return err
		}
		maxHW := hw
		for seq, uidl := range uidls {
			if uidl <= hw {
				continue
			}
			msg, err := c.cmdRetr(seq)
			if err != nil {
				return err
			}
			m, err := parsePOP3Message(msg, c.cred.Username, uidl, int64(len(msg)))
			if err != nil {
				return err
			}
			if uidl > maxHW {
				maxHW = uidl
			}
			if err := onEvent(ctx, m); err != nil {
				return err
			}
		}
		hw = maxHW
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Close 断开连接（QUIT + 关闭 socket）。
func (c *RealPOP3Connector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	_ = c.sendLineLocked("QUIT")
	_, _ = c.readLineLocked()
	err := c.conn.Close()
	c.conn = nil
	c.r = nil
	return err
}

// ── 内部：协议原语 ───────────────────────────────────────────────────────────

func (c *RealPOP3Connector) syncRange(ctx context.Context, highWater string, since int64, sink model.SyncSink) error {
	uidls, err := c.cmdUIDL()
	if err != nil {
		return err
	}
	maxHW := highWater
	for seq, uidl := range uidls {
		if highWater != "" && uidl <= highWater {
			continue
		}
		msg, err := c.cmdRetr(seq)
		if err != nil {
			return err
		}
		m, err := parsePOP3Message(msg, c.cred.Username, uidl, int64(len(msg)))
		if err != nil {
			return err
		}
		if m.InternalDate < since {
			continue
		}
		if uidl > maxHW {
			maxHW = uidl
		}
		m.Cursor = model.SyncCursor{HighWaterMark: uidl}
		if err := sink.OnMessage(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// cmdAuth 执行 USER/PASS 鉴权；服务端 -ERR 视为失败。
func (c *RealPOP3Connector) cmdAuth(cred model.Credential) error {
	if err := c.sendLine("USER " + cred.Username); err != nil {
		return err
	}
	if r, err := c.readLine(); err != nil {
		return err
	} else if !strings.HasPrefix(r, "+OK") {
		return fmt.Errorf("pop3 USER rejected: %s", r)
	}
	if err := c.sendLine("PASS " + cred.Password); err != nil {
		return err
	}
	if r, err := c.readLine(); err != nil {
		return err
	} else if !strings.HasPrefix(r, "+OK") {
		return fmt.Errorf("pop3 PASS rejected: %s", r)
	}
	return nil
}

// cmdUIDL 返回 (序号 → UIDL) 映射。
func (c *RealPOP3Connector) cmdUIDL() (map[int]string, error) {
	if err := c.sendLine("UIDL"); err != nil {
		return nil, err
	}
	lines, err := c.readMultiline()
	if err != nil {
		return nil, err
	}
	out := make(map[int]string, len(lines))
	for _, l := range lines {
		parts := strings.Fields(l)
		if len(parts) >= 2 {
			if n, err := strconv.Atoi(parts[0]); err == nil {
				out[n] = parts[1]
			}
		}
	}
	return out, nil
}

// cmdRetr 按序号拉取一封完整邮件（处理点填充/结束标记）。
func (c *RealPOP3Connector) cmdRetr(seq int) ([]byte, error) {
	if err := c.sendLine(fmt.Sprintf("RETR %d", seq)); err != nil {
		return nil, err
	}
	first, err := c.readLine()
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(first, "+OK") {
		return nil, fmt.Errorf("pop3 RETR %d: %s", seq, first)
	}
	var buf bytes.Buffer
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		if line == "." {
			break
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:] // dot-unstuffing
		}
		buf.WriteString(line)
		buf.WriteString("\r\n")
	}
	return buf.Bytes(), nil
}

// readMultiline 读取多行响应（+OK 首行 + 各行 + "." 结束），跳过 "+OK ..." 头行。
func (c *RealPOP3Connector) readMultiline() ([]string, error) {
	first, err := c.readLine()
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(first, "+OK") {
		return nil, fmt.Errorf("pop3 command failed: %s", first)
	}
	var out []string
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		if line == "." {
			return out, nil
		}
		out = append(out, line)
	}
}

func (c *RealPOP3Connector) sendLine(s string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendLineLocked(s)
}

func (c *RealPOP3Connector) sendLineLocked(s string) error {
	if c.conn == nil {
		return fmt.Errorf("pop3: not connected")
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	_, err := c.conn.Write([]byte(s + "\r\n"))
	return err
}

func (c *RealPOP3Connector) readLine() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readLineLocked()
}

func (c *RealPOP3Connector) readLineLocked() (string, error) {
	if c.r == nil {
		return "", fmt.Errorf("pop3: not connected")
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// ── RFC 822 → CanonicalMail 解析 ─────────────────────────────────────────────

// parsePOP3Message 用 net/mail 解析原始 RFC 822 邮件。
// id = UIDL（作为幂等键，供摄取层去重）。
func parsePOP3Message(raw []byte, account, uidl string, size int64) (model.CanonicalMail, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return model.CanonicalMail{}, fmt.Errorf("pop3 parse mime: %w", err)
	}
	h := msg.Header

	m := model.CanonicalMail{
		ID:          uidl,
		AccountID:   account,
		Provider:    model.ProviderPOP3,
		Folder:      "INBOX",
		SizeBytes:   size,
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

// decodePOP3Header 显式解码 RFC 2047 编码词（net/mail Get 亦会解码，此处幂等兜底）。
func decodePOP3Header(s string) string {
	d := mime.WordDecoder{}
	if out, err := d.DecodeHeader(s); err == nil {
		return out
	}
	return s
}

func pop3Address(a *mail.Address) model.Address {
	return model.Address{Name: a.Name, Email: a.Address}
}

func pop3Addresses(as []*mail.Address) []model.Address {
	out := make([]model.Address, 0, len(as))
	for _, a := range as {
		out = append(out, pop3Address(a))
	}
	return out
}

// pop3Body 提取正文：multipart 取首个 text/plain 子部分，否则取整段 body。
func pop3Body(msg *mail.Message, h mail.Header) string {
	ct := h.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		mt = "text/plain"
	}
	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(msg.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			pt, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
			if strings.HasPrefix(pt, "text/plain") {
				b, _ := io.ReadAll(p)
				return string(b)
			}
		}
		return ""
	}
	b, _ := io.ReadAll(msg.Body)
	return string(b)
}

func pop3Snippet(body string) string {
	const max = 200
	if len(body) <= max {
		return body
	}
	return body[:max]
}
