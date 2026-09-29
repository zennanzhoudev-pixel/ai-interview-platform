// 候选人空间: 概览 -> 准备(设备/授权/简历) -> 面试间 -> 报告。
//
// 这里的所有请求都用**会话令牌**而不是 API Key: 候选人不可能持有
// 企业的管理密钥。令牌只能访问属于自己的那一场面试, 且会过期。
import {
  el, clear, api, candidateApi, store, toast, navigate, confirmDialog,
  stageLabel, competencyLabel, fmtDuration, fmtDate,
} from './core.js';
import { currentUser } from './auth.js';
import {
  openMedia, listDevices, mediaErrorMessage, MicCapture, PcmPlayer,
  InterviewRecorder, PeerLink, Proctor,
} from './media.js';
import { renderReport, downloadJSON, printReport, reportFilename } from './report.js';

// candidateStore 保存跨视图的状态。放在模块作用域而不是全局 window 上,
// 避免被第三方脚本或控制台误改。
const state = {
  token: store.get('candidateToken', ''),
  sessionId: store.get('candidateSessionId', ''),
  info: null,
  report: null,
  resume: null,
  consent: { recording: false, scoring: false },
  devices: { cameras: [], microphones: [] },
  selection: { cameraId: '', micId: '' },
  live: null,
};

export function candidateState() {
  return state;
}

// adoptLink 从 URL 上取面试链接里的会话与令牌。
export function adoptLink(params, query) {
  const sessionId = params.session || query.s || '';
  const token = query.t || query.token || '';
  if (sessionId) {
    state.sessionId = sessionId;
    store.set('candidateSessionId', sessionId);
  }
  if (token) {
    state.token = token;
    store.set('candidateToken', token);
  }
}

export function hasCandidateSession() {
  return Boolean(state.token);
}

export function signOutCandidate() {
  state.token = '';
  state.sessionId = '';
  state.info = null;
  state.report = null;
  state.resume = null;
  store.remove('candidateToken');
  store.remove('candidateSessionId');
}

// loadSession 拉取本场面试的展示信息。
export async function loadSession({ force = false } = {}) {
  if (!state.token) return null;
  if (state.info && !force) return state.info;
  const client = candidateApi(state.token);
  try {
    state.info = await client.get('/api/v1/candidate/session');
    state.sessionId = state.info.session_id || state.sessionId;
    store.set('candidateSessionId', state.sessionId);
    if (state.info.consent_required === false) {
      state.consent = { recording: true, scoring: true };
    }
    return state.info;
  } catch (err) {
    if (err.status === 401) {
      signOutCandidate();
      throw new Error('面试链接已过期或无效, 请联系招聘方重新获取。');
    }
    throw err;
  }
}

/* ---------------- 概览 ---------------- */

export function renderOverview(root) {
  clear(root);
  const user = currentUser();
  const account = user && user.user ? user.user : null;
  const displayName = (account && account.name) || (state.info && state.info.candidate_name) || '';

  root.append(
    el('div', { class: 'hero' }, [
      el('div', { class: 'hero-text' }, [
        el('p', { class: 'eyebrow', text: '候选人空间' }),
        el('h1', { class: 'display', text: displayName ? `${displayName}, 你好。` : greeting() }),
        el('p', {
          class: 'lede',
          text: '这里是你自己的面试空间: 面试安排、准备清单、历史记录与报告都在这里。'
            + '企业招聘的其他内容(职位、候选人、题库)与你无关, 也不会出现在这里。',
        }),
      ]),
      el('div', { class: 'hero-side' }, [
        el('span', { class: `pill ${state.token ? 'ok' : 'neutral'}`, text: state.token ? '本场链接已验证' : '尚未进入面试' }),
        user
          ? el('span', { class: 'pill mint', text: '账号已登录' })
          : el('span', { class: 'pill neutral', text: '未登录' }),
      ]),
    ]),
  );

  const sessionCard = el('article', { class: 'card' });
  root.append(sessionCard);

  if (!state.token) {
    clear(sessionCard);
    sessionCard.append(
      el('h2', { text: '还没有面试安排' }),
      el('p', {
        class: 'muted',
        text: '面试链接由招聘方发给你, 链接里带有本场面试的凭证。'
          + (user ? '你也可以先看看自己的历史面试记录。' : '注册账号后, 你的面试记录与报告会一直留在这里。'),
      }),
      el('div', { class: 'actions' }, [
        user
          ? el('button', { class: 'btn primary', text: '查看我的面试记录', onclick: () => navigate('/candidate/history') })
          : el('button', { class: 'btn primary', text: '登录 / 注册账号', onclick: () => navigate('/login') }),
        el('button', { class: 'btn outline', text: '产品介绍', onclick: () => navigate('/about') }),
      ]),
    );
  } else {
    clear(sessionCard);
    sessionCard.append(el('p', { class: 'muted', text: '正在加载这场面试的信息…' }));
    loadSession().then((info) => {
      clear(sessionCard);
      const progress = prepProgress();
      sessionCard.append(
        el('div', { class: 'card-head' }, [
          el('span', { class: 'avatar-square', text: initials(info.company) }),
          el('div', {}, [
            el('h2', { text: info.position || '面试' }),
            el('p', { class: 'muted' }, [
              `${info.company || ''} · ${info.round_name || `第 ${info.round} 轮`}`,
              modeBadge(info.mode),
            ]),
          ]),
        ]),
        el('dl', { class: 'facts' }, [
          fact('预计时长', `${info.minutes || 45} 分钟`),
          fact('AI 面试官', info.interviewer_name || 'AI'),
          fact('面试形式', modeLabel(info.mode)),
          fact('真人面试官', info.human_panel ? '本轮有真人参与' : '本轮为 AI 主持'),
        ]),
        progressRow(progress),
        el('div', { class: 'actions' }, [
          el('button', {
            class: 'btn primary wide',
            text: progress.pct >= 100 ? '进入面试间' : '继续准备',
            onclick: () => navigate(progress.pct >= 100 ? '/candidate/room' : '/candidate/prep'),
          }),
          el('button', { class: 'btn outline wide', text: '查看报告', onclick: () => navigate('/candidate/report') }),
        ]),
      );
    }).catch((err) => {
      clear(sessionCard);
      sessionCard.append(el('p', { class: 'muted', text: err.message }));
    });
  }

  root.append(flowSection(), checklistSection(), historySection(), valueSection());
}

// flowSection 用四步说明这场面试会发生什么。
//
// 放在这里而不是让人再点一次跳转: 候选人在开始前最想知道的正是这件事,
// 而"点一个按钮去看介绍"会把这一步推迟到他最紧张的时候。
function flowSection() {
  const steps = [
    ['设备检查', '确认摄像头、麦克风与网络, 现场测试电平'],
    ['数据授权', '录音与 AI 评分需要你本人同意, 会留痕可查'],
    ['简历确认', '上传后 AI 围绕简历追问, 报告里能定位到原文'],
      ['进入面试间', 'AI 提问、可语音作答、可随时打断'],
  ];
  return el('section', { class: 'block' }, [
    el('div', { class: 'block-head' }, [
      el('h2', { text: '这场面试怎么跑' }),
      el('span', { class: 'muted small', text: '四步, 大约 5 分钟准备 + 45 分钟面试' }),
    ]),
    el('div', { class: 'flow-row' }, steps.map(([title, desc], i) => el('div', { class: 'flow-step' }, [
      el('span', { class: 'flow-index', text: `0${i + 1}` }),
      el('strong', { text: title }),
      el('span', { class: 'muted small', text: desc }),
    ]))),
  ]);
}

// checklistSection 把"准备什么"摊开成清单。
function checklistSection() {
  const progress = prepProgress();
  const labels = { device: '设备检查', consent: '数据授权', resume: '简历确认', environment: '环境确认' };
  return el('section', { class: 'block' }, [
    el('div', { class: 'block-head' }, [
      el('h2', { text: '准备清单' }),
      el('span', { class: 'muted small', text: state.token ? `已完成 ${progress.done} / ${progress.total}` : '拿到面试链接后开始' }),
    ]),
    el('ul', { class: 'task-list' }, progress.steps.map((item) => el('li', {
      class: item.done ? 'done' : '',
    }, [
      el('span', { class: 'task-mark', text: item.done ? '✓' : '○' }),
      el('span', { text: labels[item.key] || item.key }),
    ]))),
    state.token
      ? el('div', { class: 'actions' }, [
          el('button', { class: 'btn primary', text: '继续准备', onclick: () => navigate('/candidate/prep') }),
        ])
      : null,
  ]);
}

// historySection 直接列出最近的面试记录。
function historySection() {
  const container = el('section', { class: 'block' });
  const user = currentUser();
  container.append(el('div', { class: 'block-head' }, [
    el('h2', { text: '我的面试记录' }),
    user ? el('button', { class: 'btn ghost small', text: '查看全部', onclick: () => navigate('/candidate/history') }) : null,
  ]));
  if (!user) {
    container.append(el('p', {
      class: 'muted small',
      text: '注册账号后, 每次面试的记录与报告都会按时间留在这里, 随时可以回看。',
    }));
    return container;
  }
  const list = el('div', { class: 'record-list', text: '加载中…' });
  container.append(list);
  api.get('/api/v1/candidate/history').then((data) => {
    clear(list);
    const sessions = (data.sessions || []).slice(0, 4);
    if (sessions.length === 0) {
      list.append(el('p', { class: 'muted small', text: '还没有面试记录。完成一场面试后它会出现在这里。' }));
      return;
    }
    sessions.forEach((item) => list.append(el('div', { class: 'record-row' }, [
      el('div', {}, [
        el('strong', { text: item.position || '面试' }),
        el('p', { class: 'muted small', text: `${fmtDate(item.created_at)} · 第 ${item.round || 1} 轮` }),
      ]),
      el('span', {
        class: `pill ${item.report_ready ? 'mint' : 'neutral'}`,
        text: item.report_ready ? (item.score ? `${item.score} 分` : '报告可用') : '进行中',
      }),
    ])));
  }).catch(() => {
    clear(list);
    list.append(el('p', { class: 'muted small', text: '暂时读不到历史记录。' }));
  });
  return container;
}

function valueSection() {
  const values = [
    ['自适应追问', '沿着你回答里的缺口继续深入, 而不是按脚本念题。'],
    ['全程透明', '阶段、时长、追问与评分来源在整个过程中都可见。'],
    ['证据驱动', '每个判断都能回到你的原话, 报告可逐条复核。'],
  ];
  return el('section', { class: 'value-row' }, values.map(([title, desc], i) => el('div', { class: 'value-item' }, [
    el('span', { class: 'value-index', text: `0${i + 1}` }),
    el('h3', { text: title }),
    el('p', { class: 'muted small', text: desc }),
  ])));
}

function emptyState() {
  // 候选人空间里**不能**出现"进入招聘工作台"。
  //
  // 这里曾经无条件放了那个按钮, 于是面试者会看到一个通往管理后台的入口 ——
  // 既是逻辑错误(他没这个权限), 也是体验事故(会让人怀疑自己进错了系统)。
  // 现在按角色分流: 候选人看到的是"怎么进来"和"进来之后能做什么";
  // 只有企业成员才会看到工作台入口。
  const user = currentUser();
  if (user && user.staff) {
    return el('section', { class: 'card narrow center' }, [
      el('p', { class: 'eyebrow', text: '候选人空间' }),
      el('h1', { class: 'display small', text: '你正在用企业账号浏览候选人空间。' }),
      el('p', {
        class: 'lede',
        text: '企业账号没有属于自己的面试, 因此这里不会显示任何面试安排。'
          + '要管理职位、候选人与报告, 请进入招聘工作台。',
      }),
      el('div', { class: 'actions' }, [
        el('button', { class: 'btn primary', text: '进入招聘工作台', onclick: () => navigate('/console/dashboard') }),
        el('button', { class: 'btn outline', text: '查看产品介绍', onclick: () => navigate('/about') }),
      ]),
    ]);
  }
  return el('section', { class: 'card' }, [
    el('p', { class: 'eyebrow', text: '候选人空间' }),
    el('h1', { class: 'display', text: '这里是你自己的面试空间。' }),
    el('p', {
      class: 'lede',
      text: '面试链接由招聘方发给你, 链接里带有本场面试的凭证。'
        + '如果你已经注册过账号, 也可以直接登录查看历史与报告。',
    }),
    el('div', { class: 'actions' }, [
      el('button', { class: 'btn primary', text: '登录 / 注册账号', onclick: () => navigate('/login') }),
      el('button', { class: 'btn outline', text: '了解这场面试怎么跑', onclick: () => navigate('/about') }),
    ]),
  ]);
}

function greeting() {
  const hour = new Date().getHours();
  if (hour < 6) return '凌晨好。';
  if (hour < 12) return '上午好。';
  if (hour < 18) return '下午好。';
  return '晚上好。';
}

function modeLabel(mode) {
  switch (mode) {
    case 'coding':
      return '视频 + 编程';
    case 'audio':
      return '语音';
    case 'text':
      return '文字';
    default:
      return '视频';
  }
}

function modeBadge(mode) {
  return el('span', { class: 'muted small', text: ` · ${modeLabel(mode)}` });
}

function initials(name) {
  const text = (name || '企业').trim();
  return text.slice(0, 2);
}

function fact(label, value) {
  return el('div', {}, [el('dt', { text: label }), el('dd', { text: value })]);
}

// prepProgress 是"准备进度"的唯一口径。
//
// 之前概览页与准备页各算一次进度, 结果同一场面试在两个页面上显示的
// 百分比不一样。口径只能有一处, 否则用户会开始怀疑整个系统的数据。
export function prepProgress() {
  const steps = [
    { key: 'device', done: Boolean(state.devices.checked) },
    { key: 'consent', done: Boolean(state.consent.recording && state.consent.scoring) },
    { key: 'resume', done: Boolean(state.resume) },
    { key: 'environment', done: Boolean(store.get('envChecked', false)) },
  ];
  const done = steps.filter((s) => s.done).length;
  return { steps, done, total: steps.length, pct: Math.round((done / steps.length) * 100) };
}

function progressRow(progress) {
  return el('div', {}, [
    el('div', { class: 'progress-row' }, [
      el('span', { class: 'muted', text: '准备进度' }),
      el('span', { class: 'muted', text: `${progress.pct}%` }),
    ]),
    el('div', { class: 'bar' }, [el('i', { style: { width: `${progress.pct}%` } })]),
    el('p', {
      class: 'muted small',
      text: progress.pct >= 100
        ? '准备完成, 可以进入面试间。'
        : '还需完成: 设备检查、数据授权、简历上传、环境确认。',
    }),
  ]);
}

/* ---------------- 准备 ---------------- */

const PREP_STEPS = [
  { key: 'device', title: '设备检查', hint: '确认摄像头、麦克风与网络可用' },
  { key: 'consent', title: '数据授权', hint: '录音与 AI 评分需要你的明示同意' },
  { key: 'resume', title: '简历确认', hint: '上传后 AI 会围绕简历追问' },
  { key: 'environment', title: '环境与规则', hint: '独立空间、关闭其他会议软件' },
  { key: 'enter', title: '进入面试间', hint: '一切就绪后开始' },
];

export function renderPrep(root) {
  clear(root);
  if (!state.token) {
    root.append(emptyState());
    return;
  }
  let current = 0;
  const stepper = el('ol', { class: 'stepper' });
  const panel = el('div', { class: 'card' });
  const stepCount = el('span', { class: 'muted' });

  const renderStep = () => {
    clear(stepper);
    PREP_STEPS.forEach((step, i) => {
      stepper.append(el('li', {
        class: i === current ? 'active' : (i < current ? 'done' : ''),
        onclick: () => { current = i; renderStep(); },
      }, [
        // 复用样式表里已有的 .num: 它定义了圆形序号以及 active/done 的配色。
        // 自己另起一个类名会得到"有数字但没有圆"的样子 —— 之前就是这样。
        el('span', { class: 'num', text: String(i + 1).padStart(2, '0') }),
        el('div', {}, [
          el('strong', { text: step.title }),
          el('span', { class: 'muted small', text: step.hint }),
        ]),
      ]));
    });
    stepCount.textContent = `第 ${current + 1} / ${PREP_STEPS.length} 步`;
    clear(panel);
    const builder = [stepDevice, stepConsent, stepResume, stepEnvironment, stepEnter][current];
    panel.append(builder(() => {
      current = Math.min(current + 1, PREP_STEPS.length - 1);
      renderStep();
    }, () => {
      current = Math.max(current - 1, 0);
      renderStep();
    }));
  };

  root.append(
    el('div', { class: 'subbar' }, [
      el('button', { class: 'btn outline small', text: '返回概览', onclick: () => navigate('/candidate') }),
      el('span', { class: 'muted', id: 'prepContext', text: '面试准备' }),
    ]),
    el('div', { class: 'view-head' }, [
      el('div', {}, [
        el('p', { class: 'eyebrow', text: '面试准备' }),
        el('h1', { class: 'display', text: '开始前, 确认一切准备就绪。' }),
      ]),
      stepCount,
    ]),
    el('div', { class: 'prep-grid' }, [stepper, panel]),
  );
  renderStep();
}

function stepDevice(next, back) {
  const wrap = el('div', {});
  const preview = el('video', { class: 'preview', autoplay: true, muted: true, playsinline: true });
  const meter = el('i', { style: { width: '0%' } });
  const status = el('p', { class: 'muted small', text: '点击下方按钮申请摄像头与麦克风权限。' });
  const selectCam = el('select', {});
  const selectMic = el('select', {});
  let stream = null;
  let capture = null;

  const stopAll = () => {
    if (capture) capture.stop();
    if (stream) stream.getTracks().forEach((t) => t.stop());
    capture = null;
    stream = null;
  };

  const start = async () => {
    stopAll();
    try {
      stream = await openMedia({
        video: true,
        audio: true,
        cameraId: state.selection.cameraId || undefined,
        micId: state.selection.micId || undefined,
      });
    } catch (err) {
      status.textContent = mediaErrorMessage(err, '摄像头/麦克风');
      status.className = 'muted small error';
      return;
    }
    preview.srcObject = stream;
    capture = new MicCapture(stream, {
      onLevel: (level) => {
        meter.style.width = `${Math.round(level * 100)}%`;
      },
    });
    try {
      await capture.start();
    } catch (err) {
      status.textContent = err.message;
      return;
    }
    state.mediaStream = stream;
    state.devices.checked = true;
    status.textContent = '设备正常。对着麦克风说一句话, 电平条应有明显起伏。';
    status.className = 'muted small';
    next();
  };

  const devices = el('div', { class: 'form-grid' }, [
    el('label', {}, ['摄像头', selectCam]),
    el('label', {}, ['麦克风', selectMic]),
  ]);
  listDevices().then((result) => {
    state.devices = { ...state.devices, ...result };
    const fill = (select, list, fallback) => {
      clear(select);
      select.append(el('option', { value: '', text: fallback }));
      list.forEach((d, i) => {
        select.append(el('option', { value: d.deviceId, text: d.label || `${fallback} ${i + 1}` }));
      });
    };
    fill(selectCam, result.cameras, '默认摄像头');
    fill(selectMic, result.microphones, '默认麦克风');
    selectCam.onchange = () => { state.selection.cameraId = selectCam.value; start(); };
    selectMic.onchange = () => { state.selection.micId = selectMic.value; start(); };
    if (!result.supported) {
      devices.hidden = true;
      status.textContent = '当前浏览器不支持音视频采集, 请改用 Chrome / Edge / Safari 的最新版本。';
    }
  });

  wrap.append(
    el('h2', { text: '设备检查' }),
    el('p', { class: 'muted small', text: '面试全程使用视频与语音。音频会在你的浏览器里转成 16kHz 单声道后上行, 只用于识别与评分。' }),
    devices,
    el('div', { class: 'device-grid' }, [
      el('div', { class: 'self-card video' }, [preview]),
      el('div', {}, [
        el('p', { class: 'muted small', text: '麦克风电平' }),
        el('div', { class: 'bar' }, [meter]),
        status,
      ]),
    ]),
    el('div', { class: 'actions' }, [
      el('button', { class: 'btn primary', text: '开启设备并测试', onclick: start }),
      el('button', { class: 'btn outline', text: '上一步', onclick: back }),
      el('button', { class: 'btn ghost', text: '跳过 (使用键盘作答)', onclick: next }),
    ]),
  );
  return wrap;
}

function stepConsent(next, back) {
  const recording = el('input', { type: 'checkbox', checked: state.consent.recording });
  const scoring = el('input', { type: 'checkbox', checked: state.consent.scoring });
  const status = el('p', { class: 'muted small' });
  const submit = el('button', {
    class: 'btn primary',
    text: '确认授权并保存留痕',
    onclick: async () => {
      if (!recording.checked || !scoring.checked) {
        status.textContent = '两项都需要勾选才能开始面试。';
        status.className = 'muted small error';
        return;
      }
      try {
        await candidateApi(state.token).post('/api/v1/candidate/consent', {
          recording: true, scoring: true,
        });
        state.consent = { recording: true, scoring: true };
        toast('授权已记录, 可随时在报告页核对');
        next();
      } catch (err) {
        status.textContent = err.message;
        status.className = 'muted small error';
      }
    },
  });
  return el('div', {}, [
    el('h2', { text: '数据授权' }),
    el('p', {
      class: 'lede',
      text: '开始前需要你明确同意两件事。同意的时间、范围与来源网络会留痕, 你可以随时要求导出或删除。',
    }),
    el('label', { class: 'check-row' }, [recording, el('span', { text: '我同意本场面试录音录像, 用于生成面试记录与后续人工复核。' })]),
    el('label', { class: 'check-row' }, [scoring, el('span', { text: '我同意由 AI 对本次面试作答进行评分与分析。' })]),
    el('div', { class: 'callout' }, [
      el('strong', { text: '我们不做的事' }),
      el('ul', { class: 'doc-list' }, [
        el('li', { text: '不做情绪识别, 不做面相分析。' }),
        el('li', { text: '反作弊只产出风险事件供人判断, 不会自动淘汰你。' }),
        el('li', { text: 'AI 只给建议, 最终决定由人做出。' }),
      ]),
    ]),
    status,
    el('div', { class: 'actions' }, [submit, el('button', { class: 'btn outline', text: '上一步', onclick: back })]),
  ]);
}

function stepResume(next, back) {
  const textarea = el('textarea', { rows: 10, placeholder: '把简历文本粘贴到这里(或直接上传文本文件)' });
  const status = el('p', { class: 'muted small' });
  const preview = el('div', { class: 'resume-preview' });
  const file = el('input', {
    type: 'file',
    accept: '.txt,.md,text/plain',
    onchange: async (event) => {
      const chosen = event.target.files && event.target.files[0];
      if (!chosen) return;
      textarea.value = await chosen.text();
    },
  });

  const submit = el('button', {
    class: 'btn primary',
    text: '解析并提交',
    onclick: async () => {
      if (!textarea.value.trim()) {
        status.textContent = '请先粘贴简历内容。';
        status.className = 'muted small error';
        return;
      }
      try {
        const parsed = await candidateApi(state.token).post('/api/v1/candidate/resume', {
          text: textarea.value,
        });
        state.resume = parsed;
        renderResumePreview(preview, parsed);
        status.textContent = `解析出 ${parsed.entities.length} 个可定位实体, 已随本场面试提交。`;
        status.className = 'muted small';
        toast('简历已提交, AI 会围绕它追问');
      } catch (err) {
        status.textContent = err.message;
        status.className = 'muted small error';
      }
    },
  });

  return el('div', {}, [
    el('h2', { text: '简历确认' }),
    el('p', {
      class: 'lede',
      text: '简历会参与面试: AI 会围绕你写过的项目追问, 并在报告里精确定位到原文位置。'
        + '如果你不想提交, 也可以跳过 —— 那时面试会以通用题库为主。',
    }),
    file,
    textarea,
    status,
    preview,
    el('div', { class: 'actions' }, [submit, el('button', { class: 'btn outline', text: '上一步', onclick: back }),
      el('button', { class: 'btn ghost', text: '跳过 (不参与追问)', onclick: next })]),
  ]);
}

function renderResumePreview(container, parsed) {
  clear(container);
  if (!parsed || !parsed.entities) return;
  const rows = parsed.entities.slice(0, 20).map((ent) => el('tr', {}, [
    el('td', { text: kindLabel(ent.kind) }),
    el('td', { text: ent.value }),
    el('td', { class: 'mono small', text: `第 ${ent.start}–${ent.end} 字` }),
  ]));
  container.append(
    el('p', { class: 'muted small', text: '这些内容会被 AI 用来追问, 括号里是它在简历原文中的位置:' }),
    el('table', { class: 'doc-table' }, [
      el('thead', {}, [el('tr', {}, [el('th', { text: '类型' }), el('th', { text: '内容' }), el('th', { text: '原文位置' })])]),
      el('tbody', {}, rows),
    ]),
  );
}

function kindLabel(kind) {
  return { skill: '技能', project: '项目', timeline: '时间线' }[kind] || kind;
}

function stepEnvironment(next, back) {
  const checks = ['environment', 'network', 'quiet'].map((key) => {
    const box = el('input', { type: 'checkbox', checked: store.get(`env_${key}`, false) });
    box.addEventListener('change', () => store.set(`env_${key}`, box.checked));
    return box;
  });
  const [env, net, quiet] = checks;
  return el('div', {}, [
    el('h2', { text: '环境与规则' }),
    el('ul', { class: 'doc-list' }, [
      el('li', { text: '找一个安静的独立空间, 不要在公共场所或通勤途中面试。' }),
      el('li', { text: '关掉其他会议软件与占带宽的下载任务。' }),
      el('li', { text: '面试中离开页面、遮挡摄像头会被记录为风险事件, 供人工判断。' }),
      el('li', { text: '如果网络中断, 重新打开链接即可继续 —— 你的作答不会丢。' }),
    ]),
    el('label', { class: 'check-row' }, [env, el('span', { text: '我处在安静、独立的空间, 摄像头与麦克风可用。' })]),
    el('label', { class: 'check-row' }, [net, el('span', { text: '我的网络稳定, 已关闭其他占用带宽的程序。' })]),
    el('label', { class: 'check-row' }, [quiet, el('span', { text: '我理解本轮由 AI 面试官主持, 并知道可以随时中止面试。' })]),
    el('div', { class: 'actions' }, [
      el('button', {
        class: 'btn primary',
        text: '确认并继续',
        onclick: () => {
          if (!checks.every((c) => c.checked)) {
            toast('请逐项确认后再继续', 'error');
            return;
          }
          store.set('envChecked', true);
          next();
        },
      }),
      el('button', { class: 'btn outline', text: '上一步', onclick: back }),
    ]),
  ]);
}

function stepEnter(next, back) {
  const progress = prepProgress();
  return el('div', {}, [
    el('h2', { text: '进入面试间' }),
    el('p', { class: 'lede', text: '进入后 AI 面试官会开始提问。你可以随时用语音作答, 也可以打字。' }),
    el('div', { class: 'callout' }, [
      el('strong', { text: `准备完成度 ${progress.pct}%` }),
      el('ul', { class: 'doc-list' }, progress.steps.map((s) => el('li', {
        text: `${s.done ? '✓' : '○'} ${stepTitle(s.key)}`,
      }))),
    ]),
    el('div', { class: 'actions' }, [
      el('button', { class: 'btn primary wide', text: '进入面试间', onclick: () => navigate('/candidate/room') }),
      el('button', { class: 'btn outline', text: '上一步', onclick: back }),
    ]),
  ]);
}

function stepTitle(key) {
  const found = PREP_STEPS.find((s) => s.key === key);
  return found ? found.title : key;
}

/* ---------------- 面试间 ---------------- */

// renderRoom 是面试间。它同时承担文本、语音与视频三条链路:
//
//   - 文本: JSON 帧 (question / answer / turn_result);
//   - 语音: 二进制 PCM16LE 帧上行, 二进制 PCM 下行播放, interrupt 打断;
//   - 视频: WebRTC 信令 (webrtc.offer/answer/ice) 走同一条 WebSocket。
//
// 三条链路复用一条连接是刻意设计: 面试间只需要一个端口、一次鉴权、
// 一条断线重连逻辑。多开一条通道意味着多一套失败模式。
export function renderRoom(root) {
  clear(root);
  if (!state.token) {
    root.append(emptyState());
    return;
  }

  const client = candidateApi(state.token);
  const info = state.info || {};
  const interview = {
    ws: null,
    player: new PcmPlayer(),
    capture: null,
    recorder: null,
    peer: null,
    proctor: null,
    voice: false,
    muted: false,
    cameraOn: true,
    startedAt: Date.now(),
    turnCount: 0,
    selfId: '',
    closed: false,
  };
  state.live = interview;

  const timerEl = el('span', { class: 'muted mono', text: '00:00' });
  const stageEl = el('span', { class: 'muted', text: '正在连接…' });
  const statusEl = el('span', { class: 'pill live' }, [el('i'), '正在连接']);
  const questionEl = el('p', { class: 'question', text: '正在建立连接…' });
  const tagsEl = el('p', { class: 'muted small', text: '' });
  const transcriptEl = el('div', { class: 'transcript' });
  const liveEl = el('p', { class: 'live-transcript muted small', hidden: true });
  const preview = el('video', { class: 'preview', autoplay: true, muted: true, playsinline: true });
  const remoteVideo = el('video', { class: 'preview remote', autoplay: true, playsinline: true, hidden: true });
  const peersEl = el('div', { class: 'peer-list' });
  const meter = el('i', { style: { width: '0%' } });
  const modeTag = el('span', { class: 'pill neutral', text: modeLabel(info.mode) });
  const codePanel = el('div', { class: 'code-panel', hidden: true });
  const answerInput = el('textarea', {
    rows: 2,
    placeholder: '用键盘作答(语音模式下直接说话即可, 这里会同步显示识别结果)',
  });
  const voiceButton = el('button', { class: 'btn outline', type: 'button', text: '语音作答' });
  const muteButton = el('button', { class: 'btn outline', type: 'button', text: '静音' });
  const cameraButton = el('button', { class: 'btn outline', type: 'button', text: '关摄像头' });

  root.append(
    el('div', { class: 'room-head' }, [
      el('span', { text: `${info.company || ''} · ${info.position || '面试'}` }),
      stageEl,
      timerEl,
    ]),
    el('div', { class: 'room-grid' }, [
      el('div', { class: 'room-stage' }, [
        el('div', { class: 'self-card video' }, [
          preview,
          el('span', { class: 'badge', text: '你' }),
          el('div', { class: 'meter' }, [meter]),
        ]),
        el('div', { class: 'ai-card' }, [
          el('div', { class: 'bubble-avatar', text: (info.interviewer_name || 'AI').slice(0, 1) }),
          el('p', { class: 'muted small', text: `AI 面试官 · ${info.interviewer_name || ''}` }),
          modeTag,
        ]),
        remoteVideo,
      ]),
      el('div', { class: 'room-side' }, [
        el('div', { class: 'side-block' }, [
          el('p', { class: 'eyebrow', text: '面试间在场' }),
          peersEl,
        ]),
        el('div', { class: 'side-block' }, [
          el('p', { class: 'eyebrow', text: '本场状态' }),
          statusEl,
          el('p', { class: 'muted small', text: '离开页面、遮挡摄像头会被记为风险事件, 由人判断, 不会自动淘汰。' }),
        ]),
      ]),
    ]),
    el('div', { class: 'room-question' }, [
      el('p', { class: 'muted small', text: `AI 面试官 · ${info.interviewer_name || ''}` }),
      questionEl,
      tagsEl,
    ]),
    codePanel,
    transcriptEl,
    liveEl,
    el('form', { class: 'composer', onsubmit: onSubmitAnswer }, [
      answerInput,
      voiceButton,
      muteButton,
      cameraButton,
      el('button', { class: 'btn primary', type: 'submit', text: '发送' }),
    ]),
    el('div', { class: 'actions' }, [
      el('button', { class: 'btn ghost', text: '结束并查看报告', onclick: () => finish() }),
    ]),
  );

  const timer = setInterval(() => {
    timerEl.textContent = fmtDuration((Date.now() - interview.startedAt) / 1000);
  }, 1000);

  voiceButton.onclick = toggleVoice;
  muteButton.onclick = () => {
    interview.muted = !interview.muted;
    if (interview.capture) interview.capture.setMuted(interview.muted);
    muteButton.textContent = interview.muted ? '取消静音' : '静音';
  };
  cameraButton.onclick = () => {
    interview.cameraOn = !interview.cameraOn;
    if (state.mediaStream) {
      state.mediaStream.getVideoTracks().forEach((t) => { t.enabled = interview.cameraOn; });
    }
    cameraButton.textContent = interview.cameraOn ? '关摄像头' : '开摄像头';
  };

  // 离开房间时必须把采集、录制、P2P 全部收干净。
  // 漏掉任何一项的表现都是"摄像头指示灯还亮着" —— 候选人会立刻
  // 认定这个系统不可信, 而这与实现质量无关, 纯粹是收尾没做完整。
  const cleanup = () => {
    if (interview.closed) return;
    interview.closed = true;
    clearInterval(timer);
    if (interview.proctor) interview.proctor.stop();
    if (interview.recorder) interview.recorder.stop().catch(() => {});
    if (interview.capture) interview.capture.stop();
    if (interview.peer) interview.peer.close();
    interview.player.close();
    if (state.mediaStream) {
      state.mediaStream.getTracks().forEach((t) => t.stop());
      state.mediaStream = null;
    }
    if (interview.ws && interview.ws.readyState === WebSocket.OPEN) interview.ws.close();
    window.removeEventListener('hashchange', cleanup);
  };
  window.addEventListener('hashchange', cleanup);

  async function setupDevices() {
    try {
      if (!state.mediaStream) {
        state.mediaStream = await openMedia({ video: true, audio: true });
      }
      preview.srcObject = state.mediaStream;
      interview.capture = new MicCapture(state.mediaStream, {
        onFrame: (frame) => {
          if (interview.voice && interview.ws && interview.ws.readyState === WebSocket.OPEN) {
            interview.ws.send(frame);
          }
        },
        onLevel: (level) => {
          meter.style.width = `${Math.round(level * 100)}%`;
          // 候选人一开口就打断 AI —— 这是语音面试体验的关键联动。
          // 服务端的 VAD 也会独立判断, 两条路径都成立才算真的能打断。
          if (level > 0.25 && interview.player.playing) interruptAI();
        },
      });
      await interview.capture.start();
      if (interview.recorder && interview.recorder.supported()) interview.recorder.start();
    } catch (err) {
      statusEl.className = 'pill warn';
      statusEl.textContent = mediaErrorMessage(err, '摄像头/麦克风');
    }
  }

  function toggleVoice() {
    interview.voice = !interview.voice;
    voiceButton.textContent = interview.voice ? '切到键盘' : '语音作答';
    if (interview.voice) {
      if (interview.ws && interview.ws.readyState === WebSocket.OPEN) {
        interview.ws.send(JSON.stringify({ type: 'start_voice' }));
      }
      toast('语音作答已开启, 直接说话即可');
    } else {
      toast('已切换到键盘作答');
    }
  }

  function interruptAI() {
    const played = interview.player.interrupt();
    if (interview.ws && interview.ws.readyState === WebSocket.OPEN) {
      interview.ws.send(JSON.stringify({ type: 'interrupt', played_ms: played }));
    }
  }

  function onSubmitAnswer(event) {
    event.preventDefault();
    const text = answerInput.value.trim();
    if (!text) return;
    send({ type: 'answer', text });
    appendTranscript('candidate', text);
    answerInput.value = '';
  }

  function send(payload) {
    if (!interview.ws || interview.ws.readyState !== WebSocket.OPEN) {
      toast('连接已断开, 正在重连…', 'error');
      return false;
    }
    interview.ws.send(JSON.stringify(payload));
    return true;
  }

  function appendTranscript(who, text) {
    transcriptEl.append(el('div', { class: `turn ${who === 'candidate' ? 'answer' : 'ask'}` }, [
      el('p', { class: 'turn-q' }, [
        el('span', { class: 'turn-tag', text: who === 'candidate' ? '我' : 'AI' }),
        text,
      ]),
    ]));
    transcriptEl.scrollTop = transcriptEl.scrollHeight;
  }

  function renderPeers(peers) {
    clear(peersEl);
    const list = Array.isArray(peers) ? peers : [];
    if (list.length === 0) {
      peersEl.append(el('p', { class: 'muted small', text: '只有你与 AI 在场。' }));
      return;
    }
    list.forEach((p) => {
      peersEl.append(el('div', { class: 'peer-row' }, [
        el('span', { class: 'dot' }),
        el('span', { text: p.role === 'observer' ? '真人面试官' : '候选人(你)' }),
        el('span', { class: 'muted small mono', text: p.peer_id ? p.peer_id.slice(5, 13) : '' }),
      ]));
    });
  }

  function connect() {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    const url = `${proto}://${location.host}/ws/interview/${encodeURIComponent(info.session_id || state.sessionId)}?token=${encodeURIComponent(state.token)}`;
    const ws = new WebSocket(url);
    ws.binaryType = 'arraybuffer';
    interview.ws = ws;

    ws.onopen = () => {
      statusEl.className = 'pill live';
      statusEl.textContent = '已连接';
      interview.startedAt = Date.now();
    };

    ws.onmessage = (event) => {
      if (typeof event.data !== 'string') {
        interview.player.play(event.data).catch(() => {});
        return;
      }
      let msg;
      try {
        msg = JSON.parse(event.data);
      } catch {
        return;
      }
      handleMessage(msg);
    };

    ws.onclose = () => {
      if (interview.closed) return;
      statusEl.className = 'pill warn';
      statusEl.textContent = '连接已断开';
    };
    ws.onerror = () => {
      statusEl.className = 'pill warn';
      statusEl.textContent = '连接异常';
    };
  }

  async function handleMessage(msg) {
    switch (msg.type) {
      case 'consent_required':
        renderConsentGate(root, async () => {
          await client.post('/api/v1/candidate/consent', { recording: true, scoring: true });
          state.consent = { recording: true, scoring: true };
          toast('授权已记录, 正在进入面试');
          renderRoom(root);
        });
        break;
      case 'state': {
        interview.selfId = msg.peer_id || '';
        stageEl.textContent = `${stageLabel(msg.stage)} · 第 ${msg.turn_count + 1} 轮`;
        if (msg.mode) modeTag.textContent = modeLabel(msg.mode);
        if (msg.resumed) toast('已恢复上次进度, 继续作答即可');
        renderPeers(msg.peers);
        setupPeerLink(msg.ice_servers || []);
        break;
      }
      case 'question':
        questionEl.textContent = msg.text;
        tagsEl.textContent = `${stageLabel(msg.stage)}${msg.probe ? ' · 追问' : ''}`;
        stageEl.textContent = `${stageLabel(msg.stage)} · 第 ${msg.index} 轮`;
        modeTag.textContent = msg.probe ? '追问中' : modeLabel(info.mode);
        appendTranscript('ai', msg.text);
        if (msg.stage === 'SCENARIO_DESIGN' && (info.mode === 'coding' || msg.probe === false)) {
          // 二面(编程轮)会打开编码面板; 其他轮次保持纯对话。
          if (info.mode === 'coding') enableCodePanel();
        }
        break;
      case 'turn_result':
        interview.turnCount += 1;
        tagTurnResult(msg);
        break;
      case 'report':
        state.report = msg.payload;
        toast('面试已结束, 正在生成报告');
        cleanup();
        navigate('/candidate/report');
        break;
      case 'partial_transcript':
        liveEl.hidden = false;
        liveEl.textContent = `识别中: ${msg.text}`;
        break;
      case 'final_transcript':
        liveEl.hidden = false;
        liveEl.textContent = `我说: ${msg.text}`;
        appendTranscript('candidate', msg.text);
        break;
      case 'user_speech_start':
        break;
      case 'user_speech_end':
        break;
      case 'interrupted':
        toast('已打断 AI 的发言');
        break;
      case 'assistant_text':
      case 'assistant_sentence':
        liveEl.hidden = false;
        liveEl.textContent = `AI: ${msg.text}`;
        break;
      case 'peer_joined':
        renderPeers(msg.peers);
        if (msg.role === 'observer') {
          toast('真人面试官已进入面试间');
          if (interview.peer && interview.selfId) {
            interview.peer.connect(msg.peer_id, state.mediaStream).catch(() => {});
          }
        }
        break;
      case 'peer_left':
        renderPeers(msg.peers);
        break;
      case 'roster':
        renderPeers(msg.peers);
        break;
      case 'error':
        toast(msg.message || '服务返回了一个错误', 'error');
        break;
      default:
        if (typeof msg.type === 'string' && msg.type.startsWith('webrtc.')) {
          if (interview.peer) interview.peer.handleSignal(msg);
        }
    }
  }

  function setupPeerLink(iceServers) {
    if (interview.peer || !interview.selfId) return;
    interview.peer = new PeerLink({
      ws: interview.ws,
      iceServers,
      selfId: interview.selfId,
      onRemoteStream: (id, stream) => {
        remoteVideo.hidden = false;
        remoteVideo.srcObject = stream;
      },
      onState: (id, s) => {
        if (s === 'connected') toast('与真人面试官的视频已连接');
      },
      onPeerGone: () => {
        remoteVideo.hidden = true;
        remoteVideo.srcObject = null;
      },
    });
    interview.peer.attachLocalStream(state.mediaStream);
  }

  function tagTurnResult(msg) {
    if (!msg.scored) return;
    statusEl.className = 'pill mint';
    statusEl.textContent = `本轮已评分 ${msg.level || ''}`;
    setTimeout(() => {
      statusEl.className = 'pill live';
      statusEl.textContent = '正在听取回答';
    }, 2500);
  }

  function enableCodePanel() {
    if (!codePanel.hidden) return;
    codePanel.hidden = false;
    codePanel.append(buildCodePanel(client));
  }

  async function finish() {
    const ok = await confirmDialog({
      title: '结束本场面试?',
      body: '结束后会立即生成报告; 未作答的问题会记为未覆盖项。',
      confirmText: '结束并生成报告',
    });
    if (!ok) return;
    // 服务端在最后一轮作答后才会给出报告; 主动结束时直接跳到报告页,
    // 由报告页去取已完成的会话(若尚未结束则会提示继续作答)。
    cleanup();
    navigate('/candidate/report');
  }

  interview.recorder = new InterviewRecorder(null, client, { kind: 'video' });
  if (info.recording_enabled === false) interview.recorder = null;
  interview.proctor = new Proctor({
    enabled: true,
    report: (type, detail) => send({ type: 'proctor', detail: `${type}: ${detail}` }) || reportProctor(type, detail),
  });

  setupDevices().then(() => {
    if (interview.recorder) interview.recorder.stream = state.mediaStream;
    if (interview.recorder && interview.recorder.supported()) interview.recorder.start();
    interview.proctor.start(preview, { analyzeFace: true });
    connect();
  });
  return interview;
}

// reportProctor 通过候选人事件接口上报风险信号。
//
// 走 HTTP 而不是 WebSocket: 这些事件与面试流程无关, 走长连接会让
// "面试消息"和"旁路信号"耦合在一起, 一旦 WebSocket 断了信号也丢了。
function reportProctor(type, detail) {
  const token = state.token;
  if (!token) return;
  fetch(`/api/v1/candidate/events?token=${encodeURIComponent(token)}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
    body: JSON.stringify({ type, detail, at_ms: Date.now() }),
  }).catch(() => {});
}

function renderConsentGate(root, onAgree) {
  clear(root);
  root.append(el('article', { class: 'card narrow center' }, [
    el('p', { class: 'eyebrow', text: '开始前需要你确认' }),
    el('h1', { class: 'display small', text: '录音与 AI 评分授权' }),
    el('p', {
      class: 'lede',
      text: '这场面试由招聘方在你的账号外发起, 因此需要你本人在此确认。'
        + '同意的时间、范围与来源网络会被留痕, 你可以随时要求导出或删除。',
    }),
    el('div', { class: 'actions' }, [
      el('button', { class: 'btn primary wide', text: '我同意, 开始面试', onclick: onAgree }),
      el('button', { class: 'btn ghost', text: '暂时不参加', onclick: () => navigate('/candidate') }),
    ]),
  ]));
}

// buildCodePanel 是编程轮次的作答面板。
//
// 用例里带 hidden 的期望输出不会回传给我们(服务端刻意不回传),
// 所以界面上只显示"通过/未通过", 这是防作弊的基本要求。
function buildCodePanel(client) {
  const language = el('select', {}, [
    el('option', { value: 'go', text: 'Go' }),
    el('option', { value: 'python', text: 'Python' }),
    el('option', { value: 'node', text: 'Node.js' }),
  ]);
  const code = el('textarea', {
    rows: 12,
    class: 'mono',
    placeholder: '在这里写代码。程序从标准输入读数据, 把结果写到标准输出。',
  });
  const stdin = el('input', { placeholder: '自定义输入(可选)' });
  const output = el('pre', { class: 'console-output', text: '点击"运行"执行你的代码。' });
  const runButton = el('button', {
    class: 'btn primary',
    type: 'button',
    text: '运行',
    onclick: async () => {
      output.textContent = '运行中…';
      try {
        const result = await client.post('/api/v1/candidate/code/run', {
          language: language.value,
          code: code.value,
          stdin: stdin.value,
        });
        const r = result.result || {};
        output.textContent = [
          `退出码 ${r.exit_code} · 用时 ${r.duration_ms}ms${r.timed_out ? ' · 超时' : ''}`,
          r.stdout ? `\n[stdout]\n${r.stdout}` : '',
          r.stderr ? `\n[stderr]\n${r.stderr}` : '',
          result.warning ? `\n⚠ ${result.warning}` : '',
        ].join('');
      } catch (err) {
        output.textContent = `执行失败: ${err.message}`;
      }
    },
  });
  return el('div', {}, [
    el('div', { class: 'code-head' }, [
      el('span', { class: 'eyebrow', text: '编程面板' }),
      language,
      stdin,
      runButton,
    ]),
    code,
    output,
  ]);
}

/* ---------------- 报告 ---------------- */

export function renderCandidateReport(root) {
  clear(root);
  if (!state.token) {
    root.append(emptyState());
    return;
  }
  const container = el('div', { class: 'view' });
  root.append(container);
  container.append(el('p', { class: 'muted', text: '正在读取报告…' }));

  // 报告以服务端为准: 页面刷新后内存里的那一份就不在了,
  // 因此必须能重新取回 —— 而不是"刷新一下报告就没了"。
  candidateApi(state.token).get('/api/v1/candidate/report').then((data) => {
    clear(container);
    const report = data.report || state.report;
    const session = data.report ? data : { ...data, candidate_name: state.info?.candidate_name };
    container.append(
      el('div', { class: 'subbar' }, [
        el('span', { class: 'eyebrow plain', text: '候选人报告' }),
        el('span', { class: 'muted', text: `${session.company || ''} · ${session.position || ''} · 第 ${session.round || 1} 轮` }),
        el('span', { class: `pill ${report ? 'mint' : 'warn'}`, text: report ? '报告可用' : '尚未生成' }),
      ]),
    );
    if (!report) {
      container.append(el('article', { class: 'card' }, [
        el('h2', { text: '这场面试还没有结束' }),
        el('p', { class: 'muted', text: '报告会在最后一道题作答完成后生成。你也可以继续作答, 或者稍后回来查看。' }),
        el('div', { class: 'actions' }, [
          el('button', { class: 'btn primary', text: '回到面试间', onclick: () => navigate('/candidate/room') }),
        ]),
      ]));
      return;
    }
    const body = el('div', { class: 'report-body' });
    container.append(body);
    renderReport(body, report, {
      resume: data.resume || state.resume,
      actions: [
        el('button', { class: 'btn outline', text: '打印 / 导出 PDF', onclick: () => printReport(`面试报告-${session.session_id || ''}`) }),
        el('button', {
          class: 'btn outline',
          text: '导出报告 JSON',
          onclick: () => downloadJSON(reportFilename(session), { report, consents: data.consents || [] }),
        }),
        el('button', { class: 'btn outline', text: '再面一场', onclick: again }),
      ],
    });
    if (Array.isArray(data.consents) && data.consents.length > 0) {
      container.append(el('article', { class: 'card' }, [
        el('h2', { text: '我同意过什么' }),
        el('table', { class: 'doc-table' }, [
          el('thead', {}, [el('tr', {}, [
            el('th', { text: '范围' }), el('th', { text: '时间' }), el('th', { text: '来源网段' }),
          ])]),
          el('tbody', {}, data.consents.map((c) => el('tr', {}, [
            el('td', { text: c.scope === 'recording' ? '录音录像' : 'AI 评分' }),
            el('td', { text: fmtDate(c.agreed_at) }),
            el('td', { class: 'mono small', text: c.source_ip || '—' }),
          ]))),
        ]),
      ]));
    }
  }).catch((err) => {
    clear(container);
    container.append(el('p', { class: 'muted', text: err.message }));
  });
}

// again 处理"再面一场"。
//
// 之前这个按钮会把候选人直接推进准备流程, 而准备进度仍是 100% ——
// 看起来像"新的一场", 其实只是回到同一个已完成会话, 属于误导。
// 正确的事实是: 下一场面试必须由招聘方发起(候选人无法自建会话),
// 因此这里如实说明, 并把本地准备状态清空, 让下一场重新检查设备。
async function again() {
  const ok = await confirmDialog({
    title: '再来一场面试?',
    body: '下一场面试需要招聘方在本系统里发起, 并通过邮件或消息把新链接发给你。'
      + '现在会清空本地准备状态, 以便下一场重新检查设备与授权。',
    confirmText: '清空并回到候选人空间',
  });
  if (!ok) return;
  store.remove('envChecked');
  state.devices = { cameras: [], microphones: [] };
  state.selection = { cameraId: '', micId: '' };
  state.report = null;
  state.info = null;
  toast('本地准备状态已清空; 请等待招聘方发送下一场面试链接');
  navigate('/candidate');
}
