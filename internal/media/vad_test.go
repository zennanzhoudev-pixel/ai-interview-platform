package media

import (
	"math"
	"testing"
)

// msBytes 返回 ms 毫秒音频的字节数, 便于测试里按"听感时长"描述数据量。
func msBytes(ms int) int { return SampleRate * ms / 1000 * 2 }

func speechFrame() []byte { return SynthSine(FrameMS, 220, 0.3) }
func quietFrame() []byte  { return SynthSilence(FrameMS) }

func TestVADDetectsSpeechStartAndEnd(t *testing.T) {
	vad := NewVAD(DefaultVADConfig())

	for i := 0; i < 25; i++ {
		if ev := vad.ProcessFrame(quietFrame()); ev != VADSilence {
			t.Fatalf("静音段不应产生事件, 第 %d 帧得到 %s", i+1, ev)
		}
	}

	startedAt := -1
	for i := 0; i < 10; i++ {
		if vad.ProcessFrame(speechFrame()) == VADSpeechStart {
			startedAt = i + 1
			break
		}
	}
	if startedAt != 3 {
		t.Fatalf("StartFrames=3 应在第 3 帧触发说话开始, 实际第 %d 帧", startedAt)
	}

	endedAt := -1
	for i := 0; i < 40; i++ {
		if vad.ProcessFrame(quietFrame()) == VADSpeechEnd {
			endedAt = i + 1
			break
		}
	}
	if endedAt != 24 {
		t.Fatalf("EndFrames=24 应在第 24 帧静音后判定说完, 实际第 %d 帧", endedAt)
	}
	if vad.LastUtteranceMS() <= 0 {
		t.Fatal("说话结束后应记录本段语音时长")
	}
}

// 键盘敲击、咳嗽这类瞬时噪声不能触发一轮回答。
func TestVADIgnoresShortNoiseBurst(t *testing.T) {
	vad := NewVAD(DefaultVADConfig())

	vad.ProcessFrame(speechFrame())
	vad.ProcessFrame(speechFrame())
	for i := 0; i < 50; i++ {
		if ev := vad.ProcessFrame(quietFrame()); ev != VADSilence {
			t.Fatalf("两帧噪声不应触发说话开始, 第 %d 帧得到 %s", i+1, ev)
		}
	}
	if vad.SpeechCount() != 0 {
		t.Fatalf("噪声不应计入语音段, 实际 %d 段", vad.SpeechCount())
	}
}

// 噪声底必须自适应: 安静办公室里的人声和机房里的人声, 绝对能量完全不同,
// 用同一个固定阈值必然误判一边。
func TestVADNoiseFloorAdaptsToEnvironment(t *testing.T) {
	vad := NewVAD(DefaultVADConfig())
	if vad.NoiseFloor() <= 0 {
		t.Fatal("初始噪声底应大于 0")
	}

	// 模拟一个更嘈杂的环境: 背景噪声幅度 0.02, 对应 RMS 约 0.0141
	const amp = 0.02
	const wantRMS = amp / 1.4142135623730951
	noise := SynthSine(FrameMS, 400, amp)

	for i := 0; i < 300; i++ {
		if ev := vad.ProcessFrame(noise); ev != VADSilence {
			t.Fatalf("持续背景噪声不应触发说话, 第 %d 帧得到 %s", i+1, ev)
		}
	}

	floor := vad.NoiseFloor()
	if diff := math.Abs(floor - wantRMS); diff > 0.002 {
		t.Fatalf("噪声底应收敛到环境真实能量 %.4f, 实际 %.4f", wantRMS, floor)
	}
	if floor <= DefaultVADConfig().AbsFloor {
		t.Fatalf("嘈杂环境下噪声底应高于绝对下限 %.4f, 实际 %.4f",
			DefaultVADConfig().AbsFloor, floor)
	}

	// 同样的环境里说话, 仍然要能检测出来
	started := false
	for i := 0; i < 10; i++ {
		if vad.ProcessFrame(speechFrame()) == VADSpeechStart {
			started = true
			break
		}
	}
	if !started {
		t.Fatal("有背景噪声时仍应能检测到人声")
	}
}

// 说话期间的噪声底不能被候选人自己的声音拉高, 否则后半句会检测不到。
func TestVADDoesNotRaiseFloorWhileSpeaking(t *testing.T) {
	vad := NewVAD(DefaultVADConfig())
	for i := 0; i < 5; i++ {
		vad.ProcessFrame(speechFrame())
	}
	if !vad.Speaking() {
		t.Fatal("应处于说话态")
	}
	floor := vad.NoiseFloor()

	for i := 0; i < 100; i++ {
		vad.ProcessFrame(speechFrame())
	}
	if vad.NoiseFloor() != floor {
		t.Fatalf("说话期间噪声底不应变化: %.6f -> %.6f", floor, vad.NoiseFloor())
	}
	if !vad.Speaking() {
		t.Fatal("连续说话期间不应退出说话态")
	}
}

func TestVADSilenceNeverTriggersSpeech(t *testing.T) {
	vad := NewVAD(DefaultVADConfig())
	for i := 0; i < 300; i++ {
		if ev := vad.ProcessFrame(quietFrame()); ev != VADSilence {
			t.Fatalf("纯静音不应触发任何事件, 第 %d 帧得到 %s", i+1, ev)
		}
	}
	if vad.SpeechCount() != 0 {
		t.Fatalf("纯静音不应记录语音段, 实际 %d", vad.SpeechCount())
	}
}

func TestSplitFramesHandlesUnalignedInput(t *testing.T) {
	pcm := SynthSine(55, 200, 0.3) // 不足 3 整帧
	frames, rest := SplitFrames(pcm)
	if len(frames) != 2 {
		t.Fatalf("应切出 2 个整帧, 实际 %d", len(frames))
	}
	if len(rest) != msBytes(15) {
		t.Fatalf("余下应为 15ms, 实际 %d 字节", len(rest))
	}
	for i, f := range frames {
		if len(f) != FrameBytes {
			t.Fatalf("第 %d 帧长度应为 %d, 实际 %d", i, FrameBytes, len(f))
		}
	}
}
