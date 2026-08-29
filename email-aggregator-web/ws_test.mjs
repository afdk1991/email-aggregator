// WebSocket 实时推送端到端验证：连接 ws://127.0.0.1:8082/ws?accountId=acc_kafka
// 连接建立后触发 /api/demo/push，验证实时收到 new-mail 通知。
const ACCOUNT = 'acc_kafka';
const wsUrl = `ws://127.0.0.1:8082/ws?accountId=${ACCOUNT}`;
const ws = new WebSocket(wsUrl);

const results = [];
let connected = false;

const timeout = setTimeout(() => {
  console.log('RESULT_TIMEOUT', JSON.stringify(results));
  process.exit(1);
}, 20000);

ws.onopen = async () => {
  connected = true;
  console.log('WS_CONNECTED');
  // 连接成功后触发 demo push（生产由真实同步管线驱动）
  const r = await fetch('http://127.0.0.1:8082/api/demo/push?accountId=' + ACCOUNT, { method: 'POST' });
  const body = await r.json();
  console.log('PUSH_RESP', JSON.stringify(body));
  results.push({ step: 'push', ok: body.ok, id: body.id });
};

ws.onmessage = (ev) => {
  let data = ev.data;
  // 可能是字符串或 Blob
  const parse = (s) => {
    try { return JSON.parse(s); } catch { return s; }
  };
  const p = typeof data === 'string' ? parse(data) : parse(data.toString());
  console.log('WS_MSG', typeof p === 'string' ? p : JSON.stringify(p));
  results.push({ step: 'ws_msg', payload: p });
  // 收到 new-mail 通知即通过
  if (p && p.kind === 'new-mail') {
    console.log('RESULT_PASS', JSON.stringify(results));
    clearTimeout(timeout);
    ws.close();
    process.exit(0);
  }
};

ws.onerror = (e) => {
  console.log('WS_ERROR', e.message || 'error');
  results.push({ step: 'ws_error' });
  clearTimeout(timeout);
  process.exit(1);
};

ws.onclose = (e) => {
  console.log('WS_CLOSED', e.code, e.reason);
};
