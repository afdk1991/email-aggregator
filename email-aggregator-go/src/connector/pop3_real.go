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
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"regexp"
	"sort"
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
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	}
	if tlsCfg.ServerName == "" {
		// 显式 SNI / 校验名兜底：与 IMAP 一致，保证自定义 TLS 配置（如封顶版本）下校验正确
		tlsCfg.ServerName = hostOf(addr)
	}
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
		for _, seq := range sortedUIDLKeys(uidls) {
			uidl := uidls[seq]
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
	for _, seq := range sortedUIDLKeys(uidls) {
		uidl := uidls[seq]
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

// sortedUIDLKeys 返回 UIDL 映射的序号，按升序排列——POP3 序号即邮箱内的消息顺序（1..N），
// 保证拉取/回调顺序确定（Go map 迭代顺序本身是随机的，直接 range 会导致顺序不确定）。
func sortedUIDLKeys(uidls map[int]string) []int {
	seqs := make([]int, 0, len(uidls))
	for seq := range uidls {
		seqs = append(seqs, seq)
	}
	sort.Ints(seqs)
	return seqs
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
// id = UIDL（作为幂等键，供摄取层去重）。复用共享的 parseRFC822Message（provider=pop3）。
func parsePOP3Message(raw []byte, account, uidl string, size int64) (model.CanonicalMail, error) {
	return parseRFC822Message(raw, account, uidl, size, model.ProviderPOP3)
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

// pop3Body 提取纯文本正文：multipart 优先首个 text/plain，缺失时回退 text/html 并剥离标签，
// 避免 HTML-only 邮件（新闻简报/营销信）正文为空；charset 非 UTF-8（GBK/GB2312 等）自动转码。
func pop3Body(msg *mail.Message, h mail.Header) string {
	text, html := pop3BodyParts(msg, h)
	if text != "" {
		return text
	}
	if html != "" {
		return htmlToText(html)
	}
	return ""
}

// pop3BodyHTML 提取 HTML 正文（multipart 中的首个 text/html 子部分，未找到返回 ""）。
func pop3BodyHTML(msg *mail.Message, h mail.Header) string {
	_, html := pop3BodyParts(msg, h)
	return html
}

// pop3BodyParts 遍历 multipart 子部分，返回 (text/plain, text/html) 两路正文；
// 非 multipart 按顶层 Content-Type 取整段。子部分按各自 charset 转码。
// 注意：内部先整体读入缓冲，调用方只需调用一次（msg.Body 为一次性 reader）。
func pop3BodyParts(msg *mail.Message, h mail.Header) (string, string) {
	raw, _ := io.ReadAll(msg.Body)
	body := bytes.NewReader(raw)
	ct := h.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		mt = "text/plain"
	}
	text, html := walkBodyParts(body, mt, params, h.Get("Content-Transfer-Encoding"))
	return text, html
}

// walkBodyParts 递归提取正文：对 multipart 递归下降（真实邮件常见
// multipart/mixed > multipart/alternative > (text/plain, text/html) 嵌套），
// 非 multipart 按顶层 Content-Type 取整段。text/plain 优先，text/html 单独保留，
// 两者都按各自 charset 转码 + Content-Transfer-Encoding 解码。
func walkBodyParts(body io.Reader, mt string, params map[string]string, cte string) (string, string) {
	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		var text, html string
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			pt, pparams, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
			pt = strings.ToLower(pt)
			if strings.HasPrefix(pt, "multipart/") {
				// 递归下降进入嵌套 multipart
				t, h2 := walkBodyParts(p, pt, pparams, p.Header.Get("Content-Transfer-Encoding"))
				if text == "" {
					text = t
				}
				if html == "" {
					html = h2
				}
				continue
			}
			b, _ := io.ReadAll(p)
			b = decodeBodyTransfer(b, p.Header.Get("Content-Transfer-Encoding"))
			switch {
			case strings.HasPrefix(pt, "text/plain") && text == "":
				text = charsetToUTF8(pparams["charset"], string(b))
			case strings.HasPrefix(pt, "text/html") && html == "":
				html = charsetToUTF8(pparams["charset"], string(b))
			}
		}
		return text, html
	}
	b, _ := io.ReadAll(body)
	b = decodeBodyTransfer(b, cte)
	s := charsetToUTF8(params["charset"], string(b))
	if strings.HasPrefix(mt, "text/html") {
		return "", s
	}
	return s, ""
}

// decodeBodyTransfer 按 Content-Transfer-Encoding 解码子部分/整段正文：
// quoted-printable（=XX 转义，新闻简报/营销信常见）与 base64 两种；
// 其他/未知编码原样返回。解码失败时回退原始字节，不阻断正文提取。
func decodeBodyTransfer(b []byte, cte string) []byte {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "quoted-printable":
		r := quotedprintable.NewReader(bytes.NewReader(b))
		out, err := io.ReadAll(r)
		if err != nil {
			return b
		}
		return out
	case "base64":
		out, err := base64.StdEncoding.DecodeString(strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, string(b)))
		if err != nil {
			return b
		}
		return out
	default:
		return b
	}
}

// charsetToUTF8 将非 UTF-8 字符集正文转为 UTF-8（复用 rfc2047.go 的 charsetReader：
// utf-8/ascii 原样返回，GBK/GB2312/GB18030 转码）。
func charsetToUTF8(charset, s string) string {
	if s == "" {
		return s
	}
	r, err := charsetReader(charset, strings.NewReader(s))
	if err != nil {
		return s
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return s
	}
	return string(b)
}

// htmlToText 粗略剥离 HTML 标签为可读纯文本（块级换行 + 实体解码 + 空行折叠），
// 供 HTML-only 邮件兜底为 BodyText。
func htmlToText(html string) string {
	s := htmlBlockRE.ReplaceAllString(html, "\n")
	s = htmlTagRE.ReplaceAllString(s, "")
	s = htmlEntityRE.Replace(s)
	s = htmlBlankRE.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

var (
	htmlBlockRE  = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/tr|/li|/table|/h[1-6]|/blockquote)[^>]*>`)
	htmlTagRE    = regexp.MustCompile(`(?s)<[^>]+>`)
	htmlBlankRE  = regexp.MustCompile(`\n\s*\n+`)
	htmlEntityRE = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&apos;", "'")
)

func pop3Snippet(body string) string {
	const max = 200
	if len(body) <= max {
		return body
	}
	return body[:max]
}
