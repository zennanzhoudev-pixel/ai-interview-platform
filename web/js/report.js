// 报告渲染。候选人与招聘工作台共用同一份实现。
//
// 共用不是为了省代码, 而是为了保证"候选人看到的"和"面试官看到的"
// 是同一个东西。两套渲染迟早会出现口径差异(一处显示 L4 一处显示 4 分),
// 而那正是招聘里最容易引发争议的地方。
import {
  el, clear, competencyLabel, recommendationLabel, stageLabel, levelClass,
  fmtDate, fmtDuration, toast,
} from './core.js';

const EVIDENCE_KIND_LABELS = {
  support: '支持',
  against: '反证',
  neutral: '参考',
};

// renderReport 把报告渲染到容器里。
export function renderReport(container, report, options = {}) {
  const { showHeading = true, resume = null, actions = null } = options;
  clear(container);
  if (!report) {
    container.append(el('p', { class: 'muted', text: '报告尚未生成。' }));
    return;
  }
  if (showHeading) container.append(reportHead(report));
  container.append(scoreSection(report));

  const dims = Array.isArray(report.dimensions) ? report.dimensions : [];
  if (dims.length > 0) {
    container.append(el('hr', { class: 'rule' }));
    container.append(dimensionSection(dims));
  }

  const findings = findingsSection(report);
  if (findings) {
    container.append(el('hr', { class: 'rule' }));
    container.append(findings);
  }

  const resumeSection = resumeLinkageSection(report, resume);
  if (resumeSection) {
    container.append(el('hr', { class: 'rule' }));
    container.append(resumeSection);
  }

  container.append(el('hr', { class: 'rule' }));
  container.append(turnSection(report));

  if (actions) {
    container.append(el('div', { class: 'actions report-actions' }, actions));
  }
}

function reportHead(report) {
  const stats = report.stats || {};
  return el('div', { class: 'report-head' }, [
    el('div', {}, [
      el('p', { class: 'eyebrow', text: `第 ${report.round || 1} 轮 · 第 ${stats.turns || 0} 轮问答` }),
      el('h1', { class: 'display', text: recommendationLabel(report.recommendation) }),
      el('p', {
        class: 'muted',
        text: `用时 ${fmtDuration(report.duration_sec)} / 预算 ${fmtDuration(report.budget_sec)}`
          + ` · 追问 ${stats.probes || 0} 次 · 置信度 ${((report.confidence || 0) * 100).toFixed(0)}%`,
      }),
    ]),
    el('span', { class: `pill ${levelClass(report.recommendation === 'NO_HIRE' ? 'WEAK' : 'STRONG')}` },
      [el('i'), 'AI 建议 · 由人决定']),
  ]);
}

function scoreSection(report) {
  const dims = Array.isArray(report.dimensions) ? report.dimensions : [];
  const right = el('div', { class: 'score-right' });
  if (dims.length === 0) {
    right.append(el('p', { class: 'muted small', text: '本场没有产生可评分的能力项。' }));
  }
  dims.forEach((dim) => {
    const pct = Math.max(0, Math.min(100, (dim.level_num || 0) * 20));
    right.append(el('div', { class: 'dim-row' }, [
      el('div', { class: 'dim-top' }, [
        el('span', { class: 'dim-name', text: competencyLabel(dim.competency) }),
        el('span', { class: 'muted small', text: `${dim.level || '—'} · ${dim.turns || 0} 轮` }),
      ]),
      el('div', { class: 'bar' }, [el('i', { style: { width: `${pct}%` } })]),
      el('p', {
        class: 'muted small',
        text: `置信度 ${((dim.confidence || 0) * 100).toFixed(0)}%`
          + (dim.concerns && dim.concerns.length ? ` · 未覆盖: ${dim.concerns.join(' / ')}` : ''),
      }),
    ]));
  });
  return el('div', { class: 'score-grid' }, [
    el('div', { class: 'score-left' }, [
      el('div', { class: 'score' }, [
        el('span', { class: 'score-num', text: String(report.score ?? 0) }),
        el('span', { class: 'score-unit', text: '/ 100' }),
      ]),
      el('p', { class: 'muted small', text: '综合评分。等级到分数的换算规则写死在代码里, 不交给模型决定。' }),
    ]),
    right,
  ]);
}

function dimensionSection(dims) {
  const body = el('div', { class: 'evidence-grid' });
  dims.forEach((dim) => {
    const card = el('article', { class: 'card tight' }, [
      el('div', { class: 'card-head' }, [
        el('span', { class: `pill ${levelClass(dim.level)}`, text: dim.level || '—' }),
        el('div', {}, [
          el('h3', { text: competencyLabel(dim.competency) }),
          el('p', { class: 'muted small', text: `${dim.turns || 0} 轮问答 · 置信度 ${((dim.confidence || 0) * 100).toFixed(0)}%` }),
        ]),
      ]),
    ]);
    const evidence = Array.isArray(dim.evidence) ? dim.evidence : [];
    if (evidence.length === 0) {
      card.append(el('p', { class: 'muted small', text: '没有可引用的原话证据。' }));
    }
    evidence.forEach((ev) => {
      card.append(el('blockquote', { class: `quote ${ev.kind || 'neutral'}` }, [
        el('p', { text: `“${ev.quote || ''}”` }),
        el('footer', {
          class: 'muted small',
          text: `${EVIDENCE_KIND_LABELS[ev.kind] || '参考'}`
            + (ev.matched ? ` · 命中要点: ${ev.matched}` : '')
            + (ev.question_id ? ` · 题目 ${ev.question_id}` : ''),
        }),
      ]));
    });
    body.append(card);
  });
  return el('section', {}, [
    el('h2', { class: 'section-title', text: '能力维度与证据' }),
    el('p', { class: 'muted small', text: '每个维度分都必须挂候选人原话; 拿不出证据的分数不会出现在这里。' }),
    body,
  ]);
}

function findingsSection(report) {
  const gaps = Array.isArray(report.gaps) ? report.gaps : [];
  const flags = Array.isArray(report.flags) ? report.flags : [];
  if (gaps.length === 0 && flags.length === 0) return null;
  const node = el('section', {}, [el('h2', { class: 'section-title', text: '缺口与风险提示' })]);
  if (gaps.length) {
    node.append(el('div', { class: 'callout' }, [
      el('strong', { text: '未覆盖的考察项' }),
      el('ul', { class: 'doc-list' }, gaps.map((g) => el('li', { text: competencyLabel(g) }))),
    ]));
  }
  if (flags.length) {
    node.append(el('div', { class: 'callout warn' }, [
      el('strong', { text: '需要人工注意的信号' }),
      el('ul', { class: 'doc-list' }, flags.map((f) => el('li', { text: flagText(f) }))),
    ]));
  }
  return node;
}

function flagText(flag) {
  if (typeof flag === 'string') return flag;
  if (flag && flag.type) return `${flag.type}${flag.detail ? `: ${flag.detail}` : ''}`;
  return JSON.stringify(flag);
}

// resumeLinkageSection 展示"追问引用了简历哪一句"。
//
// 匹配方式是说清楚的老实话: 按字面出现来判断"这道题提到了简历里的
// 这个实体"。它不做语义推断, 因此不会出现"AI 说简历写了某件事,
// 而简历里其实没有"的情况 —— 这恰恰是原文定位存在的意义。
function resumeLinkageSection(report, resume) {
  const entities = resume && Array.isArray(resume.entities) ? resume.entities : [];
  if (entities.length === 0) return null;
  const turns = Array.isArray(report.turns) ? report.turns : [];
  const rows = [];
  turns.forEach((turn) => {
    const text = `${turn.question || ''}`;
    entities.forEach((ent) => {
      if (!ent.value || !text.includes(ent.value)) return;
      rows.push(el('tr', {}, [
        el('td', { class: 'mono small', text: turn.question_id || `#${turn.index}` }),
        el('td', { text: ent.value }),
        el('td', { class: 'mono small', text: `第 ${ent.start}–${ent.end} 字` }),
        el('td', { class: 'muted small', text: excerpt(resume.source, ent.start, ent.end) }),
      ]));
    });
  });
  if (rows.length === 0) return null;
  return el('section', {}, [
    el('h2', { class: 'section-title', text: '简历原文定位' }),
    el('p', {
      class: 'muted small',
      text: '追问命中简历实体时, 会精确定位到原文的字偏移 —— 面试官可以据此核对候选人是否真的写过这件事。',
    }),
    el('table', { class: 'doc-table' }, [
      el('thead', {}, [el('tr', {}, [
        el('th', { text: '题目' }), el('th', { text: '命中的简历实体' }),
        el('th', { text: '原文位置' }), el('th', { text: '上下文' }),
      ])]),
      el('tbody', {}, rows),
    ]),
  ]);
}

function excerpt(source, start, end) {
  if (!source) return '—';
  const chars = Array.from(source);
  const from = Math.max(0, start - 12);
  const to = Math.min(chars.length, end + 12);
  return `${from > 0 ? '…' : ''}${chars.slice(from, to).join('')}${to < chars.length ? '…' : ''}`;
}

function turnSection(report) {
  const turns = Array.isArray(report.turns) ? report.turns : [];
  const list = el('div', { class: 'transcript readonly' });
  turns.forEach((turn) => {
    const verdict = turn.verdict || {};
    const final = verdict.final || {};
    list.append(el('div', { class: `turn ${turn.is_probe ? 'probe' : ''}` }, [
      el('p', { class: 'turn-q' }, [
        el('span', { class: 'turn-tag', text: turn.is_probe ? '追问' : stageLabel(turn.stage) }),
        turn.question,
      ]),
      el('p', { class: 'turn-a', text: turn.answer || '(未作答)' }),
      el('p', { class: 'muted small' }, [
        turn.scored
          ? `评分 ${final.level || '—'} · 置信度 ${((final.confidence || 0) * 100).toFixed(0)}%`
            + (final.degraded_from ? ` · 降级自 ${final.degraded_from}` : '')
          : '本题不参与评分',
      ]),
    ]));
  });
  return el('section', {}, [
    el('h2', { class: 'section-title', text: '逐轮记录' }),
    list,
  ]);
}

/* ---------------- 导出与打印 ---------------- */

export function downloadJSON(filename, data) {
  const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
  const url = URL.createObjectURL(blob);
  const link = el('a', { href: url, download: filename });
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 2000);
}

// printReport 触发浏览器打印。
//
// 导出 PDF 走的就是浏览器自带的"打印 -> 另存为 PDF": 它不引入任何依赖,
// 而且排版由 @media print 精确控制。之前这个按钮点了没反应, 原因是
// 直接用 window.print() 时页面上还有隐藏的弹层与导航, 打印预览是一片空白。
export function printReport(title) {
  const previous = document.title;
  document.title = title || previous;
  document.body.classList.add('printing');
  const cleanup = () => {
    document.body.classList.remove('printing');
    document.title = previous;
    window.removeEventListener('afterprint', cleanup);
  };
  window.addEventListener('afterprint', cleanup);
  try {
    window.print();
  } catch (err) {
    cleanup();
    toast('当前浏览器不支持直接打印, 请使用 Cmd/Ctrl + P', 'error');
  }
  setTimeout(cleanup, 4000);
}

export function reportFilename(session) {
  const who = (session && session.candidate_name) || 'candidate';
  const id = (session && session.session_id) || 'session';
  return `${who}-${id}.json`;
}

export function sessionMeta(session) {
  if (!session) return '—';
  return `${session.company || ''} · ${session.position || ''} · 第 ${session.round || 1} 轮 · ${fmtDate(session.created_at)}`;
}
