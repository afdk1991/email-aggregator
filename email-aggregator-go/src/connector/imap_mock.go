package connector

// 本文件为测试辅助：一个进程内、纯标准库实现的 mock IMAP 服务端。
// 仅实现 PoC 所需的命令子集（CAPABILITY/STARTTLS/LOGIN·AUTHENTICATE/SELECT/UID FETCH/IDLE/LOGOUT），
// 用于驱动 RealIMAPConnector 跑通端到端采集闭环。
//
// 设计为「非 _test 文件」是因为 syncsv（另一包）的集成测试也需要启动这个 server，
// 而 Go 的 _test.go 文件无法被跨包导入。该 server 是惰性的——只有显式调用
// StartMockIMAP 才会监听端口，生产路径不会启动它，因此不影响真实运行语义。

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// MockIMAPServer 是一个内嵌的、进程内的 mock IMAP 服务端（仅用于测试）。
// 监听 127.0.0.1 随机端口，支持最小命令集以驱动 RealIMAPConnector 跑通采集闭环。
type MockIMAPServer struct {
	ln   net.Listener
	addr string
	// throttleBodyFetches 模拟 139/163 的 BODY 抓取节流：大于 0 时，接下来的
	// throttleBodyFetches 次 BODY 抓取返回 "OK Fetch completed" 但不带字面量
	// （仅测试用，仅测试协程可安全读写）。
	throttleBodyFetches int
}

// StartMockIMAP 启动 mock IMAP 服务端，返回服务器实例、停止函数与可能的错误。
// 停止函数用于测试收尾释放端口；出错时服务器未启动。
func StartMockIMAP() (*MockIMAPServer, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, func() {}, err
	}
	s := &MockIMAPServer{ln: ln, addr: ln.Addr().String()}
	go s.serve()
	stop := func() { _ = ln.Close() }
	return s, stop, nil
}

// Addr 返回监听地址（形如 127.0.0.1:port），供客户端连接。
func (s *MockIMAPServer) Addr() string { return s.addr }

func (s *MockIMAPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *MockIMAPServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	write := func(str string) {
		_, _ = w.WriteString(str + "\r\n")
		_ = w.Flush()
	}
	write("* OK mock IMAP4rev1 ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) < 2 {
			continue
		}
		tag, cmd := parts[0], strings.ToUpper(parts[1])
		switch {
		case strings.HasPrefix(cmd, "CAPABILITY"):
			write("* CAPABILITY IMAP4rev1 AUTH=PLAIN LOGIN IDLE")
			write(tag + " OK CAPABILITY completed")
		case cmd == "STARTTLS":
			write(tag + " OK Begin TLS")
		case cmd == "LOGIN", cmd == "AUTHENTICATE":
			write(tag + " OK authenticated")
		case strings.HasPrefix(cmd, "SELECT"):
			write("* 2 EXISTS")
			write("* 0 RECENT")
			write("* OK [UIDVALIDITY 1] UIDs valid")
			write("* FLAGS (\\Seen \\Flagged)")
			write(tag + " OK [READ-WRITE] SELECT completed")
		case strings.HasPrefix(cmd, "UID FETCH"), strings.HasPrefix(cmd, "FETCH"):
			upper := strings.ToUpper(line)
			if strings.Contains(upper, "BODY.PEEK[]") || strings.Contains(upper, "BODY[]") {
				if s.throttleBodyFetches > 0 {
					s.throttleBodyFetches--
					write(tag + " OK FETCH completed") // 节流：无 * FETCH 字面量
					continue
				}
				for _, l := range mockBodyResponses(line) {
					write(l)
				}
			} else {
				for _, l := range mockFetchResponses() {
					write(l)
				}
			}
			write(tag + " OK FETCH completed")
		case cmd == "IDLE":
			write("+ idling")
			done, _ := r.ReadString('\n')
			if strings.TrimSpace(done) == "DONE" {
				write("* 3 EXISTS")
				write(tag + " OK IDLE terminated")
			}
		case cmd == "LOGOUT":
			write("* BYE")
			write(tag + " OK LOGOUT")
			return
		default:
			write(tag + " OK (default)")
		}
	}
}

// mockFetchResponses 返回两封邮件的 FETCH 响应；含字面量与嵌套 ENVELOPE/BODYSTRUCTURE，
// 用于验证解析器对真实协议细节的处理（mail1 带附件 + 已读，mail2 无地址无附件）。
func mockFetchResponses() []string {
	return []string{
		`* 1 FETCH (UID 1 RFC822.SIZE 1024 INTERNALDATE "17-Jul-2026 10:00:00 +0000" FLAGS (\Seen) ENVELOPE ("Thu, 17 Jul 2026 10:00:00 +0000" "测试邮件主题" (("Alice" NIL "alice" "example.com")) (("Bob" NIL "bob" "example.com")) (("Carol" NIL "carol" "example.com")) NIL NIL NIL "<id1@x>") BODY[HEADER] {11}
X-Header: x
 BODYSTRUCTURE (("TEXT" "PLAIN" ("CHARSET" "utf-8") NIL NIL "7BIT" 100 5 NIL NIL NIL)("APPLICATION" "OCTET-STREAM" ("NAME" "doc.pdf") NIL NIL "BASE64" 500 NIL ("attachment" "doc.pdf") NIL) "MIXED"))`,
		`* 2 FETCH (UID 2 RFC822.SIZE 512 INTERNALDATE "18-Jul-2026 09:00:00 +0000" FLAGS () ENVELOPE ("Fri, 18 Jul 2026 09:00:00 +0000" "第二封无地址" NIL NIL NIL NIL NIL NIL NIL) BODYSTRUCTURE ("TEXT" "HTML" NIL NIL NIL "7BIT" 50 3 NIL NIL NIL))`,
	}
}

// mockBodyResponses 响应 UID FETCH (BODY.PEEK[])：按请求的 UID 返回完整 RFC822 原文（含字面量）。
func mockBodyResponses(cmdLine string) []string {
	var out []string
	for uid, raw := range mockIMAPRaws() {
		if !mockRequestedUID(cmdLine, uid) {
			continue
		}
		out = append(out, fmt.Sprintf("* %d FETCH (UID %d BODY[] {%d}\r\n%s)\r\n", uid, uid, len(raw), raw))
	}
	return out
}

// mockRequestedUID 判断命令行的 UID 列表中是否含指定 uid（支持 "1,2" 或 "1" 形式）。
func mockRequestedUID(cmdLine string, uid int) bool {
	up := strings.ToUpper(cmdLine)
	i := strings.Index(up, "UID FETCH")
	if i < 0 {
		return false
	}
	rest := cmdLine[i+len("UID FETCH"):]
	if j := strings.Index(rest, "("); j >= 0 {
		rest = rest[:j]
	}
	rest = strings.TrimSpace(rest)
	for _, part := range strings.Split(rest, ",") {
		if strings.TrimSpace(part) == strconv.Itoa(uid) {
			return true
		}
	}
	return false
}

// mockIMAPRaws 返回 mock IMAP 服务端的原始 RFC822 邮件（UID → raw）。
// mail1 带 text/plain + HTML（验证 text/plain 优先），mail2 仅 HTML（验证 HTML 兜底）。
func mockIMAPRaws() map[int]string {
	return map[int]string{
		1: "From: alice@example.com\r\n" +
			"To: bob@example.com\r\n" +
			"Cc: carol@example.com\r\n" +
			"Subject: =?UTF-8?B?5rWL6K+V6YKu5Lu277yMSU1BUCA=?=\r\n" +
			"Date: Thu, 17 Jul 2026 10:00:00 +0000\r\n" +
			"Message-ID: <imap-1@example.com>\r\n" +
			"Content-Type: multipart/alternative; boundary=m1\r\n" +
			"\r\n" +
			"--m1\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\n" +
			"IMAP 第一封正文（测试）。\r\n" +
			"--m1\r\n" +
			"Content-Type: text/html; charset=utf-8\r\n" +
			"\r\n" +
			"<p>IMAP HTML 正文</p>\r\n" +
			"--m1--\r\n",
		2: "From: bob@example.com\r\n" +
			"Subject: second imap mail\r\n" +
			"Date: Fri, 18 Jul 2026 09:00:00 +0000\r\n" +
			"Message-ID: <imap-2@example.com>\r\n" +
			"Content-Type: text/html; charset=utf-8\r\n" +
			"\r\n" +
			"<html><body><h1>第二封</h1><p>只有 HTML 正文</p></body></html>\r\n",
	}
}
