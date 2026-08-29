package notify

// 零依赖最小 WebSocket 服务端（RFC6455 子集），将 Notifier 事件实时推送给浏览器。
// 与 integration.WsHub（生产版，依赖 Kafka 转发 + 第三方 WS 库）形态一致，但此处用标准库实现、零外部依赖。
// 仅支持服务端→客户端文本推送 + 处理客户端 close/ping 控制帧；足以驱动前端实时通知演示。

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Hub 持有所有活动 WS 连接，并把 Notifier 的发布事件转发到对应账户的订阅连接。
type Hub struct {
	notifier Notifier
	mu       sync.Mutex
	conns    map[*wsConn]struct{}
}

// NewHub 构造
func NewHub(n Notifier) *Hub {
	return &Hub{notifier: n, conns: map[*wsConn]struct{}{}}
}

// ConnCount 返回当前活动 WS 连接数（供按需触发演示推送等使用）。
func (h *Hub) ConnCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// ActiveAccounts 返回当前有活动 WS 连接的去重账户列表（用于演示推送按账户定向）。
func (h *Hub) ActiveAccounts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[string]struct{}{}
	out := make([]string, 0, len(h.conns))
	for c := range h.conns {
		if _, ok := seen[c.accountID]; ok {
			continue
		}
		seen[c.accountID] = struct{}{}
		out = append(out, c.accountID)
	}
	return out
}

// Upgrade 执行 WS 握手并注册该账户的推送 sink。握手失败时直接返回（不劫持连接）。
func (h *Hub) Upgrade(w http.ResponseWriter, r *http.Request, accountID string) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket unsupported", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	// 手写 101 握手（Hijack 后由我们完全接管 socket）。
	accept := computeAccept(key)
	handshake := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := conn.Write([]byte(handshake)); err != nil {
		conn.Close()
		return
	}

	c := &wsConn{
		raw:       conn,
		accountID: accountID,
		hub:       h,
		writeMu:   sync.Mutex{},
		closeOnce: sync.Once{},
		done:      make(chan struct{}),
	}
	h.notifier.AddSink(accountID, c)
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()

	go c.readPump()
}

// computeAccept 计算 Sec-WebSocket-Accept（RFC6455 §1.3）
func computeAccept(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsConn 表示一个已建立的 WS 连接，同时实现 PushSink 以便被 Notifier 直接推送。
type wsConn struct {
	raw       net.Conn
	accountID string
	hub       *Hub
	writeMu   sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
}

// Send 实现 PushSink：将通知载荷以文本帧写入连接（并发安全）。
func (c *wsConn) Send(p NotificationPayload) {
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	frame := EncodeTextFrame(string(data))
	c.writeMu.Lock()
	_, _ = c.raw.Write(frame)
	c.writeMu.Unlock()
}

// Close 实现 PushSink：关闭底层连接（幂等）。
func (c *wsConn) Close() {
	c.close()
}

func (c *wsConn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.raw.Close()
	})
}

// readPump 读取客户端帧，处理 close/ping 控制帧；连接断开或出错时清理。
func (c *wsConn) readPump() {
	defer c.cleanup()
	for {
		select {
		case <-c.done:
			return
		default:
		}
		opcode, payload, err := readClientFrame(c.raw)
		if err != nil {
			return
		}
		switch opcode {
		case 0x8: // close
			return
		case 0x9: // ping → 回 pong
			c.writeControl(0xA, payload)
		case 0xA: // pong，忽略
		}
	}
}

// writeControl 发送控制帧（服务端→客户端，不掩码）。
func (c *wsConn) writeControl(opcode byte, payload []byte) {
	frame := encodeFrame(opcode, payload)
	c.writeMu.Lock()
	_, _ = c.raw.Write(frame)
	c.writeMu.Unlock()
}

// cleanup 注销 sink 并从 Hub 移除，随后关闭连接。
func (c *wsConn) cleanup() {
	c.hub.notifier.RemoveSink(c.accountID, c)
	c.hub.mu.Lock()
	delete(c.hub.conns, c)
	c.hub.mu.Unlock()
	c.close()
}

// readClientFrame 解析客户端发来的单帧（已掩码）。返回 opcode 与去掩码后的 payload。
func readClientFrame(r io.Reader) (byte, []byte, error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return 0, nil, err
	}
	opcode := hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	length := int(hdr[1] & 0x7F)
	if length == 126 {
		ext := make([]byte, 2)
		if _, err := io.ReadFull(r, ext); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint16(ext))
	} else if length == 127 {
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint64(ext))
	}
	var mask []byte
	if masked {
		mask = make([]byte, 4)
		if _, err := io.ReadFull(r, mask); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}
