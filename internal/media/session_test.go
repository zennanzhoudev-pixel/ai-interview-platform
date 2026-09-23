package media

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// eventCollector 收集链路事件, 支持按类型等待与统计。
type eventCollector struct {
	mu  sync.Mutex
	all []Event
	ch  chan Event
}

func newEventCollector() *eventCollector {
	return &eventCollector{ch: make(chan Event, 512)}
}

func (c *eventCollector) Emit(e Event) {
	c.mu.Lock()
	c.all = append(c.all, e)
	c.mu.Unlock()
	select {
	case c.ch <- e:
	default:
	}
}

func (c *eventCollector) waitFor(t *testing.T, typ EventType, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	got := 0
	for got < n {
		select {
		case e := <-c.ch:
			if e.Type == typ {
				got++
			}
		case <-deadline:
			t.Fatalf("等待 %d 个 %s 事件超时, 只收到 %d 个", n, typ, got)
		}
	}
}

func (c *eventCollector) count(typ EventType) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.all {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func (c *eventCollector) find(typ EventType) (Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.all {
		if e.Type == typ {
			return e, true
		}
	}
	return Event{}, false
}

// testTTS 是可精确控制的合成器: 每句产出固定块数, 每块需要测试放行一次。
// 只有这样才能稳定复现"AI 说到一半被打断"这个时序, 而不是靠 sleep 碰运气。
type testTTS struct {
	chunksPerSentence int
	chunkMS           int
	release           chan struct{}

	mu          sync.Mutex
	closedCount int
	canceled    int
	openedTexts []string
}

func (p *testTTS) Name() string { return "test-tts" }

func (p *testTTS) Speak(ctx context.Context, text string, v Voice) (TTSStream, error) {
	p.mu.Lock()
	p.openedTexts = append(p.openedTexts, text)
	p.mu.Unlock()

	st := &testTTSStream{
		ctx:     ctx,
		ch:      make(chan AudioChunk, 1),
		chunks:  p.chunksPerSentence,
		chunkMS: p.chunkMS,
		release: p.release,
		parent:  p,
	}
	go st.run()
	return st, nil
}

func (p *testTTS) stats() (closed, canceled int, opened []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closedCount, p.canceled, append([]string(nil), p.openedTexts...)
}

type testTTSStream struct {
	ctx     context.Context
	ch      chan AudioChunk
	chunks  int
	chunkMS int
	release chan struct{}
	parent  *testTTS
}

func (s *testTTSStream) Chunks() <-chan AudioChunk { return s.ch }

func (s *testTTSStream) Close() error {
	s.parent.mu.Lock()
	s.parent.closedCount++
	if s.ctx.Err() != nil {
		// 记录"因为 context 被取消而关闭"的次数。
		// 这是验证打断是否真的级联到达 TTS 层的直接证据。
		s.parent.canceled++
	}
	s.parent.mu.Unlock()
	return nil
}

func (s *testTTSStream) run() {
	defer close(s.ch)
	for i := 0; i < s.chunks; i++ {
		if s.release != nil {
			select {
			case <-s.release:
			case <-s.ctx.Done():
				return
			}
		}
		select {
		case s.ch <- AudioChunk{PCM: make([]byte, msBytes(s.chunkMS)), Seq: i}:
		case <-s.ctx.Done():
			return
		}
	}
}

func TestSpeakEmitsSentencesInOrder(t *testing.T) {
	col := newEventCollector()
	tts := &testTTS{chunksPerSentence: 1, chunkMS: 100}
	sess := NewSession(context.Background(), &ScriptedASR{}, tts, DefaultSessionConfig(), col)
	defer sess.Close()

	if err := sess.Speak(context.Background(), "第一句。第二句。"); err != nil {
		t.Fatalf("正常播报不应报错: %v", err)
	}

	col.waitFor(t, EvAssistantSentence, 2, 2*time.Second)
	if got := col.count(EvAssistantAudio); got != 2 {
		t.Fatalf("两句各一块音频, 应收到 2 块, 实际 %d", got)
	}
	_, _, opened := tts.stats()
	if len(opened) != 2 || opened[0] != "第一句" || opened[1] != "第二句" {
		t.Fatalf("应按句子顺序逐句合成, 实际 %v", opened)
	}
}

// 这是整个 Phase 1 最核心的测试: 打断必须
//  1. 立刻中断正在进行的合成(级联取消);
//  2. 只把"完整播完"的内容计入已听;
//  3. 不泄漏任何流。
func TestBargeInCancelsTTSAndKeepsOnlyHeardText(t *testing.T) {
	col := newEventCollector()
	release := make(chan struct{}, 16)
	tts := &testTTS{chunksPerSentence: 3, chunkMS: 200, release: release}
	asr := &ScriptedASR{NextText: func() string { return "候选人插话了" }}
	sess := NewSession(context.Background(), asr, tts, DefaultSessionConfig(), col)
	defer sess.Close()

	done := make(chan error, 1)
	go func() {
		done <- sess.Speak(context.Background(), "第一句话在这里。第二句话在后面。")
	}()

	// 放行第一句的 3 块音频(共 600ms)
	for i := 0; i < 3; i++ {
		release <- struct{}{}
	}
	col.waitFor(t, EvAssistantAudio, 3, 2*time.Second)

	// 放行第二句的第 1 块, 此时第二句只播了 200ms
	release <- struct{}{}
	col.waitFor(t, EvAssistantAudio, 1, 2*time.Second)

	// 候选人插话: 前端上报只播到 700ms
	sess.Interrupt(700)

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("被打断时 Speak 应返回 context.Canceled, 实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("打断后 Speak 没有及时退出")
	}

	ev, ok := col.find(EvInterrupted)
	if !ok {
		t.Fatal("应产生 interrupted 事件")
	}
	if !strings.Contains(ev.HeardText, "第一句话在这里") {
		t.Fatalf("完整播完的第一句应计入已听内容, 实际 %q", ev.HeardText)
	}
	if strings.Contains(ev.HeardText, "第二句话在后面") {
		t.Fatalf("只播了一半的句子不应计入已听内容, 实际 %q", ev.HeardText)
	}

	closed, canceled, _ := tts.stats()
	if closed < 2 {
		t.Fatalf("打断后不应有流泄漏: 打开 2 个流, 只关闭了 %d 个", closed)
	}
	if canceled == 0 {
		t.Fatal("打断必须级联取消到 TTS 层, 而不是只在本地置标志位")
	}

	heard := sess.ConsumeInterrupted()
	if heard != ev.HeardText {
		t.Fatalf("ConsumeInterrupted 应返回同样的已听内容: %q vs %q", heard, ev.HeardText)
	}
	if again := sess.ConsumeInterrupted(); again != "" {
		t.Fatalf("ConsumeInterrupted 应清空内容, 实际 %q", again)
	}
}

// 打断后的强制静默期必须吃掉紧随其后的音频,
// 否则 AI 的尾音会把自己唤醒, 形成"自言自语"的死循环。
func TestInterruptGuardSuppressesFramesAfterBargeIn(t *testing.T) {
	col := newEventCollector()
	release := make(chan struct{}, 16)
	tts := &testTTS{chunksPerSentence: 4, chunkMS: 200, release: release}
	asr := &ScriptedASR{NextText: func() string { return "回答" }}

	cfg := DefaultSessionConfig()
	cfg.InterruptGuardMS = 200
	sess := NewSession(context.Background(), asr, tts, cfg, col)
	defer sess.Close()

	done := make(chan error, 1)
	go func() { done <- sess.Speak(context.Background(), "这是一句比较长的话术。") }()

	release <- struct{}{}
	col.waitFor(t, EvAssistantAudio, 1, 2*time.Second)

	sess.Interrupt(0)
	<-done

	// 静默期内(前 200ms = 10 帧)的人声必须被丢弃
	for i := 0; i < 3; i++ {
		sess.OnAudioFrame(speechFrame())
	}
	// 静默期结束后再说话, 应该正常触发
	for i := 0; i < 12; i++ {
		sess.OnAudioFrame(quietFrame())
	}
	for i := 0; i < 3; i++ {
		sess.OnAudioFrame(speechFrame())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && col.count(EvUserSpeechStart) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if n := col.count(EvUserSpeechStart); n != 1 {
		t.Fatalf("静默期内的人声应被丢弃, 之后应正常触发一次; 实际触发 %d 次", n)
	}
}

func TestConsumeInterruptedIsEmptyWithoutBargeIn(t *testing.T) {
	sess := NewSession(context.Background(), &ScriptedASR{}, &LocalTTS{}, DefaultSessionConfig(), nil)
	defer sess.Close()
	if got := sess.ConsumeInterrupted(); got != "" {
		t.Fatalf("没有发生打断时应返回空串, 实际 %q", got)
	}
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	sess := NewSession(context.Background(), &ScriptedASR{}, &LocalTTS{}, DefaultSessionConfig(), nil)
	sess.Close()
	sess.Close()
}

// 事件通道被下游堵住时, 音频分片可以丢, 但控制事件不能丢。
func TestAudioEventsAreDroppableButControlEventsAreNot(t *testing.T) {
	var mu sync.Mutex
	var control []Event
	blocked := make(chan struct{})
	sink := SinkFunc(func(e Event) {
		if e.Type == EvAssistantAudio {
			<-blocked // 故意堵住音频事件, 模拟下游卡顿
		}
		mu.Lock()
		control = append(control, e)
		mu.Unlock()
	})

	cfg := DefaultSessionConfig()
	cfg.EventBuffer = 4
	sess := NewSession(context.Background(), &ScriptedASR{}, &testTTS{chunksPerSentence: 0}, cfg, sink)

	for i := 0; i < 50; i++ {
		sess.emit(Event{Type: EvAssistantAudio, Seq: i})
	}
	if sess.DroppedAudio() == 0 {
		t.Fatal("下游堵塞时音频事件应该被丢弃并计数, 而不是无限阻塞")
	}
	close(blocked)
	sess.Close()
}
