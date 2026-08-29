package connector

// 本文件为测试辅助：一个进程内、纯标准库实现的 mock POP3 服务端（RFC 1939 子集）。
// 支持 USER/PASS 鉴权、STAT/LIST/UIDL 元数据、RETR 全文拉取、QUIT 断开，
// 用于驱动 RealPOP3Connector 跑通端到端采集闭环。
//
// 设计为「非 _test 文件」是因为 syncsv（另一包）的集成测试也需要启动这个 server，
// 而 Go 的 _test.go 文件无法被跨包导入。该 server 是惰性的——只有显式调用
// StartMockPOP3 才会监听端口，生产路径不会启动它，因此不影响真实运行语义。

import (
	"bufio"
	"net"
	"strconv"
	"strings"
)

// MockPOP3Server 内嵌的进程内 mock POP3 服务端（仅用于测试）。
type MockPOP3Server struct {
	ln   net.Listener
	addr string
}

// StartMockPOP3 启动 mock POP3 服务端，返回服务器实例、停止函数与可能的错误。
func StartMockPOP3() (*MockPOP3Server, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, func() {}, err
	}
	s := &MockPOP3Server{ln: ln, addr: ln.Addr().String()}
	go s.serve()
	stop := func() { _ = ln.Close() }
	return s, stop, nil
}

// Addr 返回监听地址（形如 127.0.0.1:port）。
func (s *MockPOP3Server) Addr() string { return s.addr }

func (s *MockPOP3Server) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *MockPOP3Server) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	write := func(str string) {
		_, _ = w.WriteString(str + "\r\n")
		_ = w.Flush()
	}
	write("+OK mock POP3 ready")
	authed := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		parts := strings.SplitN(line, " ", 2)
		cmd := strings.ToUpper(parts[0])
		switch cmd {
		case "USER":
			write("+OK user accepted")
		case "PASS":
			// 密码 "badpass" 视为鉴权失败（供测试验证 -ERR 分支）
			if len(parts) == 2 && parts[1] == "badpass" {
				write("-ERR invalid password")
			} else {
				authed = true
				write("+OK logged in")
			}
		case "STAT":
			write("+OK 2 1536")
		case "LIST":
			write("+OK 2 messages (1536 octets)")
			write("1 1024")
			write("2 512")
			write(".")
		case "UIDL":
			write("+OK 2 messages")
			write("1 <pop3-uidl-1@example.com>")
			write("2 <pop3-uidl-2@example.com>")
			write(".")
		case "RETR":
			var msg string
			if len(parts) == 2 && parts[1] == "1" {
				msg = mockPOP3Messages()[0]
			} else {
				msg = mockPOP3Messages()[1]
			}
			write("+OK " + strconv.Itoa(len([]byte(msg))) + " octets")
			for _, l := range strings.Split(strings.TrimSuffix(msg, "\r\n"), "\r\n") {
				// dot-stuffing：以 "." 开头的行在传输中前加 "."（本数据无此类行，仍按规范处理）
				if strings.HasPrefix(l, ".") {
					l = "." + l
				}
				write(l)
			}
			write(".")
		case "QUIT":
			write("+OK bye")
			return
		case "NOOP":
			write("+OK")
		default:
			if !authed {
				write("-ERR not authenticated")
			} else {
				write("-ERR unknown command " + cmd)
			}
		}
	}
}

// mockPOP3Messages 返回两封 POP3 邮件（RFC 822 原始字节；mail1 带 RFC 2047 编码主题）。
func mockPOP3Messages() []string {
	return []string{
		"From: alice@example.com\r\n" +
			"To: bob@example.com\r\n" +
			"Cc: carol@example.com\r\n" +
			"Subject: =?UTF-8?B?5rWL6K+V5Li76aKY77yMUE9QMw==?=\r\n" +
			"Date: Fri, 17 Jul 2026 10:00:00 +0000\r\n" +
			"Message-ID: <pop3-1@example.com>\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\n" +
			"POP3 第一封邮件正文（测试）。\r\n",
		"From: bob@example.com\r\n" +
			"Subject: second pop3 mail\r\n" +
			"Date: Sat, 18 Jul 2026 09:00:00 +0000\r\n" +
			"Message-ID: <pop3-2@example.com>\r\n" +
			"Content-Type: text/plain\r\n" +
			"\r\n" +
			"second body line one\r\n" +
			"second body line two\r\n",
	}
}
