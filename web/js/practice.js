// 真人双向对练: 两个真人进同一间房, 一个提问一个作答。
//
// 与 AI 面试间的区别很直接: 这里没有状态机、没有 AI 提问, 服务端只负责
// 配对与转发 WebRTC 信令。因此前端要做的事反而更多 —— 它得自己维护
// "等配对 → 配对成功 → 通话中 → 结束"这条本地状态。
import { el, clear, api, toast, navigate, confirmDialog, fmtDuration } from './core.js';
import { openMedia, mediaErrorMessage, MicCapture, PeerLink } from './media.js';
import { currentUser } from './auth.js';

const state = {
  room: null,
  myRole: '',
  ws: null,
  peer: null,
  stream: null,
  capture: null,
  timer: null,
  poll: null,
  startedAt: 0,
};

export function renderPractice(root) {
  clear(root);
  if (!currentUser()) {
    root.append(el('article', { class: 'card narrow center' }, [
      el('p', { class: 'eyebrow', text: '真人双向对练' }),
      el('h1', { class: 'display small', text: '需要先登录' }),
      el('p', { class: 'lede', text: '对练需要知道"谁在和谁练", 因此需要一个账号。' }),
      el('div', { class: 'actions' }, [
        el('button', { class: 'btn primary', text: '去登录', onclick: () => navigate('/login') }),
      ]),
    ]));
    return;
  }
  if (state.room && state.room.state !== 'ended') {
    renderRoom(root);
    return;
  }
  renderLobby(root);
}

function renderLobby(root) {
  const topic = el('input', { placeholder: '想练什么? 例如: 后端一面 / 系统设计 / 自我介绍' });
  const role = el('select', {}, [
    el('option', { value: 'any', text: '都可以(系统自动补齐角色)' }),
    el('option', { value: 'interviewee', text: '我当候选人(想被面试)' }),
    el('option', { value: 'interviewer', text: '我当面试官(想练习提问)' }),
  ]);
  const status = el('p', { class: 'muted small' });

  root.append(
    el('div', { class: 'view-head' }, [
      el('div', {}, [
        el('p', { class: 'eyebrow', text: '真人双向对练' }),
        el('h1', { class: 'display', text: '找个人, 互相练一场。' }),
        el('p', {
          class: 'lede',
          text: '两个人进同一间房, 一个提问一个作答 —— 视频是点对点直连, '
            + '服务端只负责把你们配上并转发协商信息, 不经过服务端。',
        }),
      ]),
    ]),
    el('article', { class: 'card' }, [
      el('h2', { text: '选一个身份, 然后等待配对' }),
      el('div', { class: 'form-grid' }, [
        el('label', {}, ['对练主题', topic]),
        el('label', {}, ['我的角色', role]),
      ]),
      el('div', { class: 'actions' }, [
        el('button', {
          class: 'btn primary',
          text: '开始配对',
          onclick: () => join(root, topic.value, role.value, status),
        }),
      ]),
      status,
      el('ul', { class: 'doc-list' }, [
        el('li', { text: '两个人必须一正一反: 都想当面试官时不会被硬凑成一对。' }),
        el('li', { text: '等待超过 15 分钟会自动结束 —— 避免你关掉页面后留下一个空房间。' }),
        el('li', { text: '一方离开, 房间即结束; 结束后可以立刻重新排队。' }),
        el('li', { text: '对练不评分、不生成报告, 也不录像: 它是练习, 不是考核。' }),
      ]),
    ]),
  );
}

async function join(root, topic, role, status) {
  status.textContent = '正在进入配对队列…';
  status.className = 'muted small';
  try {
    const data = await api.post('/api/v1/practice/join', { topic, role });
    state.room = data.room;
    state.myRole = data.my_role;
    if (data.paired) {
      renderRoom(root);
      return;
    }
    renderWaiting(root);
  } catch (err) {
    status.textContent = err.message;
    status.className = 'muted small error';
  }
}

// renderWaiting 轮询配对结果。
// 用轮询而不是 WebSocket: 等待阶段还没有"房间通道"这个概念, 而轮询
// 每 2 秒一次的代价极低, 也不会因为 WebSocket 断线而卡在等待页。
function renderWaiting(root) {
  clear(root);
  const info = el('p', { class: 'muted' });
  root.append(
    el('article', { class: 'card narrow center' }, [
      el('p', { class: 'eyebrow', text: '正在配对' }),
      el('h1', { class: 'display small', text: '正在寻找对练伙伴…' }),
      el('p', { class: 'lede', text: `你的角色: ${roleLabel(state.myRole)}。请保持页面打开。` }),
      info,
      el('div', { class: 'actions' }, [
        el('button', {
          class: 'btn outline',
          text: '取消排队',
          onclick: async () => {
            await leaveRoom();
            renderLobby(root);
          },
        }),
      ]),
    ]),
  );

  state.poll = setInterval(async () => {
    try {
      const data = await api.get(`/api/v1/practice/room/${state.room.room_id}`);
      state.room = data.room;
      if (data.room.state === 'paired') {
        clearInterval(state.poll);
        state.poll = null;
        toast('已配对成功, 正在建立视频连接');
        renderRoom(root);
        return;
      }
      info.textContent = `已等待 ${fmtDuration((Date.now() - new Date(data.room.created_at).getTime()) / 1000)}`;
    } catch (err) {
      clearInterval(state.poll);
      state.poll = null;
      info.textContent = `等待中断: ${err.message}`;
      info.className = 'muted small error';
    }
  }, 2000);
}

function renderRoom(root) {
  clear(root);
  const room = state.room;
  const peerInfo = (room.participants || []).find((p) => p.role !== state.myRole) || {};
  const timerEl = el('span', { class: 'muted mono', text: '00:00' });
  const statusEl = el('span', { class: 'pill live' }, [el('i'), '正在连接']);
  const selfVideo = el('video', { class: 'preview', autoplay: true, muted: true, playsinline: true });
  const remoteVideo = el('video', { class: 'preview remote', autoplay: true, playsinline: true });
  const remoteNote = el('p', { class: 'muted small', text: '等待对方画面…' });

  root.append(
    el('div', { class: 'room-head' }, [
      el('span', { text: `对练 · ${room.topic}` }),
      el('span', { class: 'muted', text: `你: ${roleLabel(state.myRole)} · 对方: ${peerInfo.name || '对方'}` }),
      timerEl,
    ]),
    el('div', { class: 'room-grid' }, [
      el('div', { class: 'room-stage' }, [
        el('div', { class: 'self-card video' }, [selfVideo, el('span', { class: 'badge', text: '你' })]),
        el('div', { class: 'ai-card' }, [
          el('div', { class: 'bubble-avatar', text: (peerInfo.name || '对').slice(0, 1) }),
          el('p', { class: 'muted small', text: peerInfo.name || '对方' }),
          el('span', { class: 'pill neutral', text: roleLabel(peerInfo.role) }),
        ]),
      ]),
      el('div', { class: 'room-side' }, [
        el('div', { class: 'side-block' }, [
          el('p', { class: 'eyebrow', text: '状态' }),
          statusEl,
          remoteNote,
        ]),
        el('div', { class: 'side-block' }, [
          el('p', { class: 'eyebrow', text: '怎么练' }),
          el('p', {
            class: 'muted small',
            text: state.myRole === 'interviewer'
              ? '这一轮你来提问: 按你对这个岗位的理解出题, 并追问细节。'
              : '这一轮你来作答: 像真实面试一样完整回答, 可以要求对方追问。',
          }),
        ]),
      ]),
    ]),
    remoteVideo,
    el('div', { class: 'actions' }, [
      el('button', { class: 'btn outline', text: '重连视频', onclick: () => connectPeer() }),
      el('button', {
        class: 'btn ghost',
        text: '结束对练',
        onclick: async () => {
          const ok = await confirmDialog({
            title: '结束这次对练?',
            body: '结束后房间会立即关闭, 对方会看到"对方已离开"。',
            confirmText: '结束对练',
          });
          if (!ok) return;
          await leaveRoom();
          renderLobby(root);
        },
      }),
    ]),
  );

  state.startedAt = Date.now();
  state.timer = setInterval(() => {
    timerEl.textContent = fmtDuration((Date.now() - state.startedAt) / 1000);
  }, 1000);

  connect();

  async function connect() {
    try {
      state.stream = await openMedia({ video: true, audio: true });
      selfVideo.srcObject = state.stream;
      state.capture = new MicCapture(state.stream, { onFrame: () => {} });
      await state.capture.start();
    } catch (err) {
      statusEl.className = 'pill warn';
      statusEl.textContent = mediaErrorMessage(err, '摄像头/麦克风');
    }

    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    const ws = new WebSocket(`${proto}://${location.host}/ws/practice/${encodeURIComponent(room.room_id)}`);
    state.ws = ws;

    ws.onopen = () => {
      statusEl.className = 'pill live';
      statusEl.textContent = '已进入房间';
    };
    ws.onmessage = (event) => {
      if (typeof event.data !== 'string') return;
      let msg;
      try {
        msg = JSON.parse(event.data);
      } catch {
        return;
      }
      if (typeof msg.type === 'string' && msg.type.startsWith('webrtc.')) {
        if (state.peer) state.peer.handleSignal(msg);
        return;
      }
      switch (msg.type) {
        case 'practice_joined':
          state.selfId = msg.peer_id;
          state.peer = new PeerLink({
            ws,
            iceServers: msg.ice_servers || [],
            selfId: msg.peer_id,
            onRemoteStream: (id, stream) => {
              remoteVideo.srcObject = stream;
              remoteNote.textContent = '已连接';
            },
            onState: (id, s) => {
              if (s === 'connected') {
                statusEl.className = 'pill ok';
                statusEl.textContent = '视频已连通';
                remoteNote.textContent = '已连接';
              }
            },
            onPeerGone: () => {
              remoteVideo.srcObject = null;
              statusEl.className = 'pill warn';
              statusEl.textContent = '对方已离开';
              remoteNote.textContent = '对方已离开';
            },
          });
          state.peer.attachLocalStream(state.stream);
          // 双方都可能先进房间: 让"先到的人"在有对方加入时主动发起连接。
          break;
        case 'practice_peer_joined':
          remoteNote.textContent = `${msg.name || '对方'} 已进入房间`;
          connectPeer();
          break;
        case 'practice_peer_left':
          statusEl.className = 'pill warn';
          statusEl.textContent = '对方已离开';
          remoteNote.textContent = '对方已离开, 可以结束对练或重新排队';
          if (state.peer) state.peer.close();
          state.peer = null;
          break;
        case 'roster':
          break;
        default:
          break;
      }
    };
    ws.onclose = () => {
      statusEl.className = 'pill warn';
      statusEl.textContent = '连接已断开';
    };
  }

  // connectPeer 主动向对方发起 WebRTC 连接。
  function connectPeer() {
    if (!state.peer) return;
    const target = state.peer.peers.keys().next();
    if (target.done) return;
    state.peer.connect(target.value, state.stream).catch(() => {});
  }
}

async function leaveRoom() {
  clearInterval(state.poll);
  clearInterval(state.timer);
  state.poll = null;
  state.timer = null;
  if (state.ws && state.ws.readyState === WebSocket.OPEN) state.ws.close();
  if (state.peer) state.peer.close();
  if (state.capture) state.capture.stop();
  if (state.stream) state.stream.getTracks().forEach((t) => t.stop());
  state.ws = null;
  state.peer = null;
  state.capture = null;
  state.stream = null;
  if (state.room) {
    try {
      await api.post('/api/v1/practice/leave', { room_id: state.room.room_id });
    } catch {
      // 离开失败不影响本地清理: 让用户"退不出去"才是更糟的结果。
    }
  }
  state.room = null;
  state.myRole = '';
}

function roleLabel(role) {
  return { interviewer: '面试官', interviewee: '候选人', any: '待定' }[role] || role || '待定';
}
