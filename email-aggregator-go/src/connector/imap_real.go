package connector

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"email-aggregator-go/src/model"
)

// RealIMAPConnector 真实 IMAP 适配器（纯标准库实现，零第三方依赖）。
//
// 完整实现：CAPABILITY / STARTTLS / LOGIN（明文与 AUTH=PLAIN/XOAUTH2）/
// SELECT / UID FETCH（ENVELOPE BODYSTRUCTURE RFC822.SIZE INTERNALDATE UID FLAGS）/
// IDLE 长连接推送；并将 FETCH 响应解析为 model.CanonicalMail
// （支持字面量 {N}、引号转义、嵌套括号的 ENVELOPE 地址组、BODYSTRUCTURE 附件探测）。
//
// 通过 model.Connector 接口与 sync 编排层（syncsvc.Orchestrator）直接对接，
// 生产只需注入真实邮箱地址 + 凭据即可；测试用内嵌 mock IMAP server 端到端验证。
type RealIMAPConnector struct {
	addr    string
	useTLS  bool
	tlsCfg  *tls.Config
	timeout time.Duration

	conn    net.Conn
	br      *bufio.Reader
	bw      *bufio.Writer
	tagSeq  int
	lastUID uint32
	cap     model.ConnectorCapabilities
	user    string
}

// NewRealIMAPConnector 构造真实 IMAP 连接器。
// addr 形如 "imap.example.com:993"；useTLS=true 时建立隐式 TLS 连接（如 993 端口）；
// useTLS=false 时按 CAPABILITY 自动 STARTTLS 升级（如 143 端口）。
func NewRealIMAPConnector(addr string, useTLS bool, tlsCfg *tls.Config) *RealIMAPConnector {
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	}
	if tlsCfg.ServerName == "" {
		// 显式 SNI / 校验名兜底：即使调用方传入自定义 TLS 配置（如封顶版本）也保证校验正确
		tlsCfg.ServerName = hostOf(addr)
	}
	return &RealIMAPConnector{
		addr:    addr,
		useTLS:  useTLS,
		tlsCfg:  tlsCfg,
		timeout: 30 * time.Second,
		cap: model.ConnectorCapabilities{
			Provider:            model.ProviderIMAP,
			SupportsIncremental: true,
			SupportsIdle:        true,
			SupportsOAuth:       true,
		},
	}
}

func hostOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[:i]
	}
	return addr
}

// ── model.Connector 接口实现 ───────────────────────────────────────────────

// Connect 建立连接并完成鉴权。
func (c *RealIMAPConnector) Connect(ctx context.Context, cred model.Credential) error {
	dialer := &net.Dialer{Timeout: c.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("imap dial %s: %w", c.addr, err)
	}
	if c.useTLS {
		tlsConn := tls.Client(conn, c.tlsCfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return fmt.Errorf("imap tls handshake: %w", err)
		}
		conn = tlsConn
	}
	c.conn = conn
	c.br = bufio.NewReader(conn)
	c.bw = bufio.NewWriter(conn)

	// 读取服务端 greeting（untagged * OK）
	if _, err := c.readLine(); err != nil {
		return fmt.Errorf("imap greeting: %w", err)
	}

	caps, err := c.capability()
	if err != nil {
		return err
	}
	c.cap.SupportsIdle = strings.Contains(caps, "IDLE")

	// STARTTLS 升级（未走隐式 TLS 且服务端支持时）
	if !c.useTLS && strings.Contains(caps, "STARTTLS") {
		if err := c.startTLS(); err != nil {
			return err
		}
	}

	// 鉴权：OAuth2 优先，否则明文/应用密码
	switch {
	case cred.OAuth != nil && cred.OAuth.AccessToken != "":
		if err := c.authXOAuth2(cred.Username, cred.OAuth.AccessToken); err != nil {
			return err
		}
	default:
		if err := c.login(cred.Username, cred.Password); err != nil {
			return err
		}
	}
	c.user = cred.Username
	return nil
}

// Capabilities 返回能力声明
func (c *RealIMAPConnector) Capabilities() model.ConnectorCapabilities { return c.cap }

// InitialFullSync 初始全量：SELECT INBOX 后按序列号分批拉取（每批 50）。
// 规避 139 等服务器对单命令大响应（ENVELOPE+BODYSTRUCTURE）截断在 ~64KB/99 条的问题——
// 单次 `UID FETCH 1:*` 会被静默截断，导致只同步到前 99 封、丢失其余全部邮件。
func (c *RealIMAPConnector) InitialFullSync(ctx context.Context, since int64, sink model.SyncSink) error {
	_, exists, err := c.selectMailbox("INBOX")
	if err != nil {
		return err
	}
	if exists <= 0 {
		return nil
	}
	const batch = 50
	for from := 1; from <= exists; from += batch {
		to := from + batch - 1
		if to > exists {
			to = exists
		}
		if err := c.fetchSeqAndSink(ctx, from, to, since, sink); err != nil {
			return err
		}
	}
	return nil
}

// fetchSeqAndSink 按序列号区间拉取（FETCH from:to，响应含 UID）并过滤 since 后经 sink 回传。
func (c *RealIMAPConnector) fetchSeqAndSink(ctx context.Context, from, to int, since int64, sink model.SyncSink) error {
	tag := c.nextTag()
	resp, err := c.roundtrip(tag, fmt.Sprintf("FETCH %d:%d (%s)", from, to, defaultFetchItems))
	if err != nil {
		return err
	}
	mails, err := parseFetchResponses(resp)
	if err != nil {
		return err
	}
	c.finishMails(mails)
	c.fetchBodyTexts(mails)
	for _, m := range mails {
		if since > 0 && m.InternalDate < since {
			continue
		}
		if err := sink.OnMessage(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// IncrementalSync 增量：从 cursor 断点（LastUID+1）继续拉取。
func (c *RealIMAPConnector) IncrementalSync(ctx context.Context, cursor model.SyncCursor, sink model.SyncSink) error {
	if _, _, err := c.selectMailbox("INBOX"); err != nil {
		return err
	}
	set := "1:*"
	if cursor.LastUID > 0 {
		set = fmt.Sprintf("%d:*", cursor.LastUID+1)
	}
	return c.fetchAndSink(ctx, set, 0, sink)
}

// StreamChanges 长连接推送（IMAP IDLE）：有变更（EXISTS）时退出 IDLE 拉取新邮件并回调，再重入 IDLE。
// 单 goroutine + 读超时实现，避免并发读同一连接（数据竞争）。
func (c *RealIMAPConnector) StreamChanges(ctx context.Context, _ model.SyncCursor, onEvent func(ctx context.Context, m model.CanonicalMail) error) error {
	tag := c.nextTag()
	if err := c.writeLine(tag + " IDLE"); err != nil {
		return err
	}
	if line, _ := c.readLine(); !strings.HasPrefix(line, "+") {
		return nil // 服务端不支持 IDLE，静默返回
	}
	const idlePoll = 200 * time.Millisecond
	for {
		if c.conn != nil {
			_ = c.conn.SetReadDeadline(time.Now().Add(idlePoll))
		}
		line, err := c.readLine()
		if err != nil {
			var ne net.Error
			if ok := asNetError(err, &ne); ok && ne.Timeout() {
				select {
				case <-ctx.Done():
					c.writeLine("DONE")
					c.readLine() // 消费 OK IDLE terminated
					return nil
				default:
					continue // 超时但未取消，继续等 EXISTS
				}
			}
			return err
		}
		if strings.Contains(line, "EXISTS") {
			c.writeLine("DONE")
			c.readLine() // 消费 OK
			mails, ferr := c.uidFetch(fmt.Sprintf("%d:*", c.lastUID+1))
			if ferr == nil {
				for _, m := range mails {
					_ = onEvent(ctx, m)
				}
			}
			// 重入 IDLE
			if err := c.writeLine(tag + " IDLE"); err != nil {
				return err
			}
			if line, _ := c.readLine(); !strings.HasPrefix(line, "+") {
				return nil
			}
		}
	}
}

// asNetError 类型断言 err 为 net.Error（兼容 bufio 包装的超时错误）。
func asNetError(err error, target *net.Error) bool {
	if ne, ok := err.(net.Error); ok {
		*target = ne
		return true
	}
	return false
}

// Close 释放连接
func (c *RealIMAPConnector) Close() error {
	if c.conn == nil {
		return nil
	}
	_, _ = c.roundtrip(c.nextTag(), "LOGOUT")
	return c.conn.Close()
}

// ── 内部：协议原语 ───────────────────────────────────────────────────────────

const defaultFetchItems = "ENVELOPE BODYSTRUCTURE RFC822.SIZE INTERNALDATE UID FLAGS"

func (c *RealIMAPConnector) nextTag() string {
	c.tagSeq++
	return fmt.Sprintf("A%04d", c.tagSeq)
}

func (c *RealIMAPConnector) writeLine(s string) error {
	if _, err := c.bw.WriteString(s + "\r\n"); err != nil {
		return err
	}
	return c.bw.Flush()
}

// readLine 读取一行，并处理 IMAP 字面量（{N} 标记后紧跟 N 字节 + CRLF）。
func (c *RealIMAPConnector) readLine() (string, error) {
	line, err := c.br.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if n, ok := trailingLiteral(line); ok {
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.br, buf); err != nil {
			return "", err
		}
		// 消费字面量后的 CRLF
		_, _ = c.br.ReadString('\n')
		line = line + string(buf)
	}
	return line, nil
}

// trailingLiteral 检测行尾的 {N} 字面量标记，返回长度。
func trailingLiteral(line string) (int, bool) {
	line = strings.TrimRight(line, " \r\n")
	i := strings.LastIndex(line, "{")
	if i < 0 {
		return 0, false
	}
	j := strings.IndexByte(line[i:], '}')
	if j < 0 {
		return 0, false
	}
	if i+j+1 != len(line) {
		return 0, false
	}
	n, err := strconv.Atoi(line[i+1 : i+j])
	if err != nil {
		return 0, false
	}
	return n, true
}

// roundtrip 发送命令并读取直到带 tag 的终结行；返回其间所有 untagged 响应文本。
func (c *RealIMAPConnector) roundtrip(tag, cmd string) (string, error) {
	if err := c.writeLine(tag + " " + cmd); err != nil {
		return "", err
	}
	var sb strings.Builder
	for {
		line, err := c.readLine()
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(line, tag+" ") {
			fields := strings.SplitN(line[len(tag)+1:], " ", 2)
			if strings.EqualFold(fields[0], "OK") {
				return sb.String(), nil
			}
			return sb.String(), fmt.Errorf("imap command failed: %s", line)
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}
}

func (c *RealIMAPConnector) capability() (string, error) {
	tag := c.nextTag()
	resp, err := c.roundtrip(tag, "CAPABILITY")
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(resp, "\n") {
		if strings.Contains(l, "CAPABILITY") {
			return l, nil
		}
	}
	return "", nil
}

func (c *RealIMAPConnector) startTLS() error {
	tag := c.nextTag()
	if _, err := c.roundtrip(tag, "STARTTLS"); err != nil {
		return err
	}
	tlsConn := tls.Client(c.conn, c.tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		return err
	}
	c.conn = tlsConn
	c.br = bufio.NewReader(c.conn)
	c.bw = bufio.NewWriter(c.conn)
	return nil
}

func (c *RealIMAPConnector) login(user, pass string) error {
	tag := c.nextTag()
	cmd := fmt.Sprintf("LOGIN %s %s", quoteIMAP(user), quoteIMAP(pass))
	_, err := c.roundtrip(tag, cmd)
	return err
}

func (c *RealIMAPConnector) authPlain(user, pass string) error {
	tag := c.nextTag()
	raw := "\x00" + user + "\x00" + pass
	b64 := base64.StdEncoding.EncodeToString([]byte(raw))
	_, err := c.roundtrip(tag, "AUTHENTICATE PLAIN "+b64)
	return err
}

func (c *RealIMAPConnector) authXOAuth2(user, token string) error {
	tag := c.nextTag()
	raw := "user=" + user + "\x01auth=Bearer " + token + "\x01\x01"
	b64 := base64.StdEncoding.EncodeToString([]byte(raw))
	_, err := c.roundtrip(tag, "AUTHENTICATE XOAUTH2 "+b64)
	return err
}

func (c *RealIMAPConnector) selectMailbox(mb string) (uidValidity uint32, exists int, err error) {
	tag := c.nextTag()
	resp, err := c.roundtrip(tag, "SELECT "+quoteIMAP(mb))
	if err != nil {
		return 0, 0, err
	}
	for _, l := range strings.Split(resp, "\n") {
		if strings.Contains(l, "EXISTS") {
			// 形如 "* 2411 EXISTS"：取 EXISTS 前一个字段。
			if f := strings.Fields(l); len(f) >= 2 && f[len(f)-1] == "EXISTS" {
				if n, err := strconv.Atoi(f[len(f)-2]); err == nil {
					exists = n
				}
			}
		}
		if i := strings.Index(l, "UIDVALIDITY"); i >= 0 {
			fmt.Sscanf(l[i:], "UIDVALIDITY %d", &uidValidity)
		}
	}
	return uidValidity, exists, nil
}

func (c *RealIMAPConnector) fetchAndSink(ctx context.Context, set string, since int64, sink model.SyncSink) error {
	mails, err := c.uidFetch(set)
	if err != nil {
		return err
	}
	for _, m := range mails {
		if since > 0 && m.InternalDate < since {
			continue
		}
		if err := sink.OnMessage(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// fetchBodyTexts 为元数据已就绪的邮件批量回填正文：逐 UID 拉 BODY.PEEK[] 全文，
// 经 RFC822 解析取 text/plain（HTML-only 兜底）写入 BodyText/BodyHTML。
// 单批 1 封以规避 139 等服务器对单命令大响应的截断（~64KB）；新增邮件量小，成本可忽略。
func (c *RealIMAPConnector) fetchBodyTexts(mails []model.CanonicalMail) {
	for i := range mails {
		raw, err := c.fetchBodyByUID(mails[i].ID)
		if err != nil || len(raw) == 0 {
			continue // 单封失败降级：保留元数据，正文留空
		}
		pm, err := parseRFC822Message(raw, mails[i].AccountID, mails[i].ID, mails[i].SizeBytes, model.ProviderIMAP)
		if err != nil {
			continue
		}
		mails[i].BodyText = pm.BodyText
		mails[i].BodyHTML = pm.BodyHTML
	}
}

// fetchBodyByUID 单封拉取完整原始邮件（BODY.PEEK[]，不产生 \Seen 标记副作用）。
// 139/163 等国产服务器对 BODY 拉取存在间歇性节流：限流窗口内对同一命令返回
// “tag OK Fetch completed”但不带字面量（实测 5 连败后冷却 ~30s 单次即成功）。
// 因此空结果不立即放弃，按 imapBodyRetryBackoffs 退避重试至多 2 次。
var imapBodyRetryBackoffs = []time.Duration{0, 300 * time.Millisecond, 1200 * time.Millisecond}

func (c *RealIMAPConnector) fetchBodyByUID(uid string) ([]byte, error) {
	var lastErr error
	for attempt, d := range imapBodyRetryBackoffs {
		if d > 0 {
			time.Sleep(d)
		}
		raw, err := c.fetchBodyOnce("UID FETCH " + uid + " (BODY.PEEK[])")
		if err != nil {
			lastErr = err
			continue
		}
		if len(raw) > 0 {
			return raw, nil
		}
		if attempt == len(imapBodyRetryBackoffs)-1 {
			return nil, nil
		}
	}
	return nil, lastErr
}

// fetchBodyOnce 执行一次 BODY 抓取命令，返回字面量正文；OK 但无字面量返回空切片。
func (c *RealIMAPConnector) fetchBodyOnce(cmd string) ([]byte, error) {
	tag := c.nextTag()
	if err := c.writeLine(tag + " " + cmd); err != nil {
		return nil, err
	}
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(line, tag) {
			if strings.Contains(line, " OK ") || strings.HasSuffix(line, "OK") {
				return nil, nil // 未取到正文（限流或邮件已被删除）
			}
			return nil, fmt.Errorf("imap body fetch: %s", line)
		}
		if raw, ok := bodyLiteralFromLine(line); ok {
			return raw, nil
		}
	}
}

var bodyLiteralRE = regexp.MustCompile(`\{(\d+)\}`)

// bodyLiteralFromLine 从 readLine 返回的行中提取 BODY[] 字面量。
// readLine 已把 {N} 后的 N 字节内联到行尾（首段的 \r\n 被 TrimRight，故此处只匹配 {N}）。
func bodyLiteralFromLine(line string) ([]byte, bool) {
	loc := bodyLiteralRE.FindStringIndex(line)
	if loc == nil {
		return nil, false
	}
	n, _ := strconv.Atoi(line[loc[0]+1 : loc[1]-1])
	after := line[loc[1]:]
	if len(after) < n {
		return nil, false
	}
	return []byte(after[:n]), true
}

func (c *RealIMAPConnector) uidFetch(set string) ([]model.CanonicalMail, error) {
	tag := c.nextTag()
	resp, err := c.roundtrip(tag, "UID FETCH "+set+" ("+defaultFetchItems+")")
	if err != nil {
		return nil, err
	}
	mails, err := parseFetchResponses(resp)
	if err != nil {
		return nil, err
	}
	c.finishMails(mails)
	c.fetchBodyTexts(mails)
	return mails, nil
}

// finishMails 统一后处理：补 Provider/Folder/AccountID/ID，并推进游标 lastUID。
func (c *RealIMAPConnector) finishMails(mails []model.CanonicalMail) {
	for i := range mails {
		mails[i].Provider = model.ProviderIMAP
		mails[i].Folder = "INBOX"
		mails[i].AccountID = c.user
		if mails[i].ID == "" {
			mails[i].ID = fmt.Sprintf("%s:%d", mails[i].AccountID, mails[i].Cursor.LastUID)
		}
		if mails[i].Cursor.LastUID > c.lastUID {
			c.lastUID = mails[i].Cursor.LastUID
		}
	}
}

// quoteIMAP 按 IMAP 规范引用字符串（双引号 + 转义 " 与 \）。
func quoteIMAP(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// ── FETCH 响应解析 ───────────────────────────────────────────────────────────

// parseFetchResponses 从 roundtrip 的 untagged 文本中提取所有 * N FETCH (...) 并解析。
// 注意 FETCH 响应可能跨多行（字面量 {N} 后换行、BODYSTRUCTURE 折行），故在整段文本中定位
// "FETCH (" 并跨行取平衡括号，而非按行切分。
func parseFetchResponses(raw string) ([]model.CanonicalMail, error) {
	var out []model.CanonicalMail
	i := 0
	for i < len(raw) {
		idx := strings.Index(raw[i:], "FETCH")
		if idx < 0 {
			break
		}
		abs := i + idx
		open := strings.Index(raw[abs:], "(")
		if open < 0 {
			break
		}
		start := abs + open
		sub, ok := balancedSubstring(raw, start)
		if !ok {
			break
		}
		m, err := parseFetchList(sub)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
		i = start + len(sub)
	}
	return out, nil
}

// balancedSubstring 从 s[openIdx]（应为 '('）起截取匹配的括号对内容（含两端括号）。
func balancedSubstring(s string, openIdx int) (string, bool) {
	if openIdx >= len(s) || s[openIdx] != '(' {
		return "", false
	}
	depth := 0
	for i := openIdx; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[openIdx : i+1], true
			}
		}
	}
	return "", false
}

// parseFetchList 解析一个 FETCH 列表（含括号），提取关键字段到 CanonicalMail。
func parseFetchList(s string) (model.CanonicalMail, error) {
	toks, err := tokenize(s)
	if err != nil {
		return model.CanonicalMail{}, err
	}
	root, err := buildTree(toks)
	if err != nil {
		return model.CanonicalMail{}, err
	}
	var m model.CanonicalMail
	for i := 0; i < len(root.list); i++ {
		n := root.list[i]
		if !n.isAtom() {
			continue
		}
		switch n.atom {
		case "UID":
			if i+1 < len(root.list) {
				m.ID = root.list[i+1].atom
				m.Cursor.LastUID = uint32(atoi64(root.list[i+1].atom))
			}
		case "RFC822.SIZE":
			if i+1 < len(root.list) {
				m.SizeBytes = atoi64(root.list[i+1].atom)
			}
		case "INTERNALDATE":
			if i+1 < len(root.list) {
				m.InternalDate = parseIMAPDate(root.list[i+1].atom)
			}
		case "FLAGS":
			if i+1 < len(root.list) && root.list[i+1].isList {
				m.Read = listContains(root.list[i+1].list, "\\SEEN")
			}
		case "ENVELOPE":
			if i+1 < len(root.list) && root.list[i+1].isList {
				parseEnvelope(root.list[i+1].list, &m)
			}
		case "BODYSTRUCTURE", "BODY":
			if i+1 < len(root.list) && root.list[i+1].isList {
				if hasAttachment(root.list[i+1].list) {
					m.HasAttachment = true
				}
			}
		}
	}
	return m, nil
}

func (n node) isAtom() bool { return !n.isList }

// parseEnvelope 解析 ENVELOPE 列表：(date subject from to cc bcc reply-to in-reply-to message-id)
func parseEnvelope(l []node, m *model.CanonicalMail) {
	if len(l) < 9 {
		return
	}
	m.InternalDate = parseIMAPDate(strOf(l[0]))
	m.Subject = decodeRFC2047(strOf(l[1]))
	if addrs := parseAddresses(l[2]); len(addrs) > 0 {
		m.From = addrs[0]
	}
	m.To = parseAddresses(l[3])
	m.Cc = parseAddresses(l[4])
}

// parseAddresses 解析地址组列表（每个元素是一组地址，地址为 (name route mailbox host)）。
func parseAddresses(n node) []model.Address {
	var out []model.Address
	if !n.isList {
		return out
	}
	for _, child := range n.list {
		if !child.isList {
			continue
		}
		a := parseOneAddress(child)
		if a.Email != "" || a.Name != "" {
			out = append(out, a)
		}
	}
	return out
}

func parseOneAddress(n node) model.Address {
	if len(n.list) < 4 {
		return model.Address{}
	}
	email := strOf(n.list[2])
	if host := strOf(n.list[3]); host != "" {
		email = email + "@" + host
	}
	return model.Address{Name: decodeRFC2047(strOf(n.list[0])), Email: email}
}

// hasAttachment 递归扫描 BODYSTRUCTURE，发现 "attachment" 处置或 "NAME" 参数即判为带附件。
func hasAttachment(l []node) bool {
	for _, n := range l {
		if n.isAtom() && (strings.EqualFold(n.atom, "ATTACHMENT") || strings.EqualFold(n.atom, "NAME")) {
			return true
		}
		if n.isList && hasAttachment(n.list) {
			return true
		}
	}
	return false
}

func listContains(l []node, target string) bool {
	for _, n := range l {
		if n.isAtom() && n.atom == target {
			return true
		}
	}
	return false
}

func strOf(n node) string { return n.atom }

func atoi64(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// parseIMAPDate 解析 IMAP 日期（INTERNALDATE 与 ENVELOPE 两种布局）。
func parseIMAPDate(s string) int64 {
	s = strings.Trim(s, "\"")
	layouts := []string{
		"02-Jan-2006 15:04:05 -0700",
		"Mon, 02 Jan 2006 15:04:05 -0700",
		time.RFC1123Z,
		time.RFC3339,
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// ── 词法 + 语法（括号树）解析器 ──────────────────────────────────────────────

type tokenKind int

const (
	tLParen tokenKind = iota
	tRParen
	tAtom
)

type tok struct {
	kind tokenKind
	atom string // 引号内容 / 大写原子 / "NIL"
}

// tokenize 将 FETCH 列表字符串切分为 token；跳过 {N} 字面量长度标记（其内容已内联）。
func tokenize(s string) ([]tok, error) {
	var toks []tok
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\r' || c == '\n' || c == '\t':
			i++
		case c == '(':
			toks = append(toks, tok{kind: tLParen})
			i++
		case c == ')':
			toks = append(toks, tok{kind: tRParen})
			i++
		case c == '"':
			j := i + 1
			var b strings.Builder
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) {
					b.WriteByte(s[j+1])
					j += 2
					continue
				}
				if s[j] == '"' {
					break
				}
				b.WriteByte(s[j])
				j++
			}
			toks = append(toks, tok{kind: tAtom, atom: b.String()})
			i = j + 1
		case c == '{':
			// 字面量长度标记 {N}：已内联内容，跳过
			j := i + 1
			for j < len(s) && s[j] != '}' {
				j++
			}
			i = j + 1
		default:
			j := i
			for j < len(s) && s[j] != ' ' && s[j] != '(' && s[j] != ')' && s[j] != '\r' && s[j] != '\n' && s[j] != '\t' {
				j++
			}
			atom := strings.ToUpper(s[i:j])
			if atom == "NIL" {
				atom = ""
			}
			toks = append(toks, tok{kind: tAtom, atom: atom})
			i = j
		}
	}
	return toks, nil
}

// node 括号树节点：原子（atom）或列表（list）。
type node struct {
	isList bool
	atom   string
	list   []node
}

// buildTree 由 token 序列构建括号树。
func buildTree(toks []tok) (node, error) {
	root, _, err := buildFrom(toks, 0)
	return root, err
}

func buildFrom(toks []tok, i int) (node, int, error) {
	if i >= len(toks) || toks[i].kind != tLParen {
		return node{}, i, fmt.Errorf("expected '(' at %d", i)
	}
	i++
	var n node
	n.isList = true
	for i < len(toks) {
		t := toks[i]
		switch t.kind {
		case tRParen:
			return n, i + 1, nil
		case tLParen:
			child, ni, err := buildFrom(toks, i)
			if err != nil {
				return n, i, err
			}
			n.list = append(n.list, child)
			i = ni
		default:
			n.list = append(n.list, node{atom: t.atom})
			i++
		}
	}
	return n, i, nil
}
