// 面试并发压测: 同时开 N 场面试, 每场走完全部问答。
//
// 为什么压测的是"一场完整面试"而不是"单接口 QPS":
// 这个系统的瓶颈几乎从来不是某一个接口, 而是长连接数 × 每场轮次 ×
// 每轮的落库与评分。只压 POST /sessions 会得到一个漂亮但无意义的数字。
//
// 用法(服务需以演示模式或提前准备好 API Key 运行):
//   node deployments/loadtest.mjs --base http://localhost:8080 --sessions 20 --api-key xxx
import { parseArgs } from 'node:util';

const { values } = parseArgs({
  options: {
    base: { type: 'string', default: 'http://localhost:8080' },
    sessions: { type: 'string', default: '10' },
    'api-key': { type: 'string', default: '' },
    round: { type: 'string', default: '1' },
  },
});

const base = values.base;
const total = Number(values.sessions);
const apiKey = values['api-key'];

const answer =
  '用了 ZSet 和 score 存权重, 内存做过估算, 排序和 70% 的收益都验证过; '
  + '幂等靠唯一键去重, 状态机保证最终一致; 令牌桶和滑动窗口都考虑过, 容量估算配了降级预案; '
  + '三色标记加写屏障, 本地队列和抢占用上了用户态的优势, 冷启动靠编译产物复用; '
  + '先写库再删缓存, 用延迟双删和版本号保证一致。';

function headers() {
  const h = { 'Content-Type': 'application/json' };
  if (apiKey) h.Authorization = `Bearer ${apiKey}`;
  return h;
}

async function post(path, body) {
  const resp = await fetch(base + path, {
    method: 'POST',
    headers: headers(),
    body: JSON.stringify(body),
  });
  const text = await resp.text();
  if (!resp.ok) throw new Error(`POST ${path} -> ${resp.status} ${text}`);
  return text ? JSON.parse(text) : {};
}

// runSession 建会话 -> 确认授权 -> 走完所有问答 -> 拿到报告。
async function runSession(index) {
  const created = await post('/api/v1/sessions', {
    round: Number(values.round),
    minutes: 30,
    candidate_id: `loadtest-${index}-${Date.now()}`,
    candidate_name: `压测候选人 ${index}`,
    position: '后端工程师',
    consent_recording: true,
    consent_scoring: true,
  });
  const { session_id: sessionId, session_token: token } = created;

  const latencies = [];
  const ws = new WebSocket(
    `${base.replace('http', 'ws')}/ws/interview/${sessionId}?token=${encodeURIComponent(token)}`,
  );
  let turns = 0;

  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('面试超时')), 120000);
    ws.addEventListener('message', (event) => {
      const msg = JSON.parse(event.data);
      if (msg.type === 'state') {
        ws.send(JSON.stringify({ type: 'answer', text: answer }));
      } else if (msg.type === 'question') {
        turns += 1;
        if (msg.probe) latencies.push(1);
        ws.send(JSON.stringify({ type: 'answer', text: answer }));
      } else if (msg.type === 'report') {
        clearTimeout(timer);
        resolve();
      }
    });
    ws.addEventListener('error', () => {
      clearTimeout(timer);
      reject(new Error(`WebSocket 错误(会话 ${sessionId})`));
    });
  });
  ws.close();
  return { sessionId, turns };
}

const started = Date.now();
const results = await Promise.allSettled(
  Array.from({ length: total }, (_, i) => runSession(i + 1)),
);
const elapsed = Date.now() - started;

const ok = results.filter((r) => r.status === 'fulfilled');
const failed = results.filter((r) => r.status === 'rejected');
const turns = ok.reduce((sum, r) => sum + r.value.turns, 0);

console.log(`并发场次: ${total}`);
console.log(`成功: ${ok.length} | 失败: ${failed.length}`);
console.log(`总轮次: ${turns} | 总耗时: ${elapsed}ms`);
console.log(`每秒完成轮次: ${(turns / (elapsed / 1000)).toFixed(2)}`);
failed.slice(0, 5).forEach((f) => console.error('失败原因:', f.reason.message));
process.exit(failed.length ? 1 : 0);
