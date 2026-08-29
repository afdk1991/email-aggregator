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
	"net"
	"strings"
)

// MockIMAPServer 是一个内嵌的、进程内的 mock IMAP 服务端（仅用于测试）。
// 监听 127.0.0.1 随机端口，支持最小命令集以驱动 RealIMAPConnector 跑通采集闭环。
type MockIMAPServer struct {
	ln   net.Listener
	addr string
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
		case strings.HasPrefix(cmd, "UID FETCH"):
			for _, l := range mockFetchResponses() {
				write(l)
			}
			write(tag + " OK UID FETCH completed")
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
