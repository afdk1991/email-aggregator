package connector

import (
	"io"
	"mime"
	"strings"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// 说明：IMAP ENVELOPE 的 subject / 地址显示名按 RFC 2047 以 encoded-word 传输
// （如 "=?gbk?b?...?=" / "=?utf-8?B?...?="），必须解码后才能入库展示。
// 协议解析仍为纯标准库；此处仅引入 Go 官方 x/text 字符集库处理 GBK/GB2312/GB18030
// 的编码转换（UTF-8 走 stdlib 原生路径，无额外依赖）。
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "utf-8", "utf8", "us-ascii", "ascii", "":
		return input, nil
	case "gbk", "gb2312", "gb18030":
		return transform.NewReader(input, simplifiedchinese.GBK.NewDecoder()), nil
	case "gb18030x":
		return transform.NewReader(input, simplifiedchinese.GB18030.NewDecoder()), nil
	case "big5":
		return input, nil // 无内置表，原样透传避免破坏原始字节
	default:
		return input, nil
	}
}

// decodeRFC2047 解码 MIME 头字段中的 RFC 2047 encoded-word；非编码内容原样返回。
func decodeRFC2047(s string) string {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "=?") {
		return s
	}
	wd := mime.WordDecoder{CharsetReader: charsetReader}
	dec, err := wd.DecodeHeader(s)
	if err != nil {
		return s // 解码失败回退原始值，不丢失信息
	}
	return strings.TrimSpace(dec)
}
