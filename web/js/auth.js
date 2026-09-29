// 账号中心: 登录、注册、人脸登录、个人中心、历史面试记录。
//
// 这一层要回答用户最关心的一个问题: **登录后进哪个界面**。
// 答案是服务端给的, 不是前端猜的 —— 登录响应里带 role/staff/next_path,
// 前端照着跳。让前端自己判断角色的后果是把候选人放进管理工作台,
// 或者反过来把管理员挡在门外。
import { el, clear, api, store, toast, navigate, confirmDialog, fmtDate, recommendationLabel } from './core.js';
import { openMedia, mediaErrorMessage } from './media.js';
import { renderReport } from './report.js';

// 当前登录状态缓存在模块里, 由 app.js 在启动与切换身份时刷新。
let me = null;

export function currentUser() {
  return me;
}

export function isStaff() {
  return Boolean(me && me.staff);
}

export function isLoggedIn() {
  return Boolean(me);
}

// loadMe 拉取当前登录账号; 未登录返回 null。
export async function loadMe() {
  try {
    const data = await api.get('/api/v1/auth/me');
    // 必须校验 payload 形状: 只有一个 {} 的话, "已登录"会被误判为真,
    // 于是页面渲染出一个没有身份的界面 —— 之后每个请求都 401,
    // 而用户看到的是"登录了但什么都点不动"。
    if (!data || !data.user) {
      me = null;
      return null;
    }
    me = data;
    return me;
  } catch {
    me = null;
    return null;
  }
}

export async function logout() {
  try {
    await api.post('/api/v1/auth/logout', {});
  } catch {
    // 退出失败也要清掉本地状态: 让用户"退不出去"比多一次请求更糟。
  }
  me = null;
  store.remove('candidateToken');
  store.remove('candidateSessionId');
  toast('已退出登录');
  navigate('/login');
}

/* ---------------- 登录 / 注册 / 人脸 ---------------- */

// renderAuthPage 是系统入口: 先选身份, 再决定进哪个平台。
export function renderAuthPage(root, tab = 'login') {
  clear(root);
  const tabs = [
    ['login', '密码登录'],
    ['register', '注册账号'],
    ['face', '人脸登录'],
  ];
  const tabBar = el('div', { class: 'auth-tabs' }, tabs.map(([key, label]) => el('button', {
    class: `auth-tab ${tab === key ? 'is-active' : ''}`,
    type: 'button',
    text: label,
    onclick: () => renderAuthPage(root, key),
  })));

  const body = el('div', { class: 'auth-body' });
  root.append(el('div', { class: 'auth-shell' }, [
    el('div', { class: 'auth-intro' }, [
      el('p', { class: 'eyebrow', text: 'AI Interview OS' }),
      el('h1', { class: 'display', text: '一个入口, 两种身份。' }),
      el('p', {
        class: 'lede',
        text: '企业成员登录后进入招聘工作台(职位、管道、题库、报告); '
          + '候选人登录后进入候选人空间(面试、报告、历史记录)。'
          + '两者用同一个登录页, 由系统按角色分流。',
      }),
      el('ul', { class: 'doc-list' }, [
        el('li', { text: '企业成员注册需要邀请码 —— 否则任何人都能给自己开管理员账号。' }),
        el('li', { text: '人脸登录必须先注册并在个人中心录入, 不支持无账号刷脸注册。' }),
        el('li', { text: '密码始终是主凭证, 人脸只是可选的便捷方式。' }),
      ]),
      // 机器身份的后路: 客户的 ATS 集成没有"人"来登录, 用 API Key。
      // 保留入口是为了不让"加了账号体系"把既有的集成路径堵死。
      el('p', { class: 'muted small' }, [
        '系统集成(ATS)没有人工登录环节, ',
        el('button', {
          class: 'link-btn',
          type: 'button',
          text: '改用 API Key 连接工作台',
          onclick: () => {
            store.set('useApiKey', true);
            navigate('/console/dashboard');
          },
        }),
        '。',
      ]),
    ]),
    el('article', { class: 'card auth-card' }, [tabBar, body]),
  ]));

  if (tab === 'register') renderRegister(body, root);
  else if (tab === 'face') renderFaceLogin(body, root);
  else renderLogin(body, root);
}

// enterByRole 是"登录之后去哪"的唯一实现。
function enterByRole(payload) {
  const next = payload && payload.next_path
    ? payload.next_path
    : (payload && payload.staff ? '/console/dashboard' : '/candidate');
  navigate(next);
}

function renderLogin(body, root) {
  const identifier = el('input', { placeholder: '手机号或邮箱', autocomplete: 'username' });
  const password = el('input', { type: 'password', placeholder: '密码', autocomplete: 'current-password' });
  const status = el('p', { class: 'muted small' });
  const submit = async () => {
    status.textContent = '正在登录…';
    status.className = 'muted small';
    try {
      const payload = await api.post('/api/v1/auth/login', {
        identifier: identifier.value.trim(),
        password: password.value,
      });
      me = payload;
      toast(payload.staff ? '已进入招聘工作台' : '已进入候选人空间');
      enterByRole(payload);
    } catch (err) {
      status.textContent = err.message;
      status.className = 'muted small error';
    }
  };
  body.append(
    el('h2', { text: '登录' }),
    el('label', {}, ['账号', identifier]),
    el('label', {}, ['密码', password]),
    el('div', { class: 'actions' }, [
      el('button', { class: 'btn primary wide', text: '登录', onclick: submit }),
    ]),
    status,
    el('p', { class: 'muted small', text: '登录后由系统按角色分流: 企业成员进工作台, 候选人进候选人空间。' }),
  );
  password.addEventListener('keydown', (e) => { if (e.key === 'Enter') submit(); });
}

function renderRegister(body, root) {
  const name = el('input', { placeholder: '真实姓名或称呼' });
  const email = el('input', { placeholder: '邮箱(与手机号至少填一个)' });
  const phone = el('input', { placeholder: '手机号(与邮箱至少填一个)' });
  const password = el('input', { type: 'password', placeholder: '密码, 至少 8 位' });
  const role = el('select', {}, [
    el('option', { value: 'candidate', text: '我是候选人(参加面试)' }),
    el('option', { value: 'interviewer', text: '我是面试官(进工作台)' }),
    el('option', { value: 'admin', text: '我是管理员(进工作台)' }),
    el('option', { value: 'scheduler', text: '我是系统集成账号' }),
  ]);
  const invite = el('input', { placeholder: '企业邀请码(仅企业成员需要)' });
  // 邀请码输入框**始终显示**。
  //
  // 之前它只在选中企业身份时才出现, 结果是"我明明记得有邀请码这一栏,
  // 怎么没了?" —— 隐藏一栏比显示一栏多出来的那点空间, 不值得让人怀疑
  // 功能被删掉了。这里改成常驻, 并用一句话说清什么时候需要它。
  const inviteRow = el('label', {}, ['企业邀请码(选企业身份时必填, 候选人留空)', invite]);
  const status = el('p', { class: 'muted small' });
  const submit = async () => {
    status.textContent = '正在创建账号…';
    status.className = 'muted small';
    try {
      const payload = await api.post('/api/v1/auth/register', {
        name: name.value.trim(),
        email: email.value.trim(),
        phone: phone.value.trim(),
        password: password.value,
        role: role.value,
        invite_code: invite.value.trim(),
      });
      me = payload;
      toast('账号已创建, 正在进入');
      enterByRole(payload);
    } catch (err) {
      status.textContent = err.message;
      status.className = 'muted small error';
    }
  };
  body.append(
    el('h2', { text: '注册' }),
    el('div', { class: 'form-grid' }, [
      el('label', {}, ['姓名', name]),
      el('label', {}, ['身份', role]),
      el('label', {}, ['邮箱', email]),
      el('label', {}, ['手机号', phone]),
    ]),
    el('label', {}, ['密码', password]),
    inviteRow,
    el('p', {
      class: 'muted small',
      text: '候选人不需要邀请码; 管理员/面试官/系统集成账号需要 —— 邀请码在服务启动时打印, '
        + '也可以用 make status 查看。',
    }),
    el('div', { class: 'actions' }, [
      el('button', { class: 'btn primary wide', text: '创建账号', onclick: submit }),
    ]),
    status,
    el('p', {
      class: 'muted small',
      text: '联系方式只用于登录与面试通知; 列表里会脱敏展示, 你也可以随时在个人中心删除账号数据。',
    }),
  );
}

// renderFaceLogin 实现"先有账号, 再刷脸"。
//
// 界面上必须把这条规则说清楚: 人脸登录需要先提供账号标识(手机号/邮箱),
// 做的是 1:1 比对 —— 不支持"没有账号直接刷脸", 因为一次误识别就会凭空
// 创建一个没人知道是谁的账号。
function renderFaceLogin(body, root) {
  const identifier = el('input', { placeholder: '先填手机号或邮箱, 再刷脸' });
  const video = el('video', { class: 'preview', autoplay: true, muted: true, playsinline: true });
  const status = el('p', { class: 'muted small', text: '点击"开始刷脸"后请在光线充足处正对摄像头。' });
  let stream = null;

  const capture = async () => {
    if (!identifier.value.trim()) {
      status.textContent = '请先填写手机号或邮箱 —— 人脸登录需要先有账号。';
      status.className = 'muted small error';
      return;
    }
    try {
      if (!stream) {
        stream = await openMedia({ video: true, audio: false });
        video.srcObject = stream;
      }
    } catch (err) {
      status.textContent = mediaErrorMessage(err, '摄像头');
      status.className = 'muted small error';
      return;
    }
    status.textContent = '正在采集…请保持正对摄像头 1 秒';
    status.className = 'muted small';
    try {
      const frame = await grabFrame(video);
      const payload = await api.post('/api/v1/auth/face/login', {
        identifier: identifier.value.trim(),
        frame,
      });
      me = payload;
      stopStream(stream);
      stream = null;
      toast(`人脸登录成功(相似度 ${(payload.match_score || 0).toFixed(3)})`);
      enterByRole(payload);
    } catch (err) {
      status.textContent = err.message;
      status.className = 'muted small error';
    }
  };

  body.append(
    el('h2', { text: '人脸登录' }),
    el('label', {}, ['账号', identifier]),
    el('div', { class: 'face-capture' }, [video]),
    el('div', { class: 'actions' }, [
      el('button', { class: 'btn primary', text: '开始刷脸', onclick: capture }),
      el('button', {
        class: 'btn outline',
        text: '停止摄像头',
        onclick: () => { stopStream(stream); stream = null; },
      }),
    ]),
    status,
    el('p', {
      class: 'muted small',
      text: '人脸登录的前置条件: 已注册账号, 且已在个人中心完成人脸录入。'
        + '当前人脸能力为开发级图像匹配, 识别不通过时请改用密码登录。',
    }),
  );
}

/* ---------------- 个人中心 ---------------- */

export function renderProfile(root) {
  clear(root);
  if (!me) {
    root.append(needLogin());
    return;
  }
  const user = me.user || {};
  const face = me.face || { enrolled: false };
  const matcher = me.matcher || {};

  root.append(el('div', { class: 'view-head' }, [
    el('div', {}, [
      el('p', { class: 'eyebrow', text: '个人中心' }),
      el('h1', { class: 'display', text: user.name || '我的账号' }),
      el('p', { class: 'lede', text: `${roleLabel(user.role)} · ${user.email || user.phone || ''}` }),
    ]),
    el('span', { class: `pill ${me.staff ? 'ok' : 'mint'}`, text: me.staff ? '企业成员' : '候选人' }),
  ]));

  // 人脸录入: 三帧一致才接受(与后端规则对应)。
  const video = el('video', { class: 'preview', autoplay: true, muted: true, playsinline: true });
  const faceStatus = el('p', { class: 'muted small', text: face.enrolled ? '已录入人脸' : '还没有录入人脸' });
  let stream = null;
  const faceCard = el('article', { class: 'card' }, [
    el('h2', { text: '人脸登录' }),
    el('div', { class: 'callout warn' }, [
      el('strong', { text: '关于人脸能力的实话' }),
      el('p', {
        class: 'muted small',
        text: `当前匹配器: ${matcher.name || 'local-image-similarity'}, 可信级别: ${matcher.assurance || 'development-only'}`,
      }),
      el('p', {
        class: 'muted small',
        text: matcher.note
          || '它是图像相似度匹配, 会受光线与姿态影响, 也挡不住用照片冒充。',
      }),
      (matcher.assurance || '').startsWith('development-only')
        ? el('p', {
            class: 'muted small',
            text: '想让识别接近 Face ID 的体验, 请用 FACE_MATCHER=http 接入真实人脸模型服务'
              + '(约定见 README: POST /embed 返回特征向量即可)。浏览器没有深度摄像头, '
              + '本地实现再怎么调参也到不了那个水平 —— 这是硬件差异, 不是配置问题。',
          })
        : null,
    ]),
    me.account_storage === 'memory'
      ? el('div', { class: 'callout warn' }, [
          el('strong', { text: '当前账号存在内存里' }),
          el('p', {
            class: 'muted small',
            text: '服务重启后账号与人脸模板都会消失(这也是"录入了却登不上"最常见的原因)。'
              + '用 MYSQL_DSN 启动可以把账号持久化到数据库。',
          }),
        ])
      : null,
    el('div', { class: 'face-capture' }, [video]),
    el('div', { class: 'actions' }, [
      el('button', {
        class: 'btn primary',
        text: face.enrolled ? '重新录入人脸' : '录入人脸',
        onclick: async () => {
          try {
            if (!stream) {
              stream = await openMedia({ video: true, audio: false });
              video.srcObject = stream;
            }
          } catch (err) {
            faceStatus.textContent = mediaErrorMessage(err, '摄像头');
            faceStatus.className = 'muted small error';
            return;
          }
          faceStatus.textContent = '请保持姿势稳定, 正在采集 3 帧…';
          faceStatus.className = 'muted small';
          try {
            const frames = [];
            for (let i = 0; i < 3; i += 1) {
              frames.push(await grabFrame(video));
              await sleep(400);
            }
            const result = await api.post('/api/v1/auth/face/enroll', { frames });
            face.enrolled = true;
            stopStream(stream);
            stream = null;
            faceStatus.textContent = `已录入 ${result.frames} 帧特征(维度 ${result.dim}); 模板只存特征向量, 不存照片。`;
            faceStatus.className = 'muted small';
            toast('人脸已录入, 下次可以直接刷脸登录');
          } catch (err) {
            faceStatus.textContent = err.message;
            faceStatus.className = 'muted small error';
          }
        },
      }),
      face.enrolled ? el('button', {
        class: 'btn outline',
        text: '移除人脸',
        onclick: async () => {
          const ok = await confirmDialog({
            title: '移除人脸模板?',
            body: '移除后无法再用人脸登录, 仍然可以用密码登录。可以随时重新录入。',
            confirmText: '移除',
            danger: true,
          });
          if (!ok) return;
          try {
            await api.del('/api/v1/auth/face');
            face.enrolled = false;
            faceStatus.textContent = '已移除人脸模板。';
            toast('人脸模板已移除');
          } catch (err) {
            toast(err.message, 'error');
          }
        },
      }) : null,
      face.enrolled ? el('button', {
        class: 'btn outline',
        text: '测一次(看相似度)',
        onclick: async () => {
          // "登不进去"最终都能归结成一个数字。把分数摊出来, 用户就不用反复试,
          // 维护者也不用猜是光线、姿势还是阈值的问题。
          try {
            if (!stream) {
              stream = await openMedia({ video: true, audio: false });
              video.srcObject = stream;
            }
            const frame = await grabFrame(video);
            const result = await api.post('/api/v1/auth/face/check', { frame });
            faceStatus.textContent = `相似度 ${result.score.toFixed(4)} · 阈值 ${result.threshold.toFixed(2)} · `
              + `${result.pass ? '可以通过' : '不会通过(换个光线或姿势再试)'}`;
            faceStatus.className = result.pass ? 'muted small' : 'muted small error';
          } catch (err) {
            faceStatus.textContent = err.message;
            faceStatus.className = 'muted small error';
          }
        },
      }) : null,
      el('button', {
        class: 'btn outline',
        text: '停止摄像头',
        onclick: () => { stopStream(stream); stream = null; },
      }),
    ]),
    faceStatus,
  ]);

  const current = el('input', { type: 'password', placeholder: '当前密码' });
  const next = el('input', { type: 'password', placeholder: '新密码, 至少 8 位' });
  const pwStatus = el('p', { class: 'muted small' });
  const passwordCard = el('article', { class: 'card' }, [
    el('h2', { text: '修改密码' }),
    el('div', { class: 'form-grid' }, [
      el('label', {}, ['当前密码', current]),
      el('label', {}, ['新密码', next]),
    ]),
    el('div', { class: 'actions' }, [
      el('button', {
        class: 'btn primary',
        text: '修改密码',
        onclick: async () => {
          try {
            await api.post('/api/v1/auth/password', {
              current_password: current.value, new_password: next.value,
            });
            toast('密码已修改, 请用新密码重新登录');
            me = null;
            navigate('/login');
          } catch (err) {
            pwStatus.textContent = err.message;
            pwStatus.className = 'muted small error';
          }
        },
      }),
    ]),
    el('p', { class: 'muted small', text: '修改密码后, 其它设备上的登录会立即失效。' }),
    pwStatus,
  ]);

  root.append(
    el('div', { class: 'profile-grid' }, [faceCard, passwordCard]),
    el('article', { class: 'card' }, [
      el('h2', { text: '账号信息' }),
      el('dl', { class: 'facts' }, [
        fact('姓名', user.name || '—'),
        fact('邮箱', user.email || '—'),
        fact('手机号', user.phone || '—'),
        fact('角色', roleLabel(user.role)),
        fact('注册时间', fmtDate(user.created_at)),
        fact('最近登录', fmtDate(user.last_login_at)),
        fact('人脸登录', face.enrolled ? '已录入' : '未录入'),
        fact('匹配器可信级别', matcher.assurance || '—'),
      ]),
      el('div', { class: 'actions' }, [
        el('button', {
          class: 'btn outline',
          text: me.staff ? '进入招聘工作台' : '回到候选人空间',
          onclick: () => navigate(me.staff ? '/console/dashboard' : '/candidate'),
        }),
        el('button', { class: 'btn ghost', text: '退出登录', onclick: () => logout() }),
      ]),
    ]),
  );
}

/* ---------------- 历史面试记录 ---------------- */

export function renderCandidateHistory(root) {
  clear(root);
  if (!me) {
    root.append(needLogin());
    return;
  }
  const body = el('div', { text: '正在读取历史记录…' });
  root.append(el('div', { class: 'view-head' }, [
    el('div', {}, [
      el('p', { class: 'eyebrow', text: '候选人空间' }),
      el('h1', { class: 'display', text: '我的面试记录。' }),
      el('p', {
        class: 'lede',
        text: '这里按时间列出你参加过的每一场面试, 以及当时生成的报告。'
          + '记录属于你本人, 只显示与你账号匹配的面试。',
      }),
    ]),
  ]), body);

  api.get('/api/v1/candidate/history').then((data) => {
    clear(body);
    const sessions = data.sessions || [];
    if (sessions.length === 0) {
      body.append(el('article', { class: 'card' }, [
        el('h2', { text: '还没有面试记录' }),
        el('p', {
          class: 'muted',
          text: '当你通过招聘方发送的面试链接完成一场面试后, 这里会出现记录与报告。'
            + '如果你刚换了手机号或邮箱注册, 可能需要用当时投递时使用的联系方式注册才能对上。',
        }),
        el('div', { class: 'actions' }, [
          el('button', { class: 'btn primary', text: '开始一场面试', onclick: () => navigate('/candidate') }),
        ]),
      ]));
      return;
    }
    const table = el('table', { class: 'doc-table' }, [
      el('thead', {}, [el('tr', {}, [
        el('th', { text: '时间' }), el('th', { text: '职位' }), el('th', { text: '轮次' }),
        el('th', { text: '结论' }), el('th', { text: '综合分' }), el('th', { text: '报告' }),
      ])]),
      el('tbody', {}, sessions.map((s) => el('tr', {}, [
        el('td', { class: 'muted small', text: fmtDate(s.created_at) }),
        el('td', { text: s.position || '—' }),
        el('td', { text: `第 ${s.round || 1} 轮` }),
        el('td', { text: s.recommendation ? recommendationLabel(s.recommendation) : '—' }),
        el('td', { text: s.score ? String(s.score) : '—' }),
        el('td', {}, [
          s.report_ready
            ? el('button', {
                class: 'btn outline small',
                text: '查看报告',
                onclick: () => showHistoryReport(s.session_id),
              })
            : el('span', { class: 'muted small', text: '面试未结束' }),
        ]),
      ]))),
    ]);
    body.append(el('article', { class: 'card' }, [
      el('h2', { text: `共 ${sessions.length} 场面试` }),
      table,
    ]));
  }).catch((err) => {
    clear(body);
    body.append(el('p', { class: 'muted', text: err.message }));
  });
}

// showHistoryReport 打开某场历史面试的报告。
//
// 候选人有账号时走账号接口, 没有账号(通过面试链接进来)时用手上的会话令牌 ——
// 两条路径都不需要 API Key, 因为报告属于候选人自己。
function showHistoryReport(sessionID) {
  const token = store.get('candidateToken', '');
  if (token && store.get('candidateSessionId', '') === sessionID) {
    navigate('/candidate/report');
    return;
  }
  toast('请使用该场面试的专属链接查看详细报告');
}

/* ---------------- 小工具 ---------------- */

function roleLabel(role) {
  return {
    admin: '管理员', interviewer: '面试官', scheduler: '系统集成账号', candidate: '候选人',
  }[role] || role || '未知角色';
}

// grabFrame 把当前视频画面截成一帧 JPEG data URL。
//
// 用 canvas 而不是直接传视频: 服务端只需要一张静帧, 而且 JPEG 比原始帧小得多。
// 分辨率压到 480 宽: 足够做匹配, 又不至于让每次请求带着几百 KB 上传。
function grabFrame(video, width = 480) {
  return new Promise((resolve, reject) => {
    const vw = video.videoWidth || 0;
    const vh = video.videoHeight || 0;
    if (!vw || !vh) {
      reject(new Error('摄像头画面还没准备好, 请稍候重试'));
      return;
    }
    const height = Math.round((vh / vw) * width);
    const canvas = document.createElement('canvas');
    canvas.width = width;
    canvas.height = height;
    canvas.getContext('2d').drawImage(video, 0, 0, width, height);
    resolve(canvas.toDataURL('image/jpeg', 0.85));
  });
}

function stopStream(stream) {
  if (stream) stream.getTracks().forEach((t) => t.stop());
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function fact(label, value) {
  return el('div', {}, [el('dt', { text: label }), el('dd', { text: value })]);
}

function needLogin() {
  return el('article', { class: 'card narrow center' }, [
    el('p', { class: 'eyebrow', text: '需要登录' }),
    el('h1', { class: 'display small', text: '请先登录' }),
    el('p', { class: 'lede', text: '登录后才能查看个人中心与面试记录。' }),
    el('div', { class: 'actions' }, [
      el('button', { class: 'btn primary', text: '去登录', onclick: () => navigate('/login') }),
    ]),
  ]);
}

// renderHistoryDetail 目前不需要单独页面: 报告复用候选人报告页。
export function noop() {}

// 让报告模块的渲染函数在账号场景下也能被复用(将来做"历史报告列表"时用到)。
export { renderReport };
