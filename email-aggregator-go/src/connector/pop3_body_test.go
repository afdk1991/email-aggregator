package connector

import (
	"strings"
	"testing"

	"email-aggregator-go/src/model"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// 覆盖 RFC822 解析（parseRFC822Message → pop3BodyParts）对真实世界邮件形态的处理：
//  1. multipart/alternative 只有 text/html（新闻简报/营销信）→ BodyText 兜底为剥离标签后的纯文本，BodyHTML 保留原始 HTML；
//  2. multipart/alternative 同时含 text/plain + text/html → 优先 text/plain，且 BodyHTML 单独保留；
//  3. text/plain 用 GBK 字符集 → 自动转码为 UTF-8（避免中文乱码）；
//  4. quoted-printable 编码正文 → 自动解码。

func parseBody(t *testing.T, raw string) (text, html string) {
	t.Helper()
	m, err := parseRFC822Message([]byte(raw), "u@example.com", "id1", int64(len(raw)), model.ProviderPOP3)
	if err != nil {
		t.Fatalf("parseRFC822Message: %v", err)
	}
	return m.BodyText, m.BodyHTML
}

func TestPOP3Body_HTMLOnlyFallback(t *testing.T) {
	raw := "From: n@qoder.com\r\n" +
		"To: u@example.com\r\n" +
		"Subject: Newsletter\r\n" +
		"Content-Type: multipart/alternative; boundary=b\r\n" +
		"Date: Fri, 17 Jul 2026 10:00:00 +0000\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<html><body><h1>标题</h1><p>这是一段<b>加粗</b>正文</p><br>第二行</body></html>\r\n" +
		"--b--\r\n"
	text, html := parseBody(t, raw)
	if !strings.Contains(text, "标题") || !strings.Contains(text, "这是一段加粗正文") || !strings.Contains(text, "第二行") {
		t.Errorf("HTML-only 兜底失败: text=%q", text)
	}
	if !strings.Contains(html, "<h1>") {
		t.Errorf("BodyHTML 应为原始 HTML: html=%q", html)
	}
}

func TestPOP3Body_PreferTextPlain(t *testing.T) {
	raw := "From: n@example.com\r\n" +
		"To: u@example.com\r\n" +
		"Subject: both\r\n" +
		"Content-Type: multipart/alternative; boundary=b\r\n" +
		"Date: Fri, 17 Jul 2026 10:00:00 +0000\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"纯文本正文\r\n" +
		"--b\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>HTML 正文</p>\r\n" +
		"--b--\r\n"
	text, html := parseBody(t, raw)
	if text != "纯文本正文" {
		t.Errorf("应优先 text/plain（multipart 吞掉边界前 CRLF）: %q", text)
	}
	if !strings.Contains(html, "HTML 正文") {
		t.Errorf("BodyHTML 未提取: %q", html)
	}
}

func TestPOP3Body_GBKCharset(t *testing.T) {
	gbk, _, _ := transform.String(simplifiedchinese.GBK.NewEncoder(), "中文正文测试")
	raw := "From: n@example.com\r\n" +
		"To: u@example.com\r\n" +
		"Subject: gbk\r\n" +
		"Content-Type: text/plain; charset=gbk\r\n" +
		"Date: Fri, 17 Jul 2026 10:00:00 +0000\r\n" +
		"\r\n" +
		gbk + "\r\n"
	text, _ := parseBody(t, raw)
	if !strings.Contains(text, "中文正文测试") {
		t.Errorf("GBK 应转码为 UTF-8: %q", text)
	}
}

func TestPOP3Body_QuotedPrintable(t *testing.T) {
	raw := "From: n@example.com\r\n" +
		"To: u@example.com\r\n" +
		"Subject: qp\r\n" +
		"Content-Type: multipart/alternative; boundary=b\r\n" +
		"Date: Fri, 17 Jul 2026 10:00:00 +0000\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"=E4=B8=AD=E6=96=87=E6=AD=A3=E6=96=87\r\n" +
		"--b--\r\n"
	text, _ := parseBody(t, raw)
	if text != "中文正文" {
		t.Errorf("quoted-printable 应自动解码: %q", text)
	}
}

func TestPOP3Body_QuotedPrintableHTML(t *testing.T) {
	// HTML-only 子部分 + quoted-printable（新闻简报/营销信典型）：解码后再 htmlToText。
	raw := "From: n@example.com\r\n" +
		"To: u@example.com\r\n" +
		"Subject: qp-html\r\n" +
		"Content-Type: multipart/alternative; boundary=b\r\n" +
		"Date: Fri, 17 Jul 2026 10:00:00 +0000\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"<p>Hello =E4=B8=96=E7=95=8C,</p>\r\n" +
		"<br>\r\n" +
		"<div>Release =C2=A9 2026</div>\r\n" +
		"--b--\r\n"
	text, _ := parseBody(t, raw)
	for _, want := range []string{"Hello 世界,", "Release © 2026"} {
		if !strings.Contains(text, want) {
			t.Errorf("QP+HTML 应解码并提取文本, 缺 %q: %q", want, text)
		}
	}
	if strings.Contains(text, "=0D") || strings.Contains(text, "=C2") {
		t.Errorf("QP 转义残留: %q", text)
	}
}

func TestPOP3Body_Base64Part(t *testing.T) {
	// base64 编码的 text/plain 子部分（常见于 Gmail/邮件客户端）。
	raw := "From: n@example.com\r\n" +
		"To: u@example.com\r\n" +
		"Subject: b64\r\n" +
		"Content-Type: multipart/alternative; boundary=b\r\n" +
		"Date: Fri, 17 Jul 2026 10:00:00 +0000\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"5Lit5paH5q2j5paH\r\n" + // 中文正文 (base64)
		"--b--\r\n"
	text, _ := parseBody(t, raw)
	if text != "中文正文" {
		t.Errorf("base64 应自动解码: %q", text)
	}
}

func TestPOP3Body_NestedMultipart(t *testing.T) {
	// multipart/mixed > multipart/alternative > (text/plain, text/html)：真实邮件常见结构，
	// 扁平遍历会漏掉嵌套子部分，必须递归下降。
	raw := "From: n@example.com\r\n" +
		"To: u@example.com\r\n" +
		"Subject: nested\r\n" +
		"Content-Type: multipart/mixed; boundary=m\r\n" +
		"Date: Fri, 17 Jul 2026 10:00:00 +0000\r\n" +
		"\r\n" +
		"--m\r\n" +
		"Content-Type: multipart/alternative; boundary=a\r\n" +
		"\r\n" +
		"--a\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"外层正文可用\r\n" +
		"--a\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>HTML 版本</p>\r\n" +
		"--a--\r\n" +
		"--m\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"a.pdf\"\r\n" +
		"\r\n" +
		"%PDF-1.4 fake\r\n" +
		"--m--\r\n"
	text, _ := parseBody(t, raw)
	if text != "外层正文可用" {
		t.Errorf("嵌套 multipart 应提取到 text/plain: %q", text)
	}
}
