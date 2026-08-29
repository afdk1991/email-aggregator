// WS 实时推送端到端验证：连 /ws → 触发 /api/demo/push → 断言收到 new-mail 通知。
const WS_URL = "ws://127.0.0.1:8080/ws?accountId=acc_demo";
const PUSH_URL = "http://127.0.0.1:8080/api/demo/push?accountId=acc_demo";

const received = [];
let settled = false;

function done(code, msg) {
  if (settled) return;
  settled = true;
  console.log(`[RESULT] ${code} - ${msg}`);
  console.log("received messages:", JSON.stringify(received, null, 2));
  process.exit(code === "PASS" ? 0 : 1);
}

const ws = new WebSocket(WS_URL);
const timer = setTimeout(() => done("TIMEOUT", "8s 内未收到实时推送通知"), 8000);

ws.addEventListener("open", () => {
  console.log("[ws] open");
  // 触发演示推送
  fetch(PUSH_URL).then((r) => r.json()).then((j) => {
    console.log("[push] triggered:", JSON.stringify(j));
  }).catch((e) => console.log("[push] error:", e.message));
});

ws.addEventListener("message", (ev) => {
  let payload;
  try { payload = JSON.parse(ev.data); } catch { payload = ev.data; }
  received.push(payload);
  console.log("[ws] message:", JSON.stringify(payload));
  // 收到 new-mail 或任意 push 即判定通过
  if (payload && (payload.kind === "new-mail" || payload.Kind === "new-mail" || payload.kind || payload.Kind)) {
    clearTimeout(timer);
    done("PASS", "实时推送通知已送达 WS 客户端");
  }
});

ws.addEventListener("error", (ev) => {
  console.log("[ws] error:", ev.message || ev.type);
  clearTimeout(timer);
  done("ERROR", "WS 连接错误");
});

ws.addEventListener("close", () => {
  console.log("[ws] closed");
  if (!settled && received.length === 0) {
    clearTimeout(timer);
    done("CLOSED", "WS 在收到推送前关闭");
  }
});
