// AI 日志页: 把一场面试里"系统做了什么判断"按时间读一遍。
//
// 报告给的是结论, 而复核时要看的是过程: 这一轮为什么追问、评分依据是哪句
// 原话、哪一步降级了、谁看过、谁改过分。这些信息以前散在四张表里,
// 串成一条时间线之后才真的能被用起来。
import { el, clear, api, navigate, toast } from './core.js';

const KIND_LABELS = {
  session: '会话',
  question: '提问',
  probe: '追问',
  answer: '作答',
  score: '评分',
  report: '报告',
  access: '访问',
  override: '改分',
  code: '判题',
  recording: '录像',
  observer: '旁听',
  proctor: '防作弊',
  audit: '审计',
};

export async function renderAILog(root, sessionId) {
  clear(root);
  root.append(el('p', { class: 'muted', text: '正在读取 AI 日志…' }));
  let data;
  try {
    data = await api.get(`/api/v1/sessions/${sessionId}/ai-log`);
  } catch (err) {
    clear(root);
    root.append(el('p', { class: 'muted', text: err.message }));
    return;
  }
  clear(root);
  const session = data.session || {};
  const timeline = data.timeline || [];

  root.append(
    el('div', { class: 'subbar' }, [
      el('button', {
        class: 'btn outline small',
        text: '返回报告',
        onclick: () => navigate(`/console/reports/${sessionId}`),
      }),
      el('span', { class: 'muted mono small', text: sessionId }),
      el('span', { class: 'pill mint', text: `${timeline.length} 条记录` }),
    ]),
    el('div', { class: 'view-head' }, [
      el('div', {}, [
        el('p', { class: 'eyebrow', text: 'AI 日志' }),
        el('h1', { class: 'display', text: '这场面试里, 系统做了什么判断。' }),
        el('p', {
          class: 'lede',
          text: `${session.candidate_name || '候选人'} · ${session.position || ''} · 第 ${session.round || 1} 轮`,
        }),
      ]),
    ]),
  );

  // 能力边界如实写在页面上, 而不是只留在代码注释里。
  const limits = data.limits || {};
  if (limits.note) {
    root.append(el('div', { class: 'callout warn' }, [
      el('strong', { text: '这份日志目前不能还原什么' }),
      el('p', { class: 'muted small', text: limits.note }),
    ]));
  }

  if (timeline.length === 0) {
    root.append(el('article', { class: 'card' }, [
      el('p', { class: 'muted', text: '这场面试还没有产生记录。' }),
    ]));
    return;
  }

  const list = el('div', { class: 'ai-timeline' });
  timeline.forEach((entry) => list.append(timelineRow(entry)));
  root.append(el('article', { class: 'card' }, [list]));
}

function timelineRow(entry) {
  const payload = entry.payload || {};
  const row = el('div', { class: `ai-entry kind-${entry.kind}` }, [
    el('div', { class: 'ai-entry-head' }, [
      el('span', { class: `pill ${kindClass(entry.kind)}`, text: KIND_LABELS[entry.kind] || entry.kind }),
      el('strong', { text: entry.title }),
      el('span', { class: 'muted small mono', text: formatTime(entry.at) }),
    ]),
  ]);
  if (entry.detail) {
    row.append(el('p', { class: 'ai-entry-detail', text: entry.detail }));
  }
  // 评分的证据原话逐条列出: 复核的核心就是"这个等级凭什么"。
  if (Array.isArray(payload.evidence) && payload.evidence.length > 0) {
    const quotes = el('div', { class: 'ai-evidence' });
    payload.evidence.forEach((e) => {
      quotes.append(el('blockquote', { class: `quote ${e.kind || 'neutral'}` }, [
        el('p', { text: `“${e.quote || ''}”` }),
        el('footer', {
          class: 'muted small',
          text: e.kind === 'against' ? '反证' : (e.matched ? `命中要点: ${e.matched}` : '支持'),
        }),
      ]));
    });
    row.append(quotes);
  }
  if (Array.isArray(payload.missing) && payload.missing.length > 0) {
    row.append(el('p', { class: 'muted small', text: `未覆盖: ${payload.missing.join(' / ')}` }));
  }
  // 追问的依据: 靶子 + 当时的检索命中(含分数)。
  // 这是"它凭什么问这句"的答案, 因此直接把快照摊开显示, 而不是给一个结论。
  if (payload.probe_focus || Array.isArray(payload.retrieval)) {
    const box = el('div', { class: 'ai-retrieval' });
    if (payload.probe_focus) {
      box.append(el('p', {
        class: 'muted small',
        text: `追问靶子: ${payload.probe_focus}${payload.probe_reference ? ` —— ${payload.probe_reference}` : ''}`,
      }));
    }
    const hits = Array.isArray(payload.retrieval) ? payload.retrieval : [];
    if (hits.length > 0) {
      box.append(el('p', { class: 'muted small', text: '当时的检索命中(按相关性):' }));
      box.append(el('ol', { class: 'hit-list' }, hits.map((h) => el('li', {}, [
        el('span', { class: 'mono small', text: h.question_id || '' }),
        h.point_key ? el('span', { class: 'pill mint', text: h.point_key }) : null,
        el('span', { class: 'muted small', text: (h.score || 0).toFixed(4) }),
        el('span', { text: h.text || '' }),
      ]))));
    }
    row.append(box);
  }
  // 降级必须显眼: 悄悄降级会让整批面试的评分标准在无人察觉时改变。
  if (payload.degraded_reason || payload.degraded_from) {
    row.append(el('p', { class: 'muted small error', text: payload.degraded_reason || `降级自 ${payload.degraded_from}` }));
  }
  if (payload.scorer) {
    row.append(el('p', { class: 'muted small', text: `评分器: ${payload.scorer}` }));
  }
  if (entry.actor) {
    row.append(el('p', { class: 'muted small', text: `操作主体: ${entry.actor}` }));
  }
  return row;
}

function kindClass(kind) {
  switch (kind) {
    case 'score':
    case 'report':
      return 'ok';
    case 'probe':
      return 'warn';
    case 'override':
    case 'proctor':
      return 'warn';
    case 'proctor':
      return 'warn';
    default:
      return 'neutral';
  }
}

function formatTime(value) {
  if (!value) return '—';
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleTimeString('zh-CN', { hour12: false });
}
