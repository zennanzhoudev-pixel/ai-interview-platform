// 人类面试官的旁听席。
//
// 3 面之后人类面试官需要真的进房间: 看候选人的表情与表达、与候选人视频
// 对话、随时接管节奏。这个页面就是那间房。
//
// 它刻意**不能**替候选人作答: 处理 answer 的只有候选人那条连接。
// 人类面试官影响面试的方式是"出现在视频里与候选人交流",
// 而不是冒充 AI 提交答案 —— 后者会污染评分证据链。
import { el, clear, toast, navigate, fmtDuration, stageLabel } from './core.js';
import { openMedia, mediaErrorMessage, MicCapture, PcmPlayer, PeerLink } from './media.js';

export function renderObserverRoom(root, sessionId) {
  clear(root);
  let ticket = null;
  try {
    ticket = JSON.parse(sessionStorage.getItem('observerTicket') || 'null');
  } catch {
    ticket = null;
  }
  if (!ticket || ticket.sessionId !== sessionId) {
    root.append(el('article', { class: 'card narrow center' }, [
      el('p', { class: 'eyebrow', text: '旁听席' }),
      el('h1', { class: 'display small', text: '没有可用的旁听票据' }),
      el('p', {
        class: 'lede',
        text: '旁听票据在"开始面试"时签发, 有效期为两小时。请回到面试安排页重新签发。',
      }),
      el('div', { class: 'actions' }, [
        el('button', { class: 'btn primary', text: '回到面试安排', onclick: () => navigate('/console/schedules') }),
      ]),
    ]));
    return;
  }

  const player = new PcmPlayer();
  const state = { ws: null, peer: null, capture: null, stream: null, selfId: '', closed: false };
  const timerEl = el('span', { class: 'muted mono', text: '00:00' });
  const stageEl = el('span', { class: 'muted', text: '正在连接…' });
  const statusEl = el('span', { class: 'pill live' }, [el('i'), '连接中']);
  const questionEl = el('p', { class: 'question', text: '等待候选人开始作答…' });
  const transcriptEl = el('div', { class: 'transcript' });
  const liveEl = el('p', { class: 'live-transcript muted small', hidden: true });
  const peersEl = el('div', { class: 'peer-list' });
  const selfPreview = el('video', { class: 'preview', autoplay: true, muted: true, playsinline: true });
  const remotePreview = el('video', { class: 'preview remote', autoplay: true, playsinline: true, hidden: true });
  const startedAt = Date.now();

  root.append(
    el('div', { class: 'subbar' }, [
      el('button', { class: 'btn outline small', text: '离开旁听席', onclick: () => leave() }),
      el('span', { class: 'muted mono small', text: sessionId }),
      stageEl,
      timerEl,
    ]),
    el('div', { class: 'room-grid' }, [
      el('div', { class: 'room-stage' }, [
        el('div', { class: 'self-card video' }, [
          selfPreview,
          el('span', { class: 'badge', text: '你(面试官)' }),
        ]),
        remotePreview,
        el('div', { class: 'ai-card' }, [
          el('div', { class: 'bubble-avatar', text: 'AI' }),
          el('p', { class: 'muted small', text: 'AI 面试官正在主持' }),
        ]),
      ]),
      el('div', { class: 'room-side' }, [
        el('div', { class: 'side-block' }, [
          el('p', { class: 'eyebrow', text: '在场' }),
          peersEl,
        ]),
        el('div', { class: 'side-block' }, [
          el('p', { class: 'eyebrow', text: '状态' }),
          statusEl,
          el('p', {
            class: 'muted small',
            text: '你能看到 AI 的提问与候选人作答进度, 但不能替候选人提交答案 —— 这会污染评分证据链。',
          }),
        ]),
      ]),
    ]),
    el('div', { class: 'room-question' }, [
      el('p', { class: 'muted small', text: '当前问题' }),
      questionEl,
    ]),
    transcriptEl,
    liveEl,
    el('div', { class: 'actions' }, [
      el('button', { class: 'btn outline', text: '开摄像头并接入视频', onclick: () => startVideo() }),
      el('button', { class: 'btn outline', text: '请求与候选人通话', onclick: () => callCandidate() }),
      el('button', { class: 'btn ghost', text: '离开', onclick: () => leave() }),
    ]),
  );

  const timer = setInterval(() => {
    timerEl.textContent = fmtDuration((Date.now() - startedAt) / 1000);
  }, 1000);

  function leave() {
    if (state.closed) return;
    state.closed = true;
    clearInterval(timer);
    if (state.capture) state.capture.stop();
    if (state.peer) state.peer.close();
    if (state.stream) state.stream.getTracks().forEach((t) => t.stop());
    player.close();
    if (state.ws && state.ws.readyState === WebSocket.OPEN) state.ws.close();
    sessionStorage.removeItem('observerTicket');
    navigate('/console/schedules');
  }

  async function startVideo() {
    try {
      if (!state.stream) {
        state.stream = await openMedia({ video: true, audio: true });
        selfPreview.srcObject = state.stream;
        state.capture = new MicCapture(state.stream, { onFrame: () => {} });
        await state.capture.start();
      }
      if (state.peer) state.peer.attachLocalStream(state.stream);
      toast('摄像头已开启, 等待与候选人建立点对点连接');
      // 主动向房间里已有的候选人发起连接。
      const candidate = currentPeers.find((p) => p.role === 'candidate');
      if (candidate && state.peer) await state.peer.connect(candidate.peer_id, state.stream);
    } catch (err) {
      toast(mediaErrorMessage(err, '摄像头/麦克风'), 'error');
    }
  }

  async function callCandidate() {
    const candidate = currentPeers.find((p) => p.role === 'candidate');
    if (!candidate) {
      toast('候选人当前不在房间里', 'error');
      return;
    }
    if (!state.peer) {
      toast('请先开启摄像头', 'error');
      return;
    }
    try {
      await state.peer.connect(candidate.peer_id, state.stream);
      toast('已发起视频连接');
    } catch (err) {
      toast(`连接失败: ${err.message}`, 'error');
    }
  }

  let currentPeers = [];
  function renderPeers(peers) {
    currentPeers = Array.isArray(peers) ? peers : [];
    clear(peersEl);
    if (currentPeers.length === 0) {
      peersEl.append(el('p', { class: 'muted small', text: '房间里暂时没有其他人。' }));
      return;
    }
    currentPeers.forEach((p) => {
      peersEl.append(el('div', { class: 'peer-row' }, [
        el('span', { class: 'dot' }),
        el('span', { text: p.role === 'candidate' ? '候选人' : '真人面试官' }),
      ]));
    });
  }

  function appendTurn(who, text) {
    transcriptEl.append(el('div', { class: `turn ${who === 'candidate' ? 'answer' : 'ask'}` }, [
      el('p', { class: 'turn-q' }, [
        el('span', { class: 'turn-tag', text: who === 'candidate' ? '候选人' : 'AI' }),
        text,
      ]),
    ]));
    transcriptEl.scrollTop = transcriptEl.scrollHeight;
  }

  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const url = `${proto}://${location.host}/ws/interview/${encodeURIComponent(sessionId)}`
    + `?role=observer&ticket=${encodeURIComponent(ticket.ticket)}`;
  const ws = new WebSocket(url);
  ws.binaryType = 'arraybuffer';
  state.ws = ws;

  ws.onopen = () => {
    statusEl.className = 'pill live';
    statusEl.textContent = '已进入面试间';
  };

  ws.onmessage = (event) => {
    if (typeof event.data !== 'string') {
      // AI 的语音是给候选人听的; 面试官听到会造成回声, 因此默认不播。
      return;
    }
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
      case 'observer_joined':
        state.selfId = msg.peer_id || '';
        stageEl.textContent = `${stageLabel(msg.stage)} · 第 ${msg.round} 轮`;
        renderPeers(msg.peers);
        state.peer = new PeerLink({
          ws,
          iceServers: ticket.iceServers || msg.ice_servers || [],
          selfId: state.selfId,
          onRemoteStream: (id, stream) => {
            remotePreview.hidden = false;
            remotePreview.srcObject = stream;
          },
          onState: (id, s) => {
            if (s === 'connected') toast('与候选人的视频已连接');
          },
          onPeerGone: () => {
            remotePreview.hidden = true;
            remotePreview.srcObject = null;
          },
        });
        if (state.stream) state.peer.attachLocalStream(state.stream);
        break;
      case 'peer_joined':
        renderPeers(msg.peers);
        if (msg.role === 'candidate') toast('候选人已进入面试间');
        break;
      case 'peer_left':
        renderPeers(msg.peers);
        break;
      case 'question':
        questionEl.textContent = msg.text;
        appendTurn('ai', msg.text);
        break;
      case 'turn_result':
        if (msg.scored) {
          transcriptEl.append(el('p', { class: 'muted small', text: `本轮评分: ${msg.level || ''} (置信度 ${((msg.confidence || 0) * 100).toFixed(0)}%)` }));
        }
        break;
      case 'final_transcript':
        appendTurn('candidate', msg.text);
        break;
      case 'partial_transcript':
        liveEl.hidden = false;
        liveEl.textContent = `候选人正在说: ${msg.text}`;
        break;
      case 'report':
        toast('候选人已答完, 报告已生成');
        window.open(`/#/console/reports/${msg.payload && msg.payload.session_id ? msg.payload.session_id : sessionId}`, '_blank');
        break;
      default:
        break;
    }
  };

  ws.onclose = () => {
    if (state.closed) return;
    statusEl.className = 'pill warn';
    statusEl.textContent = '连接已断开';
  };
  ws.onerror = () => {
    statusEl.className = 'pill warn';
    statusEl.textContent = '连接异常';
  };
}
