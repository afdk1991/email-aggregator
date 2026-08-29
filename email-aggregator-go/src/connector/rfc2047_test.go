package connector

import "testing"

func TestDecodeRFC2047(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// UTF-8 base64 encoded-word
		{"=?utf-8?B?5Lqy77yM5oGt5Zac5oKo5Y+v5Lul55+l6YGT?=", "亲，恭喜您可以知道"},
		// 混合：前缀 + encoded-word
		{"Re: =?utf-8?B?5q2j5Zyo5oCO5qC3?=", "Re: 正在怎样"},
		// 纯文本原样
		{"Meeting at 3pm", "Meeting at 3pm"},
		// 空串
		{"", ""},
		// 非编码内容（含 =? 但不构成合法 encoded-word）回退原值，不丢信息
		{"a=?b?c", "a=?b?c"},
	}
	for _, c := range cases {
		got := decodeRFC2047(c.in)
		if got != c.want {
			t.Errorf("decodeRFC2047(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDecodeRFC2047_GBK 验证 139 服务器常用的 GBK/GB18030 encoded-word 能解出可读中文（非乱码）。
func TestDecodeRFC2047_GBK(t *testing.T) {
	// 取自真实 imap.139.com ENVELOPE 的完整 subject（=?gb18030?B?...?=）
	raw := "=?gb18030?B?16q3oqO6u6rPxNDF08O/qC2159fT1cu1pQ==?="
	want := "转发：华夏信用卡-电子账单"
	got := decodeRFC2047(raw)
	if got != want {
		t.Errorf("decodeRFC2047(gb18030) = %q, want %q", got, want)
	}
	if containsAny(got, "\uFFFD") {
		t.Errorf("GB18030 解码出现替换符（乱码）: %q", got)
	}
	t.Logf("GB18030 解码样例: %q", got)
}

func containsAny(s, chars string) bool {
	for _, r := range s {
		for _, c := range chars {
			if r == c {
				return true
			}
		}
	}
	return false
}
