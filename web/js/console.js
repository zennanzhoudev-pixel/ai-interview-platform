// 招聘工作台与管理台。
//
// 这是"面试之外"的部分: 职位、候选人、管道、题库、排期、报告中心、
// 密钥与审计。它存在的理由很直接 —— 只有面试功能的系统不能叫招聘中台,
// 面试结果必须回流到流程里, 否则面试官拿到报告后还要手工记到别处。
import {
  el, clear, api, store, toast, navigate, confirmDialog, fmtDate, fmtDay,
  fmtBytes, competencyLabel, pipelineLabel, recommendationLabel, levelClass,
  PIPELINE_ORDER, stageLabel,
} from './core.js';
import { renderReport, downloadJSON, printReport } from './report.js';

const consoleState = {
  ready: false,
  jobs: [],
  candidates: [],
  applications: [],
  questions: [],
  schedules: [],
  sessions: [],
  analytics: null,
  knowledge: null,
  providers: null,
  me: null,
};

export function ensureConsoleKey() {
  const key = store.get('apiKey', '');
  if (key) api.key = key;
  return key;
}

// renderKeyGate 让用户填入 API Key。
//
// 不把密钥硬编码在前端: 它是租户级凭据, 一旦写进静态资源, 任何
// 打开页面的人都拿到了整个租户的数据权限。
export function renderKeyGate(root) {
  clear(root);
  const input = el('input', { type: 'password', placeholder: '粘贴 API Key (仅在创建时显示一次)' });
  const status = el('p', { class: 'muted small', text: '密钥只保存在你自己的浏览器里, 不会上传到任何第三方。' });
  root.append(el('article', { class: 'card narrow center' }, [
    el('p', { class: 'eyebrow', text: '招聘工作台' }),
    el('h1', { class: 'display small', text: '用企业密钥连接工作台' }),
    el('p', {
      class: 'lede',
      text: '服务启动时会打印一个引导密钥(或由管理员在"系统"页创建)。'
        + '密钥只以哈希形式存储, 明文仅显示一次。',
    }),
    input,
    status,
    el('div', { class: 'actions' }, [
      el('button', {
        class: 'btn primary wide',
        text: '连接',
        onclick: async () => {
          const value = input.value.trim();
          if (!value) return;
          api.key = value;
          try {
            await api.get('/api/v1/jobs');
            store.set('apiKey', value);
            toast('已连接工作台');
            navigate('/console/dashboard');
          } catch (err) {
            api.key = null;
            status.textContent = err.message;
            status.className = 'muted small error';
          }
        },
      }),
      el('button', {
        class: 'btn ghost',
        text: '用演示密钥(仅本地)',
        onclick: async () => {
          // 演示模式(未开启鉴权)下任何请求都能通过, 因此留一个快捷入口,
          // 但它不会写入任何密钥 —— 免得有人以为"这就是登录"。
          api.key = null;
          try {
            await api.get('/api/v1/jobs');
            store.remove('apiKey');
            toast('已连接到本地演示服务(未开启鉴权)');
            navigate('/console/dashboard');
          } catch (err) {
            status.textContent = err.message;
            status.className = 'muted small error';
          }
        },
      }),
    ]),
  ]));
}

export function disconnectConsole() {
  store.remove('apiKey');
  api.key = null;
  consoleState.ready = false;
  navigate('/console');
}

// loadAll 拉取工作台需要的全部数据。
//
// 并发拉取而不是串行: 看板要同时展示职位、管道、题库与统计, 串行往返
// 会让首屏慢到用户以为打不开。任何一个接口失败都不阻断整体 ——
// 权限不足的接口(例如审计)应当只是那一块空着, 而不是整页白屏。
async function loadAll() {
  const tasks = [
    api.get('/api/v1/jobs').then((d) => { consoleState.jobs = d.jobs || []; }).catch(() => {}),
    api.get('/api/v1/candidates?limit=200').then((d) => { consoleState.candidates = d.candidates || []; }).catch(() => {}),
    api.get('/api/v1/applications').then((d) => { consoleState.applications = d.applications || []; }).catch(() => {}),
    api.get('/api/v1/schedules').then((d) => { consoleState.schedules = d.schedules || []; }).catch(() => {}),
    api.get('/api/v1/sessions?limit=100').then((d) => { consoleState.sessions = d.sessions || []; }).catch(() => {}),
    api.get('/api/v1/analytics/pipeline').then((d) => { consoleState.analytics = d; }).catch(() => {}),
    api.get('/api/v1/knowledge/stats').then((d) => { consoleState.knowledge = d; }).catch(() => {}),
  ];
  await Promise.all(tasks);
  consoleState.ready = true;
}

function pageHead(title, lede, actions = []) {
  return el('div', { class: 'view-head' }, [
    el('div', {}, [
      el('p', { class: 'eyebrow', text: '招聘工作台' }),
      el('h1', { class: 'display', text: title }),
      lede ? el('p', { class: 'lede', text: lede }) : null,
    ]),
    actions.length ? el('div', { class: 'head-actions' }, actions) : null,
  ]);
}

function loading(root) {
  clear(root);
  root.append(el('p', { class: 'muted', text: '正在加载…' }));
}

/* ---------------- 看板 ---------------- */

export async function renderDashboard(root) {
  loading(root);
  await loadAll();
  clear(root);
  const a = consoleState.analytics || {};
  const byStage = a.by_stage || {};
  const funnel = a.round_funnel || {};

  const totalApps = consoleState.applications.length;
  const inAi = byStage.ai_interview || 0;
  const finished = (consoleState.sessions || []).filter((s) => s.status === 'finished').length;
  const openJobs = (consoleState.jobs || []).filter((j) => j.status === 'open').length;

  root.append(
    pageHead('招聘看板', 'AI 面试只是流程的一环。这里看到的是职位、管道与面试结论的整体状态。', [
      el('button', { class: 'btn outline', text: '刷新', onclick: () => renderDashboard(root) }),
    ]),
    el('section', { class: 'metric-grid' }, [
      metric('在招职位', openJobs, `${consoleState.jobs.length} 个职位`),
      metric('在流程候选人', totalApps, `${inAi} 人处于 AI 面试阶段`),
      metric('已完成面试', finished, `${(consoleState.sessions || []).length} 场会话`),
      metric('平均轮次分', (a.avg_round_score || 0).toFixed(1), `${a.scored_rounds || 0} 个已评分轮次`),
    ]),
  );

  const funnelCard = el('article', { class: 'card' }, [
    el('h2', { text: '管道漏斗' }),
    el('p', { class: 'muted small', text: '每一格是当前处于该阶段的候选人数。淘汰与退出单列, 不混进主漏斗。' }),
    el('div', { class: 'funnel' }, PIPELINE_ORDER.map((stage) => {
      const count = byStage[stage] || 0;
      const pct = totalApps > 0 ? Math.round((count / totalApps) * 100) : 0;
      return el('div', { class: 'funnel-row' }, [
        el('span', { class: 'funnel-label', text: pipelineLabel(stage) }),
        el('div', { class: 'bar' }, [el('i', { style: { width: `${pct}%` } })]),
        el('span', { class: 'muted small', text: `${count}` }),
      ]);
    })),
  ]);
  root.append(funnelCard);

  const roundsCard = el('article', { class: 'card' }, [
    el('h2', { text: '各轮次进度' }),
    el('p', { class: 'muted small', text: '"进行中"表示会话已创建但未结束 —— 这个数字持续偏高, 通常意味着候选人卡在了设备或授权环节。' }),
    el('table', { class: 'doc-table' }, [
      el('thead', {}, [el('tr', {}, [
        el('th', { text: '轮次' }), el('th', { text: '待开始' }), el('th', { text: '已排期' }),
        el('th', { text: '进行中' }), el('th', { text: '已完成' }),
      ])]),
      el('tbody', {}, [1, 2, 3, 4, 5].map((n) => {
        const row = funnel[n] || {};
        return el('tr', {}, [
          el('td', { text: `第 ${n} 轮` }),
          el('td', { text: String(row.pending || 0) }),
          el('td', { text: String(row.scheduled || 0) }),
          el('td', { text: String(row.running || 0) }),
          el('td', { text: String(row.finished || 0) }),
        ]);
      })),
    ]),
  ]);
  root.append(roundsCard);

  const k = consoleState.knowledge || {};
  if (k.ready) {
    root.append(el('article', { class: 'card' }, [
      el('h2', { text: '知识库(RAG 检索底座)' }),
      el('p', { class: 'muted small', text: '追问方向来自这里的检索结果, 而不是模型自由发挥。' }),
      el('dl', { class: 'facts' }, [
        fact('题目总数', String(k.questions || 0)),
        fact('参考要点', String(k.reference_points || 0)),
        fact('检索文档', String(k.documents || 0)),
        fact('嵌入模型', k.embedder || '—'),
        fact('检索链路', k.pipeline || '—'),
        fact('数据来源', k.source || '—'),
      ]),
      el('div', { class: 'actions' }, [
        el('button', { class: 'btn outline', text: '去题库与检索', onclick: () => navigate('/console/questions') }),
      ]),
    ]));
  }
}

function metric(label, value, hint) {
  return el('div', { class: 'metric' }, [
    el('span', { class: 'metric-label', text: label }),
    el('strong', { class: 'metric-value', text: String(value) }),
    el('span', { class: 'muted small', text: hint }),
  ]);
}

function fact(label, value) {
  return el('div', {}, [el('dt', { text: label }), el('dd', { text: value })]);
}

/* ---------------- 职位 ---------------- */

export async function renderJobs(root) {
  loading(root);
  await loadAll();
  clear(root);
  root.append(
    pageHead('职位与轮次编排', '1 到 5 轮怎么跑由职位决定: 哪一轮 AI 主导、哪一轮需要真人、哪一轮是编程轮。', [
      el('button', { class: 'btn primary', text: '新建职位', onclick: () => jobEditor(root, null) }),
    ]),
  );
  const grid = el('div', { class: 'card-grid' });
  if (consoleState.jobs.length === 0) {
    grid.append(el('p', { class: 'muted', text: '还没有职位。先创建一个, 之后候选人才能投递并进入流程。' }));
  }
  consoleState.jobs.forEach((job) => {
    grid.append(el('article', { class: 'card' }, [
      el('div', { class: 'card-head' }, [
        el('span', { class: `pill ${job.status === 'open' ? 'ok' : 'neutral'}`, text: jobStatusLabel(job.status) }),
        el('div', {}, [
          el('h3', { text: job.title }),
          el('p', { class: 'muted small', text: `${job.department || '—'} · ${job.level || '—'} · ${job.location || '不限'} · 招聘 ${job.headcount} 人` }),
        ]),
      ]),
      el('ol', { class: 'round-list' }, (job.rounds || []).map((r) => el('li', {}, [
        el('strong', { text: r.name || `第 ${r.round} 轮` }),
        el('span', { class: 'muted small', text: `${r.minutes} 分钟 · ${roundModeLabel(r.mode)}${r.ai_lead ? ' · AI 主导' : ' · 真人主导'}${r.human_panel ? ' · 需真人到场' : ''}` }),
      ]))),
      el('div', { class: 'actions' }, [
        el('button', { class: 'btn outline small', text: '编辑', onclick: () => jobEditor(root, job) }),
        el('button', {
          class: 'btn outline small',
          text: job.status === 'open' ? '暂停招聘' : '开放招聘',
          onclick: async () => {
            try {
              await api.patch(`/api/v1/jobs/${job.id || job.job_id}`, { status: job.status === 'open' ? 'paused' : 'open' });
              toast('已更新职位状态');
              renderJobs(root);
            } catch (err) {
              toast(err.message, 'error');
            }
          },
        }),
      ]),
    ]));
  });
  root.append(grid);
}

function jobStatusLabel(status) {
  return { draft: '草稿', open: '在招', paused: '暂停', closed: '已关闭' }[status] || status;
}

function roundModeLabel(mode) {
  return { video: '视频', audio: '语音', text: '文字', coding: '编程' }[mode] || mode;
}

// jobEditor 编辑职位与它的 1..5 轮编排。
function jobEditor(root, job) {
  const isNew = !job;
  const model = {
    title: job?.title || '',
    department: job?.department || '',
    level: job?.level || '',
    location: job?.location || '',
    headcount: job?.headcount || 1,
    status: job?.status || 'open',
    owner: job?.owner || '',
    competency_model: job?.competency_model || { project_depth: 20, language_core: 20, architecture: 30, distributed_system: 30 },
    rounds: job?.rounds ? JSON.parse(JSON.stringify(job.rounds)) : [],
  };

  const overlay = el('div', { class: 'modal' });
  const close = () => overlay.remove();
  const fields = el('div', { class: 'form-grid' });
  const add = (label, key) => {
    const input = el('input', { value: model[key] });
    input.addEventListener('input', () => { model[key] = input.value; });
    fields.append(el('label', {}, [label, input]));
  };
  add('职位名称', 'title');
  add('部门', 'department');
  add('职级', 'level');
  add('地点', 'location');
  add('招聘人数', 'headcount');
  add('负责人', 'owner');

  const statusSelect = el('select', {});
  ['draft', 'open', 'paused', 'closed'].forEach((s) => {
    statusSelect.append(el('option', { value: s, text: jobStatusLabel(s), selected: model.status === s }));
  });
  statusSelect.onchange = () => { model.status = statusSelect.value; };
  fields.append(el('label', {}, ['状态', statusSelect]));

  const roundRows = el('div', { class: 'round-editor' });
  for (let i = 1; i <= 5; i += 1) {
    const existing = model.rounds.find((r) => r.round === i) || { round: i, name: `第 ${i} 轮`, minutes: 45, mode: 'video', focus: [] };
    const name = el('input', { value: existing.name || '' });
    const minutes = el('input', { type: 'number', value: existing.minutes || 45 });
    const mode = el('select', {});
    ['video', 'audio', 'text', 'coding'].forEach((m) => {
      mode.append(el('option', { value: m, text: roundModeLabel(m), selected: existing.mode === m }));
    });
    const aiLead = el('input', { type: 'checkbox', checked: Boolean(existing.ai_lead) });
    const human = el('input', { type: 'checkbox', checked: Boolean(existing.human_panel) });
    const sync = () => {
      const next = {
        round: i, name: name.value, minutes: Number(minutes.value) || 45,
        mode: mode.value, ai_lead: aiLead.checked, human_panel: human.checked,
        focus: existing.focus || [],
      };
      const idx = model.rounds.findIndex((r) => r.round === i);
      if (idx >= 0) model.rounds[idx] = next;
      else model.rounds.push(next);
    };
    [name, minutes, mode, aiLead, human].forEach((node) => node.addEventListener('change', sync));
    sync();
    roundRows.append(el('div', { class: 'round-row' }, [
      el('span', { class: 'mono small', text: `R${i}` }),
      name, minutes, mode,
      el('label', { class: 'inline-check' }, [aiLead, 'AI 主导']),
      el('label', { class: 'inline-check' }, [human, '需真人到场']),
    ]));
  }

  const status = el('p', { class: 'muted small' });
  overlay.append(
    el('div', { class: 'modal-scrim', onclick: close }),
    el('div', { class: 'modal-card', role: 'dialog', 'aria-modal': 'true' }, [
      el('div', { class: 'modal-body' }, [
        el('p', { class: 'eyebrow', text: isNew ? '新建职位' : '编辑职位' }),
        el('h2', { class: 'display small', text: model.title || '未命名职位' }),
        fields,
        el('h3', { class: 'section-title', text: '轮次编排' }),
        el('p', { class: 'muted small', text: '没填的轮次会用默认值补齐(一面视频技术面、二面编程、三面起需要真人)。' }),
        roundRows,
        status,
        el('div', { class: 'actions' }, [
          el('button', {
            class: 'btn primary',
            text: isNew ? '创建' : '保存',
            onclick: async () => {
              if (!model.title.trim()) {
                status.textContent = '职位名称不能为空';
                status.className = 'muted small error';
                return;
              }
              try {
                const payload = { ...model, headcount: Number(model.headcount) || 1 };
                if (isNew) await api.post('/api/v1/jobs', payload);
                else await api.patch(`/api/v1/jobs/${job.id || job.job_id}`, payload);
                toast(isNew ? '职位已创建' : '职位已更新');
                close();
                renderJobs(root);
              } catch (err) {
                status.textContent = err.message;
                status.className = 'muted small error';
              }
            },
          }),
          el('button', { class: 'btn outline', text: '取消', onclick: close }),
        ]),
      ]),
    ]),
  );
  document.body.append(overlay);
}

/* ---------------- 系统: 自检 / 密钥 / 审计 / 数据权利 ---------------- */

export async function renderSystem(root) {
  loading(root);
  clear(root);
  root.append(pageHead('系统与治理', '这里回答三个运维最常问的问题: 连上了什么、谁动过数据、怎么行使删除权。'));

  const providerCard = el('article', { class: 'card' }, [
    el('h2', { text: '上游能力自检' }),
    el('p', {
      class: 'muted small',
      text: '默认只报告"是否配置"; 点"真实探测"会真的发一次请求 —— 它花时间也花钱, 因此不挂在探针上被每分钟调用。',
    }),
  ]);
  const providerBody = el('div', { class: 'provider-list', text: '加载中…' });
  const loadProviders = async (live) => {
    providerBody.textContent = live ? '正在真实探测上游…' : '加载中…';
    try {
      const data = await api.get(`/api/v1/system/providers${live ? '?live=1' : ''}`);
      clear(providerBody);
      (data.providers || []).forEach((p) => {
        providerBody.append(el('div', { class: 'provider-row' }, [
          el('span', { class: `pill ${stateClass(p.state)}`, text: stateLabel(p.state) }),
          el('div', {}, [
            el('strong', { text: p.label }),
            el('p', { class: 'muted small', text: providerNote(p) }),
          ]),
        ]));
      });
      const sandbox = data.sandbox || {};
      providerBody.append(el('div', { class: 'provider-row' }, [
        el('span', { class: `pill ${sandbox.isolated ? 'ok' : 'warn'}`, text: sandbox.isolated ? '已隔离' : '未隔离' }),
        el('div', {}, [
          el('strong', { text: '编程判题沙箱' }),
          el('p', { class: 'muted small', text: sandbox.configured ? `${sandbox.engine}${sandbox.note ? ` · ${sandbox.note}` : ''}` : '未配置, 判题接口会返回 503' }),
        ]),
      ]));
      const rec = data.recording || {};
      providerBody.append(el('div', { class: 'provider-row' }, [
        el('span', { class: `pill ${rec.configured ? 'ok' : 'neutral'}`, text: rec.configured ? '已启用' : '未启用' }),
        el('div', {}, [
          el('strong', { text: '面试录制存储' }),
          el('p', { class: 'muted small', text: rec.configured ? `保留 ${rec.retention_days} 天` : '未启用, 候选人界面不会出现录制入口' }),
        ]),
      ]));
      const storeInfo = data.store || {};
      providerBody.append(el('div', { class: 'provider-row' }, [
        el('span', { class: 'pill neutral', text: '存储' }),
        el('div', {}, [
          el('strong', { text: '业务主数据' }),
          el('p', { class: 'muted small', text: storeInfo.kind || '—' }),
        ]),
      ]));
    } catch (err) {
      providerBody.textContent = err.message;
    }
  };
  providerCard.append(providerBody, el('div', { class: 'actions' }, [
    el('button', { class: 'btn outline', text: '刷新', onclick: () => loadProviders(false) }),
    el('button', { class: 'btn primary', text: '真实探测', onclick: () => loadProviders(true) }),
  ]));
  root.append(providerCard);
  loadProviders(false);

  // 密钥管理。明文只在创建时显示一次 —— 这一点必须在界面上说清楚。
  const keyCard = el('article', { class: 'card' }, [
    el('h2', { text: 'API 密钥' }),
    el('p', { class: 'muted small', text: '密钥只以 sha256 存储。明文只在创建时显示一次, 之后无法找回, 只能吊销重建。' }),
  ]);
  const keyBody = el('div', { class: 'provider-list', text: '加载中…' });
  const loadKeys = async () => {
    try {
      const data = await api.get('/api/v1/keys');
      clear(keyBody);
      const keys = data.keys || [];
      if (keys.length === 0) keyBody.append(el('p', { class: 'muted small', text: '还没有密钥。' }));
      keys.forEach((k) => {
        keyBody.append(el('div', { class: 'provider-row' }, [
          el('span', { class: `pill ${k.revoked_at ? 'neutral' : 'ok'}`, text: k.revoked_at ? '已吊销' : '生效中' }),
          el('div', {}, [
            el('strong', { text: `${k.name || k.key_id} · ${k.role}` }),
            el('p', { class: 'muted small', text: `创建于 ${fmtDate(k.created_at)} · ${k.key_id}` }),
          ]),
          k.revoked_at ? null : el('button', {
            class: 'btn ghost small',
            text: '吊销',
            onclick: async () => {
              const ok = await confirmDialog({
                title: '吊销这个密钥?',
                body: '使用它的集成会立刻失效。这是一个不可逆操作。',
                confirmText: '吊销',
                danger: true,
              });
              if (!ok) return;
              try {
                await api.del(`/api/v1/keys/${k.key_id}`);
                toast('密钥已吊销');
                loadKeys();
              } catch (err) {
                toast(err.message, 'error');
              }
            },
          }),
        ]));
      });
    } catch (err) {
      keyBody.textContent = err.message;
    }
  };
  const roleSelect = el('select', {});
  [['admin', '管理员(全部权限)'], ['interviewer', '面试官(看报告/改分/排期)'], ['scheduler', '系统集成(仅排期与结论)']]
    .forEach(([value, label]) => roleSelect.append(el('option', { value, text: label })));
  const keyName = el('input', { placeholder: '用途备注, 例如 "生产 ATS 集成"' });
  keyCard.append(
    keyBody,
    el('div', { class: 'search-row' }, [keyName, roleSelect, el('button', {
      class: 'btn primary',
      text: '创建密钥',
      onclick: async () => {
        try {
          const created = await api.post('/api/v1/keys', {
            name: keyName.value || '未命名密钥',
            role: roleSelect.value,
          });
          const overlay = el('div', { class: 'modal' });
          const close = () => overlay.remove();
          overlay.append(
            el('div', { class: 'modal-scrim', onclick: close }),
            el('div', { class: 'modal-card narrow', role: 'dialog', 'aria-modal': 'true' }, [
              el('div', { class: 'modal-body' }, [
                el('p', { class: 'eyebrow', text: '新密钥' }),
                el('h2', { class: 'display small', text: '请立刻保存这个密钥' }),
                el('p', { class: 'muted', text: '关闭这个窗口后就再也看不到明文了。' }),
                el('input', { value: created.api_key || created.key || '', readonly: true, onclick: (e) => e.target.select() }),
                el('div', { class: 'actions' }, [
                  el('button', {
                    class: 'btn primary',
                    text: '复制',
                    onclick: async () => {
                      try {
                        await navigator.clipboard.writeText(created.api_key || created.key || '');
                        toast('已复制');
                      } catch {
                        toast('复制失败, 请手动选择', 'error');
                      }
                    },
                  }),
                  el('button', { class: 'btn outline', text: '我已保存', onclick: () => { close(); loadKeys(); } }),
                ]),
              ]),
            ]),
          );
          document.body.append(overlay);
        } catch (err) {
          toast(err.message, 'error');
        }
      },
    })]),
  );
  root.append(keyCard);
  loadKeys();

  // 数据主体权利: 导出与删除。
  const rightsCard = el('article', { class: 'card' }, [
    el('h2', { text: '数据主体权利' }),
    el('p', {
      class: 'muted small',
      text: '按候选人引用值导出或删除其全部数据。删除会同时清掉面试录像内容与元数据, 但审计日志会保留 —— '
        + '它是平台自身的合规证据, 不属于候选人数据。',
    }),
  ]);
  const refInput = el('input', { placeholder: 'candidate_ref (候选人详情页可复制)' });
  rightsCard.append(el('div', { class: 'search-row' }, [
    refInput,
    el('button', {
      class: 'btn outline',
      text: '导出',
      onclick: async () => {
        if (!refInput.value.trim()) return;
        try {
          const data = await api.get(`/api/v1/candidates/${encodeURIComponent(refInput.value.trim())}/export`);
          downloadJSON(`${refInput.value.trim()}-export.json`, data);
          toast('导出完成');
        } catch (err) {
          toast(err.message, 'error');
        }
      },
    }),
    el('button', {
      class: 'btn danger',
      text: '删除',
      onclick: async () => {
        const ref = refInput.value.trim();
        if (!ref) return;
        const ok = await confirmDialog({
          title: `删除候选人 ${ref} 的全部数据?`,
          body: '会话、问答记录、报告、授权记录与面试录像都会被删除, 且不可恢复。审计日志会保留删除这件事本身。',
          confirmText: '永久删除',
          danger: true,
        });
        if (!ok) return;
        try {
          const result = await api.del(`/api/v1/candidates/${encodeURIComponent(ref)}`);
          toast(`已删除 ${result.sessions ?? 0} 场会话`);
        } catch (err) {
          toast(err.message, 'error');
        }
      },
    }),
  ]));
  root.append(rightsCard);

  const auditCard = el('article', { class: 'card' }, [
    el('h2', { text: '审计日志' }),
    el('p', { class: 'muted small', text: '只追加, 不删除。面试创建、报告查看、改分、密钥操作都在这里。' }),
  ]);
  const auditBody = el('div', { text: '加载中…' });
  auditCard.append(auditBody, el('div', { class: 'actions' }, [
    el('button', {
      class: 'btn outline',
      text: '加载最近 100 条',
      onclick: async () => {
        try {
          const data = await api.get('/api/v1/audit?limit=100');
          clear(auditBody);
          const entries = data.entries || [];
          if (entries.length === 0) {
            auditBody.append(el('p', { class: 'muted small', text: '还没有审计记录。' }));
            return;
          }
          auditBody.append(el('table', { class: 'doc-table' }, [
            el('thead', {}, [el('tr', {}, [
              el('th', { text: '时间' }), el('th', { text: '操作' }), el('th', { text: '主体' }),
              el('th', { text: '对象' }), el('th', { text: '详情' }),
            ])]),
            el('tbody', {}, entries.map((e) => el('tr', {}, [
              el('td', { class: 'muted small', text: fmtDate(e.created_at) }),
              el('td', { class: 'mono small', text: e.action }),
              el('td', { class: 'muted small', text: e.actor }),
              el('td', { class: 'mono small', text: e.target || '' }),
              el('td', { class: 'muted small', text: e.detail || '' }),
            ]))),
          ]));
        } catch (err) {
          auditBody.textContent = err.message;
        }
      },
    }),
  ]));
  root.append(auditCard);
}

function stateLabel(state) {
  return {
    ok: '正常', error: '失败', configured: '已配置', not_configured: '未配置',
    configured_unverified: '待真实验证',
  }[state] || state;
}

function stateClass(state) {
  return { ok: 'ok', error: 'warn', configured: 'mint', not_configured: 'neutral', configured_unverified: 'warn' }[state] || 'neutral';
}

function providerNote(p) {
  if (p.state === 'not_configured') return p.note || '未配置, 已按降级策略运行';
  if (p.state === 'error') return `探测失败: ${p.error || ''}`;
  if (p.state === 'configured_unverified') return p.note || '已配置, 需要在真实面试中验证';
  if (p.latency_ms !== undefined) return `探测通过, 耗时 ${p.latency_ms}ms`;
  return '已配置';
}

/* ---------------- 报告中心 ---------------- */

export async function renderReports(root) {
  loading(root);
  await loadAll();
  clear(root);
  root.append(pageHead('报告中心', '报告里的每个结论都能回到候选人原话。这里的改分不会覆盖 AI 的原始结论, 两者都会留在审计日志里。'));

  const table = el('table', { class: 'doc-table' });
  table.append(el('thead', {}, [el('tr', {}, [
    el('th', { text: '候选人' }), el('th', { text: '岗位' }), el('th', { text: '轮次' }),
    el('th', { text: '状态' }), el('th', { text: '结论' }), el('th', { text: '时间' }), el('th', { text: '操作' }),
  ])]));
  const body = el('tbody', {});
  if (consoleState.sessions.length === 0) {
    body.append(el('tr', {}, [el('td', { colspan: 7, class: 'muted', text: '还没有面试会话。' })]));
  }
  consoleState.sessions.forEach((s) => {
    body.append(el('tr', {}, [
      el('td', { text: s.candidate_name || '—' }),
      el('td', { text: s.position || '—' }),
      el('td', { text: `第 ${s.round} 轮` }),
      el('td', {}, [el('span', { class: `pill ${s.status === 'finished' ? 'mint' : 'live'}`, text: s.status === 'finished' ? '已完成' : '进行中' })]),
      el('td', { text: s.recommendation ? recommendationLabel(s.recommendation) : '—' }),
      el('td', { class: 'muted small', text: fmtDate(s.created_at) }),
      el('td', {}, [el('button', {
        class: 'btn outline small',
        text: '查看报告',
        onclick: () => navigate(`/console/reports/${s.session_id}`),
      })]),
    ]));
  });
  table.append(body);
  root.append(el('article', { class: 'card' }, [table]));
}

export async function renderReportDetail(root, sessionId) {
  loading(root);
  let session;
  let report;
  try {
    const detail = await api.get(`/api/v1/sessions/${sessionId}`);
    session = detail.session;
    const list = await api.get(`/api/v1/sessions/${sessionId}/consents`).catch(() => ({ consents: [] }));
    const recordings = await api.get(`/api/v1/sessions/${sessionId}/recordings`).catch(() => ({ recordings: [] }));
    const audit = await api.get('/api/v1/audit?limit=200').catch(() => ({ entries: [] }));
    if (session.status === 'finished') {
      report = await api.get(`/api/v1/sessions/${sessionId}/report`).catch(() => null);
    }
    clear(root);
    root.append(el('div', { class: 'subbar' }, [
      el('button', { class: 'btn outline small', text: '返回报告中心', onclick: () => renderReports(root) }),
      el('span', { class: 'muted mono small', text: sessionId }),
      el('span', { class: `pill ${report ? 'mint' : 'warn'}`, text: report ? '报告可用' : '尚未生成' }),
    ]));
    root.append(pageHead(
      `${session.candidate_name || '候选人'} · ${session.position || ''}`,
      `${session.company || ''} · 第 ${session.round} 轮 · ${fmtDate(session.created_at)}`,
    ));

    if (!report) {
      root.append(el('article', { class: 'card' }, [
        el('h2', { text: '这场面试还没有结束' }),
        el('p', { class: 'muted', text: '报告会在最后一道题作答完成后生成。进行中的会话可以从面试安排页进入旁听席。' }),
        el('div', { class: 'actions' }, [
          el('button', {
            class: 'btn primary',
            text: '以面试官身份进入旁听席',
            onclick: async () => {
              try {
                const ticket = await api.post(`/api/v1/sessions/${sessionId}/observer-ticket`, {});
                sessionStorage.setItem('observerTicket', JSON.stringify({
                  sessionId, ticket: ticket.ticket, iceServers: ticket.ice_servers,
                }));
                navigate(`/observer/${sessionId}`);
              } catch (err) {
                toast(err.message, 'error');
              }
            },
          }),
        ]),
      ]));
    } else {
      const body = el('div', { class: 'report-body' });
      root.append(body);
      renderReport(body, report, {
        actions: [
          el('button', { class: 'btn primary', text: '人工改分', onclick: () => overrideDialog(root, sessionId, report) }),
          el('button', { class: 'btn outline', text: '打印 / 导出 PDF', onclick: () => printReport(`面试报告-${sessionId}`) }),
          el('button', {
            class: 'btn outline',
            text: '导出报告 JSON',
            onclick: () => downloadJSON(`${sessionId}.json`, { session, report, consents: list.consents }),
          }),
        ],
      });

      // 录像回放: 需要 recording:read 权限。面试官角色默认看不到。
      const recordingInfo = await api.get(`/api/v1/sessions/${sessionId}/recordings`).catch(() => null);
      if (recordingInfo) {
        const card = el('article', { class: 'card' }, [
          el('h2', { text: '面试录像' }),
          el('p', { class: 'muted small', text: `保留 ${recordingInfo.retention_days || 90} 天, 到期自动清理。播放行为会写入审计日志。` }),
        ]);
        const recordingList = recordingInfo.recordings || [];
        if (recordingList.length === 0) {
          card.append(el('p', { class: 'muted small', text: '本场没有录像(候选人可能未授权录制, 或浏览器不支持录制)。' }));
        }
        recordingList.forEach((rec) => {
          const url = `/api/v1/sessions/${sessionId}/recordings/${rec.kind}`;
          card.append(el('div', { class: 'recording-row' }, [
            el('div', {}, [
              el('strong', { text: rec.kind === 'video' ? '视频' : rec.kind === 'screen' ? '屏幕' : '音频' }),
              el('p', { class: 'muted small', text: `${fmtBytes(rec.size_bytes)} · 用时 ${fmtDuration((rec.duration_ms || 0) / 1000)} · ${rec.status}` }),
            ]),
            el('div', { class: 'actions tight' }, [
              el('button', {
                class: 'btn outline small',
                text: '播放',
                onclick: async () => {
                  // 视频用 fetch + blob 播放: 直接给 <video src> 不会带
                  // Authorization 头, 而录像接口需要鉴权。
                  try {
                    const resp = await api.request('GET', url, { raw: true });
                    if (!resp.ok) throw new Error(`播放失败(${resp.status})`);
                    const blob = await resp.blob();
                    const player = el('video', { controls: true, src: URL.createObjectURL(blob), class: 'recording-player' });
                    card.append(player);
                  } catch (err) {
                    toast(err.message, 'error');
                  }
                },
              }),
              el('button', {
                class: 'btn ghost small',
                text: '删除',
                onclick: async () => {
                  const ok = await confirmDialog({
                    title: '删除这段录像?',
                    body: '内容与元数据会同时删除, 且不可恢复。删除动作会写入审计日志。',
                    confirmText: '删除',
                    danger: true,
                  });
                  if (!ok) return;
                  try {
                    await api.del(url);
                    toast('录像已删除');
                    renderReportDetail(root, sessionId);
                  } catch (err) {
                    toast(err.message, 'error');
                  }
                },
              }),
            ]),
          ]));
        });
        root.append(card);
      }
    }

    root.append(el('article', { class: 'card' }, [
      el('h2', { text: '授权留痕' }),
      el('p', { class: 'muted small', text: '谁在什么时候同意了哪一项处理。' }),
      el('table', { class: 'doc-table' }, [
        el('thead', {}, [el('tr', {}, [
          el('th', { text: '范围' }), el('th', { text: '时间' }), el('th', { text: '来源网段' }), el('th', { text: 'User-Agent' }),
        ])]),
        el('tbody', {}, (list.consents || []).map((c) => el('tr', {}, [
          el('td', { text: c.scope === 'recording' ? '录音录像' : 'AI 评分' }),
          el('td', { text: fmtDate(c.agreed_at) }),
          el('td', { class: 'mono small', text: c.ip || '—' }),
          el('td', { class: 'muted small', text: c.user_agent || '—' }),
        ]))),
      ]),
    ]));

    const trail = (audit.entries || []).filter((e) => e.target === sessionId);
    root.append(el('article', { class: 'card' }, [
      el('h2', { text: '本场操作轨迹' }),
      el('p', { class: 'muted small', text: '面试创建、报告查看、改分、录像播放都会留痕。' }),
      trail.length === 0
        ? el('p', { class: 'muted small', text: '暂无记录。' })
        : el('table', { class: 'doc-table' }, [
            el('thead', {}, [el('tr', {}, [
              el('th', { text: '时间' }), el('th', { text: '操作' }), el('th', { text: '主体' }), el('th', { text: '详情' }),
            ])]),
            el('tbody', {}, trail.map((e) => el('tr', {}, [
              el('td', { class: 'muted small', text: fmtDate(e.created_at) }),
              el('td', { class: 'mono small', text: e.action }),
              el('td', { class: 'muted small', text: e.actor }),
              el('td', { class: 'muted small', text: e.detail || '' }),
            ]))),
          ]),
    ]));
  } catch (err) {
    clear(root);
    root.append(el('p', { class: 'muted', text: err.message }));
  }
}

// overrideDialog 是人工改分。
//
// 产品立场很明确: AI 给建议, 人做决定。因此这里需要填写理由,
// 且原始结论不会被抹掉 —— 两者都进审计日志, 便于事后复盘"当初为什么改"。
function overrideDialog(root, sessionId, report) {
  const overlay = el('div', { class: 'modal' });
  const close = () => overlay.remove();
  const select = el('select', {});
  ['STRONG_HIRE', 'HIRE', 'PASS_WITH_CONCERN', 'NO_HIRE'].forEach((value) => {
    select.append(el('option', {
      value, text: recommendationLabel(value), selected: report.recommendation === value,
    }));
  });
  const comment = el('textarea', { rows: 3, placeholder: '改分理由(会写入审计日志, 必填)' });
  const status = el('p', { class: 'muted small' });
  overlay.append(
    el('div', { class: 'modal-scrim', onclick: close }),
    el('div', { class: 'modal-card narrow', role: 'dialog', 'aria-modal': 'true' }, [
      el('div', { class: 'modal-body' }, [
        el('p', { class: 'eyebrow', text: '人工改分' }),
        el('h2', { class: 'display small', text: '以人的判断覆盖 AI 建议' }),
        el('p', {
          class: 'muted',
          text: `AI 原始结论: ${recommendationLabel(report.recommendation)} (置信度 ${((report.confidence || 0) * 100).toFixed(0)}%)。改分不会删除原始结论。`,
        }),
        el('label', {}, ['人工结论', select]),
        el('label', {}, ['理由', comment]),
        status,
        el('div', { class: 'actions' }, [
          el('button', {
            class: 'btn primary',
            text: '提交改分',
            onclick: async () => {
              if (!comment.value.trim()) {
                status.textContent = '请填写改分理由';
                status.className = 'muted small error';
                return;
              }
              try {
                await api.post(`/api/v1/sessions/${sessionId}/override`, {
                  recommendation: select.value,
                  comment: comment.value,
                });
                toast('人工结论已记录');
                close();
                renderReportDetail(root, sessionId);
              } catch (err) {
                status.textContent = err.message;
                status.className = 'muted small error';
              }
            },
          }),
          el('button', { class: 'btn outline', text: '取消', onclick: close }),
        ]),
      ]),
    ]),
  );
  document.body.append(overlay);
}

/* ---------------- 面试安排 ---------------- */

export async function renderSchedules(root, query = {}) {
  loading(root);
  await loadAll();
  clear(root);
  root.append(
    pageHead('面试安排', '排期 -> 开始面试 -> 结果回流。开始面试会生成候选人链接, 并强制候选人本人确认授权。', [
      el('button', { class: 'btn primary', text: '新建安排', onclick: () => scheduleEditor(root, query.application) }),
    ]),
  );

  if (consoleState.applications.length === 0) {
    root.append(el('article', { class: 'card' }, [
      el('h2', { text: '还没有可排期的投递' }),
      el('p', { class: 'muted', text: '先在"职位"里建一个职位, 在"候选人"里导入候选人, 再到"管道"里创建投递。' }),
      el('div', { class: 'actions' }, [
        el('button', { class: 'btn outline', text: '去职位', onclick: () => navigate('/console/jobs') }),
        el('button', { class: 'btn outline', text: '去候选人', onclick: () => navigate('/console/candidates') }),
      ]),
    ]));
    return;
  }

  const table = el('table', { class: 'doc-table' });
  table.append(el('thead', {}, [el('tr', {}, [
    el('th', { text: '时间' }), el('th', { text: '候选人' }), el('th', { text: '轮次' }),
    el('th', { text: '形式' }), el('th', { text: '面试官' }), el('th', { text: '状态' }), el('th', { text: '操作' }),
  ])]));
  const body = el('tbody', {});
  if (consoleState.schedules.length === 0) {
    body.append(el('tr', {}, [el('td', { colspan: 7, class: 'muted', text: '还没有安排。' })]));
  }
  consoleState.schedules.forEach((s) => {
    const app = consoleState.applications.find((a) => (a.application_id || a.id) === s.application_id) || {};
    body.append(el('tr', {}, [
      el('td', { text: fmtDate(s.scheduled_at) }),
      el('td', { text: app.candidate_name || s.candidate_ref || '—' }),
      el('td', { text: `第 ${s.round} 轮` }),
      el('td', { text: roundModeLabel(s.mode) }),
      el('td', { class: 'muted small', text: s.interviewer || '—' }),
      el('td', {}, [el('span', { class: `pill ${s.status === 'done' ? 'mint' : 'neutral'}`, text: scheduleStatusLabel(s.status) })]),
      el('td', {}, [
        s.session_id
          ? el('button', {
              class: 'btn outline small',
              text: '看报告',
              onclick: () => navigate(`/console/reports/${s.session_id}`),
            })
          : el('button', {
              class: 'btn primary small',
              text: '开始面试',
              onclick: () => startInterview(root, s),
            }),
      ]),
    ]));
  });
  table.append(body);
  root.append(el('article', { class: 'card' }, [table]));
}

function scheduleStatusLabel(status) {
  return {
    pending: '待确认', confirmed: '已确认', running: '进行中',
    done: '已完成', cancelled: '已取消', no_show: '未到场',
  }[status] || status;
}

function scheduleEditor(root, preselect) {
  const overlay = el('div', { class: 'modal' });
  const close = () => overlay.remove();
  const appSelect = el('select', {});
  consoleState.applications.forEach((a) => {
    const id = a.application_id || a.id;
    appSelect.append(el('option', {
      value: id,
      text: `${a.candidate_name || a.candidate_ref} · ${a.job_title || a.job_id}`,
      selected: id === preselect,
    }));
  });
  const roundInput = el('input', { type: 'number', value: 1, min: 1, max: 5 });
  const timeInput = el('input', { type: 'datetime-local' });
  const modeSelect = el('select', {});
  ['video', 'audio', 'text', 'coding'].forEach((m) => modeSelect.append(el('option', { value: m, text: roundModeLabel(m) })));
  const interviewer = el('input', { placeholder: '面试官(留空则用轮次默认名)' });
  const status = el('p', { class: 'muted small' });

  // 默认明天 10:00, 而不是"现在" —— 排一个已经过去的时间是最常见的
  // 误操作, 而它要到候选人点开链接时才会暴露。
  const tomorrow = new Date(Date.now() + 24 * 3600 * 1000);
  tomorrow.setHours(10, 0, 0, 0);
  timeInput.value = new Date(tomorrow.getTime() - tomorrow.getTimezoneOffset() * 60000)
    .toISOString().slice(0, 16);

  overlay.append(
    el('div', { class: 'modal-scrim', onclick: close }),
    el('div', { class: 'modal-card narrow', role: 'dialog', 'aria-modal': 'true' }, [
      el('div', { class: 'modal-body' }, [
        el('p', { class: 'eyebrow', text: '新建面试安排' }),
        el('h2', { class: 'display small', text: '安排一场面试' }),
        el('label', {}, ['投递', appSelect]),
        el('div', { class: 'form-grid' }, [
          el('label', {}, ['轮次', roundInput]),
          el('label', {}, ['形式', modeSelect]),
          el('label', {}, ['时间', timeInput]),
          el('label', {}, ['面试官', interviewer]),
        ]),
        status,
        el('div', { class: 'actions' }, [
          el('button', {
            class: 'btn primary',
            text: '创建安排',
            onclick: async () => {
              if (!timeInput.value) {
                status.textContent = '请选择面试时间';
                status.className = 'muted small error';
                return;
              }
              try {
                await api.post('/api/v1/schedules', {
                  application_id: appSelect.value,
                  round: Number(roundInput.value) || 1,
                  mode: modeSelect.value,
                  interviewer: interviewer.value,
                  scheduled_at: new Date(timeInput.value).toISOString(),
                });
                toast('安排已创建');
                close();
                renderSchedules(root);
              } catch (err) {
                status.textContent = err.message;
                status.className = 'muted small error';
              }
            },
          }),
          el('button', { class: 'btn outline', text: '取消', onclick: close }),
        ]),
      ]),
    ]),
  );
  document.body.append(overlay);
}

// startInterview 把安排变成真实会话, 并把候选人链接显示出来。
async function startInterview(root, schedule) {
  const id = schedule.schedule_id || schedule.id;
  try {
    const result = await api.post(`/api/v1/schedules/${id}/start`, {});
    const link = `${location.origin}${result.candidate_url}`;
    const overlay = el('div', { class: 'modal' });
    const close = () => overlay.remove();
    overlay.append(
      el('div', { class: 'modal-scrim', onclick: close }),
      el('div', { class: 'modal-card narrow', role: 'dialog', 'aria-modal': 'true' }, [
        el('div', { class: 'modal-body' }, [
          el('p', { class: 'eyebrow', text: '面试已开始' }),
          el('h2', { class: 'display small', text: '把链接发给候选人' }),
          el('p', {
            class: 'muted',
            text: '链接里的凭证只对这一场面试有效, 并在数小时后过期。候选人打开后需要先确认录音与 AI 评分授权。',
          }),
          el('input', { value: link, readonly: true, onclick: (e) => e.target.select() }),
          el('div', { class: 'actions' }, [
            el('button', {
              class: 'btn primary',
              text: '复制链接',
              onclick: async () => {
                try {
                  await navigator.clipboard.writeText(link);
                  toast('已复制');
                } catch {
                  toast('复制失败, 请手动选择文本', 'error');
                }
              },
            }),
            el('button', {
              class: 'btn outline',
              text: '进入旁听席',
              onclick: async () => {
                try {
                  const ticket = await api.post(`/api/v1/sessions/${result.session_id}/observer-ticket`, {});
                  // 票据放进 sessionStorage 而不是 URL: URL 会进地址栏、
                  // 进浏览器历史、进 Referer, 而票据是一张能进入面试间的凭证。
                  sessionStorage.setItem('observerTicket', JSON.stringify({
                    sessionId: result.session_id,
                    ticket: ticket.ticket,
                    expiresAt: ticket.expires_at,
                    iceServers: ticket.ice_servers,
                  }));
                  close();
                  navigate(`/observer/${result.session_id}`);
                } catch (err) {
                  toast(err.message, 'error');
                }
              },
            }),
            el('button', { class: 'btn ghost', text: '关闭', onclick: () => { close(); renderSchedules(root); } }),
          ]),
        ]),
      ]),
    );
    document.body.append(overlay);
  } catch (err) {
    toast(err.message, 'error');
  }
}

/* ---------------- 题库与检索 ---------------- */

export async function renderQuestions(root) {
  loading(root);
  let data;
  try {
    data = await api.get('/api/v1/questions');
  } catch (err) {
    clear(root);
    root.append(el('p', { class: 'muted', text: err.message }));
    return;
  }
  clear(root);
  const questions = data.questions || [];
  const stats = data.stats || {};
  root.append(
    pageHead('题库与检索', '同一份数据承担三件事: 提问、评分要点、检索语料。报告里的评分依据因此和实际问的问题永远对得上。', [
      el('button', { class: 'btn primary', text: '新建题目', onclick: () => questionEditor(root, null) }),
    ]),
  );

  const knowledgeCard = el('article', { class: 'card' }, [
    el('h2', { text: '检索底座' }),
    el('dl', { class: 'facts' }, [
      fact('题目', String(stats.questions || questions.length)),
      fact('参考要点', String(stats.reference_points || 0)),
      fact('检索文档', String(stats.documents || 0)),
      fact('嵌入模型', stats.embedder || '—'),
      fact('检索链路', stats.pipeline || 'BM25 + 向量 + RRF + 精排'),
      fact('数据来源', stats.source || '—'),
    ]),
  ]);
  if (Array.isArray(stats.missing_structural_stages) && stats.missing_structural_stages.length > 0) {
    knowledgeCard.append(el('div', { class: 'callout warn' }, [
      el('strong', { text: '流程骨架有缺口' }),
      el('p', { class: 'muted small', text: `以下阶段没有可用题目, 面试会跳过它们: ${stats.missing_structural_stages.join('、')}` }),
    ]));
  }
  root.append(knowledgeCard);

  const searchInput = el('input', { placeholder: '输入一句话, 看看 AI 会检索到什么(例如: 缓存和数据库不一致怎么处理)' });
  const searchResults = el('div', { class: 'search-results' });
  const searchButton = el('button', {
    class: 'btn primary',
    text: '检索',
    onclick: async () => {
      const query = searchInput.value.trim();
      if (!query) return;
      searchResults.textContent = '检索中…';
      try {
        const result = await api.get(`/api/v1/retrieval/search?q=${encodeURIComponent(query)}&top_k=8`);
        clear(searchResults);
        const hits = result.hits || [];
        if (hits.length === 0) {
          searchResults.append(el('p', { class: 'muted small', text: '没有命中。' }));
          return;
        }
        searchResults.append(el('p', {
          class: 'muted small',
          text: `命中 ${hits.length} 条 · 用时 ${result.elapsed_ms}ms · 链路 ${result.pipeline}`,
        }));
        hits.forEach((hit, i) => {
          searchResults.append(el('div', { class: 'hit' }, [
            el('div', { class: 'hit-head' }, [
              el('span', { class: 'pill neutral', text: `#${i + 1}` }),
              el('span', { class: 'mono small', text: hit.question_id || '' }),
              hit.point_key ? el('span', { class: 'pill mint', text: hit.point_key }) : null,
              el('span', { class: 'muted small', text: `score ${hit.score.toFixed(4)}` }),
            ]),
            el('p', { text: hit.text }),
          ]));
        });
      } catch (err) {
        searchResults.textContent = err.message;
      }
    },
  });
  root.append(el('article', { class: 'card' }, [
    el('h2', { text: '检索可视化' }),
    el('p', {
      class: 'muted small',
      text: '追问决策用的就是这个索引。如果这里的检索结果不相关, 那追问也不会好 —— 因此它值得被直接看到, 而不是留在黑盒里。',
    }),
    el('div', { class: 'search-row' }, [searchInput, searchButton]),
    searchResults,
  ]));

  const table = el('table', { class: 'doc-table' });
  table.append(el('thead', {}, [el('tr', {}, [
    el('th', { text: '题目' }), el('th', { text: '阶段' }), el('th', { text: '能力项' }),
    el('th', { text: '要点数' }), el('th', { text: '状态' }), el('th', { text: '来源' }), el('th', { text: '操作' }),
  ])]));
  const body = el('tbody', {});
  questions.forEach((q) => {
    const builtin = q.tenant_id === '';
    body.append(el('tr', {}, [
      el('td', { text: q.text }),
      el('td', { class: 'muted small', text: stageLabel(q.stage) }),
      el('td', { class: 'muted small', text: competencyLabel(q.competency) }),
      el('td', { text: String((q.reference_points || []).length) }),
      el('td', {}, [el('span', { class: `pill ${q.status === 'published' ? 'ok' : 'neutral'}`, text: questionStatusLabel(q.status) })]),
      el('td', { class: 'muted small', text: builtin ? '内置' : `v${q.version}` }),
      el('td', {}, [
        builtin
          ? el('span', { class: 'muted small', text: '只读' })
          : el('button', { class: 'btn outline small', text: '编辑', onclick: () => questionEditor(root, q) }),
      ]),
    ]));
  });
  table.append(body);
  root.append(el('article', { class: 'card' }, [
    el('h2', { text: '题目列表' }),
    el('p', { class: 'muted small', text: '内置题目保证流程骨架完整(开场、反问、收尾), 因此不可编辑; 你也可以用同名阶段的自定义题覆盖它。' }),
    table,
  ]));
}

function questionStatusLabel(status) {
  return { draft: '草稿', reviewing: '评审中', published: '已发布', retired: '已下线' }[status] || status;
}

function questionEditor(root, question) {
  const isNew = !question;
  const model = {
    text: question?.text || '',
    stage: question?.stage || 'TECH_FUNDAMENTAL',
    competency: question?.competency || 'language_core',
    keywordText: (question?.keywords || []).join(', '),
    importance: question?.importance || 'high',
    max_probe: question?.max_probe || 2,
    status: question?.status || 'published',
    points: (question?.reference_points || []).map((p) => ({ ...p })),
  };
  const overlay = el('div', { class: 'modal' });
  const close = () => overlay.remove();
  const status = el('p', { class: 'muted small' });

  const text = el('textarea', { rows: 3, value: model.text });
  text.addEventListener('input', () => { model.text = text.value; });
  const stage = el('select', {});
  ['GREETING', 'RESUME_DEEP_DIVE', 'TECH_FUNDAMENTAL', 'SCENARIO_DESIGN', 'CANDIDATE_QA', 'WRAP_UP']
    .forEach((s) => stage.append(el('option', { value: s, text: stageLabel(s), selected: model.stage === s })));
  stage.onchange = () => { model.stage = stage.value; };
  const competency = el('select', {});
  ['project_depth', 'tech_choice', 'language_core', 'distributed_system', 'architecture', 'coding', 'communication']
    .forEach((c) => competency.append(el('option', { value: c, text: competencyLabel(c), selected: model.competency === c })));
  competency.onchange = () => { model.competency = competency.value; };
  const keywords = el('input', { value: model.keywordText });
  keywords.addEventListener('input', () => { model.keywordText = keywords.value; });
  const importance = el('select', {});
  [['high', '高'], ['medium', '中'], ['low', '低']].forEach(([v, label]) => {
    importance.append(el('option', { value: v, text: label, selected: model.importance === v }));
  });
  importance.onchange = () => { model.importance = importance.value; };
  const maxProbe = el('input', { type: 'number', value: model.max_probe });
  maxProbe.addEventListener('input', () => { model.max_probe = Number(maxProbe.value) || 0; });
  const published = el('select', {});
  [['published', '已发布'], ['draft', '草稿'], ['reviewing', '评审中'], ['retired', '已下线']]
    .forEach(([v, label]) => published.append(el('option', { value: v, text: label, selected: model.status === v })));
  published.onchange = () => { model.status = published.value; };

  const pointList = el('div', { class: 'point-editor' });
  const renderPoints = () => {
    clear(pointList);
    model.points.forEach((point, index) => {
      const key = el('input', { value: point.key, placeholder: '要点名' });
      const value = el('input', { value: point.text, placeholder: '参考答案里的表述' });
      key.addEventListener('input', () => { point.key = key.value; });
      value.addEventListener('input', () => { point.text = value.value; });
      pointList.append(el('div', { class: 'point-row' }, [
        key, value,
        el('button', {
          class: 'btn ghost small',
          type: 'button',
          text: '删除',
          onclick: () => {
            model.points.splice(index, 1);
            renderPoints();
          },
        }),
      ]));
    });
  };
  renderPoints();

  overlay.append(
    el('div', { class: 'modal-scrim', onclick: close }),
    el('div', { class: 'modal-card', role: 'dialog', 'aria-modal': 'true' }, [
      el('div', { class: 'modal-body' }, [
        el('p', { class: 'eyebrow', text: isNew ? '新建题目' : `编辑题目 · 当前 v${question.version}` }),
        el('h2', { class: 'display small', text: '题目与参考答案要点' }),
        el('label', {}, ['题干', text]),
        el('div', { class: 'form-grid' }, [
          el('label', {}, ['阶段', stage]),
          el('label', {}, ['能力项', competency]),
          el('label', {}, ['判定要点(逗号分隔, 用于评分)', keywords]),
          el('label', {}, ['重要性', importance]),
          el('label', {}, ['最大追问深度', maxProbe]),
          el('label', {}, ['状态', published]),
        ]),
        el('h3', { class: 'section-title', text: '参考答案要点' }),
        el('p', {
          class: 'muted small',
          text: '这些要点会被拆成独立文档建索引, 用来判断"候选人漏了哪一点"并生成追问方向。'
            + '要点名与表述都必须填写 —— 只有名字无法生成追问, 只有表述无法做覆盖度去重。',
        }),
        pointList,
        el('button', {
          class: 'btn outline small',
          type: 'button',
          text: '+ 添加要点',
          onclick: () => {
            model.points.push({ key: '', text: '' });
            renderPoints();
          },
        }),
        status,
        el('div', { class: 'actions' }, [
          el('button', {
            class: 'btn primary',
            text: isNew ? '创建' : '保存(版本 +1)',
            onclick: async () => {
              const payload = {
                text: model.text,
                stage: model.stage,
                competency: model.competency,
                keywords: model.keywordText.split(/[,，]/).map((s) => s.trim()).filter(Boolean),
                importance: model.importance,
                max_probe: model.max_probe,
                status: model.status,
                reference_points: model.points,
              };
              try {
                if (isNew) await api.post('/api/v1/questions', payload);
                else await api.patch(`/api/v1/questions/${question.question_id}`, payload);
                toast(isNew ? '题目已创建' : '题目已更新, 知识库已重建');
                close();
                renderQuestions(root);
              } catch (err) {
                status.textContent = err.message;
                status.className = 'muted small error';
              }
            },
          }),
          !isNew ? el('button', {
            class: 'btn danger',
            text: '删除',
            onclick: async () => {
              const ok = await confirmDialog({
                title: '删除这道题?',
                body: '删除后它不再参与抽题与检索。已完成的面试报告不受影响(报告里保存了当时的评分依据)。',
                confirmText: '删除',
                danger: true,
              });
              if (!ok) return;
              try {
                await api.del(`/api/v1/questions/${question.question_id}`);
                toast('题目已删除');
                close();
                renderQuestions(root);
              } catch (err) {
                status.textContent = err.message;
                status.className = 'muted small error';
              }
            },
          }) : null,
          el('button', { class: 'btn outline', text: '取消', onclick: close }),
        ]),
      ]),
    ]),
  );
  document.body.append(overlay);
}

/* ---------------- 候选人 ---------------- */

export async function renderCandidates(root) {
  loading(root);
  await loadAll();
  clear(root);
  root.append(
    pageHead('候选人', '联系方式落库前已脱敏; 简历原文与结构化实体只在这个页面按需加载。', [
      el('button', { class: 'btn primary', text: '导入候选人', onclick: () => candidateEditor(root) }),
    ]),
  );
  const table = el('table', { class: 'doc-table' });
  table.append(
    el('thead', {}, [el('tr', {}, [
      el('th', { text: '姓名' }), el('th', { text: '邮箱' }), el('th', { text: '电话' }),
      el('th', { text: '来源' }), el('th', { text: '在流程中' }), el('th', { text: '操作' }),
    ])]),
  );
  const body = el('tbody', {});
  if (consoleState.candidates.length === 0) {
    body.append(el('tr', {}, [el('td', { colspan: 6, class: 'muted', text: '还没有候选人。' })]));
  }
  consoleState.candidates.forEach((c) => {
    const apps = consoleState.applications.filter((a) => a.candidate_ref === c.candidate_ref);
    body.append(el('tr', {}, [
      el('td', { text: c.name || '—' }),
      el('td', { class: 'muted small', text: c.email || '—' }),
      el('td', { class: 'muted small', text: c.phone || '—' }),
      el('td', { class: 'muted small', text: c.source || '—' }),
      el('td', { text: String(apps.length) }),
      el('td', {}, [el('button', {
        class: 'btn outline small',
        text: '查看',
        onclick: () => renderCandidateDetail(root, c.candidate_ref),
      })]),
    ]));
  });
  table.append(body);
  root.append(el('article', { class: 'card' }, [table]));
}

function candidateEditor(root) {
  const model = { candidate_id: '', name: '', email: '', phone: '', source: '内推', resume_text: '' };
  const overlay = el('div', { class: 'modal' });
  const close = () => overlay.remove();
  const fields = el('div', { class: 'form-grid' });
  [
    ['候选人 ID / 手机号 / 邮箱(用于生成假名引用值)', 'candidate_id'],
    ['姓名', 'name'],
    ['邮箱', 'email'],
    ['电话', 'phone'],
    ['来源渠道', 'source'],
  ].forEach(([label, key]) => {
    const input = el('input', { value: model[key] });
    input.addEventListener('input', () => { model[key] = input.value; });
    fields.append(el('label', {}, [label, input]));
  });
  const resume = el('textarea', { rows: 8, placeholder: '简历原文(可选, 会解析成带原文位置的实体)' });
  resume.addEventListener('input', () => { model.resume_text = resume.value; });
  const status = el('p', { class: 'muted small' });
  overlay.append(
    el('div', { class: 'modal-scrim', onclick: close }),
    el('div', { class: 'modal-card', role: 'dialog', 'aria-modal': 'true' }, [
      el('div', { class: 'modal-body' }, [
        el('p', { class: 'eyebrow', text: '导入候选人' }),
        el('h2', { class: 'display small', text: '候选人档案' }),
        el('p', { class: 'muted small', text: '重复导入同一人是幂等的: 系统会用引用值覆盖更新, 不会产生重复档案。' }),
        fields,
        el('label', {}, ['简历', resume]),
        status,
        el('div', { class: 'actions' }, [
          el('button', {
            class: 'btn primary',
            text: '保存',
            onclick: async () => {
              try {
                await api.post('/api/v1/candidates', model);
                toast('候选人已保存');
                close();
                renderCandidates(root);
              } catch (err) {
                status.textContent = err.message;
                status.className = 'muted small error';
              }
            },
          }),
          el('button', { class: 'btn outline', text: '取消', onclick: close }),
        ]),
      ]),
    ]),
  );
  document.body.append(overlay);
}

async function renderCandidateDetail(root, ref) {
  loading(root);
  let data;
  try {
    data = await api.get(`/api/v1/candidates/${encodeURIComponent(ref)}`);
  } catch (err) {
    clear(root);
    root.append(el('p', { class: 'muted', text: err.message }));
    return;
  }
  clear(root);
  const candidate = data.candidate || {};
  root.append(
    el('div', { class: 'subbar' }, [
      el('button', { class: 'btn outline small', text: '返回候选人列表', onclick: () => renderCandidates(root) }),
      el('span', { class: 'muted mono small', text: candidate.candidate_ref || ref }),
    ]),
    pageHead(candidate.name || '候选人', `${candidate.email || ''} ${candidate.phone || ''} · 来源 ${candidate.source || '—'}`),
  );

  const apps = data.applications || [];
  root.append(el('article', { class: 'card' }, [
    el('h2', { text: '流程' }),
    apps.length === 0
      ? el('p', { class: 'muted small', text: '这个人还没有进入任何职位流程。' })
      : el('table', { class: 'doc-table' }, [
          el('thead', {}, [el('tr', {}, [
            el('th', { text: '职位' }), el('th', { text: '阶段' }), el('th', { text: '当前轮次' }), el('th', { text: '操作' }),
          ])]),
          el('tbody', {}, apps.map((a) => el('tr', {}, [
            el('td', { text: a.job_title || a.job_id }),
            el('td', { text: pipelineLabel(a.stage) }),
            el('td', { text: `第 ${a.current_round || 1} 轮` }),
            el('td', {}, [el('button', {
              class: 'btn outline small',
              text: '去排期',
              onclick: () => navigate(`/console/schedules?application=${a.application_id || a.id}`),
            })]),
          ]))),
        ]),
  ]));

  const resume = data.resume;
  if (resume && Array.isArray(resume.entities)) {
    root.append(el('article', { class: 'card' }, [
      el('h2', { text: '简历实体与原文定位' }),
      el('p', { class: 'muted small', text: '每个实体都带原文偏移。追问命中时, 报告里会显示具体位置, 便于核对。' }),
      el('table', { class: 'doc-table' }, [
        el('thead', {}, [el('tr', {}, [
          el('th', { text: '类型' }), el('th', { text: '内容' }), el('th', { text: '原文位置' }),
        ])]),
        el('tbody', {}, resume.entities.map((ent) => el('tr', {}, [
          el('td', { text: { skill: '技能', project: '项目', timeline: '时间线' }[ent.kind] || ent.kind }),
          el('td', { text: ent.value }),
          el('td', { class: 'mono small', text: `第 ${ent.start}–${ent.end} 字` }),
        ]))),
      ]),
      data.resume_text ? el('details', {}, [
        el('summary', { text: '查看简历原文' }),
        el('pre', { class: 'resume-source', text: data.resume_text }),
      ]) : null,
    ]));
  }

  const sessions = data.sessions || [];
  root.append(el('article', { class: 'card' }, [
    el('h2', { text: '面试会话' }),
    sessions.length === 0
      ? el('p', { class: 'muted small', text: '还没有面试记录。' })
      : el('table', { class: 'doc-table' }, [
          el('thead', {}, [el('tr', {}, [
            el('th', { text: '会话' }), el('th', { text: '轮次' }), el('th', { text: '状态' }), el('th', { text: '结论' }), el('th', { text: '操作' }),
          ])]),
          el('tbody', {}, sessions.map((s) => el('tr', {}, [
            el('td', { class: 'mono small', text: s.session_id }),
            el('td', { text: `第 ${s.round} 轮` }),
            el('td', { text: s.status }),
            el('td', { text: recommendationLabel(s.recommendation) }),
            el('td', {}, [el('button', {
              class: 'btn outline small',
              text: '看报告',
              onclick: () => navigate(`/console/reports/${s.session_id}`),
            })]),
          ]))),
        ]),
  ]));
}

/* ---------------- 招聘管道 ---------------- */

export async function renderPipeline(root) {
  loading(root);
  await loadAll();
  clear(root);
  root.append(
    pageHead('招聘管道', '拖不动也没关系 —— 每张卡片上的阶段按钮就是入口。AI 只记录事实, 阶段由人推进。', [
      el('button', { class: 'btn primary', text: '新建投递', onclick: () => applicationEditor(root) }),
    ]),
  );
  const board = el('div', { class: 'kanban' });
  PIPELINE_ORDER.forEach((stage) => {
    const items = consoleState.applications.filter((a) => a.stage === stage);
    const column = el('div', { class: 'kanban-col' }, [
      el('div', { class: 'kanban-head' }, [
        el('strong', { text: pipelineLabel(stage) }),
        el('span', { class: 'muted small', text: String(items.length) }),
      ]),
    ]);
    items.forEach((a) => column.append(pipelineCard(root, a)));
    board.append(column);
  });
  root.append(board);
}

function pipelineCard(root, app) {
  const nextStage = PIPELINE_ORDER[PIPELINE_ORDER.indexOf(app.stage) + 1];
  const rounds = app.rounds || [];
  const finished = rounds.filter((r) => r.status === 'finished').length;
  return el('article', { class: 'kanban-card' }, [
    el('strong', { text: app.candidate_name || app.candidate_ref }),
    el('p', { class: 'muted small', text: app.job_title || app.job_id }),
    el('div', { class: 'bar thin' }, [
      el('i', { style: { width: `${(finished / Math.max(1, rounds.length)) * 100}%` } }),
    ]),
    el('p', { class: 'muted small', text: `已完成 ${finished}/${rounds.length} 轮 · 当前第 ${app.current_round || 1} 轮` }),
    el('div', { class: 'actions tight' }, [
      nextStage ? el('button', {
        class: 'btn outline small',
        text: `推进到${pipelineLabel(nextStage)}`,
        onclick: () => moveStage(root, app, nextStage),
      }) : null,
      el('button', {
        class: 'btn outline small',
        text: '排期',
        onclick: () => navigate(`/console/schedules?application=${app.application_id || app.id}`),
      }),
      el('button', {
        class: 'btn ghost small',
        text: '淘汰',
        onclick: () => moveStage(root, app, 'rejected'),
      }),
    ]),
  ]);
}

async function moveStage(root, app, stage) {
  const id = app.application_id || app.id;
  const ok = await confirmDialog({
    title: `把 ${app.candidate_name || '该候选人'} 推进到"${pipelineLabel(stage)}"?`,
    body: '这一步会写入审计日志: 谁在什么时候改了阶段、备注是什么。',
    confirmText: '确认推进',
    danger: stage === 'rejected',
  });
  if (!ok) return;
  try {
    await api.patch(`/api/v1/applications/${id}`, { stage, note: '工作台操作' });
    toast('已更新流程阶段');
    renderPipeline(root);
  } catch (err) {
    toast(err.message, 'error');
  }
}

function applicationEditor(root) {
  const overlay = el('div', { class: 'modal' });
  const close = () => overlay.remove();
  const jobSelect = el('select', {});
  consoleState.jobs.forEach((j) => jobSelect.append(el('option', { value: j.id || j.job_id, text: j.title })));
  const candidateSelect = el('select', {});
  consoleState.candidates.forEach((c) => candidateSelect.append(el('option', { value: c.candidate_ref, text: `${c.name || c.candidate_ref}` })));
  const status = el('p', { class: 'muted small' });
  overlay.append(
    el('div', { class: 'modal-scrim', onclick: close }),
    el('div', { class: 'modal-card narrow', role: 'dialog', 'aria-modal': 'true' }, [
      el('div', { class: 'modal-body' }, [
        el('p', { class: 'eyebrow', text: '新建投递' }),
        el('h2', { class: 'display small', text: '把候选人放入职位流程' }),
        el('label', {}, ['职位', jobSelect]),
        el('label', {}, ['候选人', candidateSelect]),
        status,
        el('div', { class: 'actions' }, [
          el('button', {
            class: 'btn primary',
            text: '创建',
            onclick: async () => {
              try {
                await api.post('/api/v1/applications', {
                  job_id: jobSelect.value,
                  candidate_ref: candidateSelect.value,
                  owner: '工作台',
                });
                toast('投递已创建');
                close();
                renderPipeline(root);
              } catch (err) {
                status.textContent = err.message;
                status.className = 'muted small error';
              }
            },
          }),
          el('button', { class: 'btn outline', text: '取消', onclick: close }),
        ]),
      ]),
    ]),
  );
  document.body.append(overlay);
}
