package media

import "testing"

func TestEndpointerTreatsTooShortUtteranceAsNoise(t *testing.T) {
	cfg := DefaultEndpointerConfig()
	cfg.MinUtteranceMS = 300
	ep := NewEndpointer(DefaultVADConfig(), cfg)

	// 200ms 语音: 小于 300ms, 视为咳嗽/环境音
	for i := 0; i < 10; i++ {
		ep.OnFrame(speechFrame())
	}
	var reason EndReason
	for i := 0; i < 30; i++ {
		_, r := ep.OnFrame(quietFrame())
		if r != EndNone {
			reason = r
			break
		}
	}
	if reason != EndNone {
		t.Fatalf("200ms 的短促声音不应构成一轮回答, 实际结束原因 %q", reason)
	}
}

func TestEndpointerEndsOnSilence(t *testing.T) {
	ep := NewEndpointer(DefaultVADConfig(), DefaultEndpointerConfig())

	for i := 0; i < 30; i++ {
		ep.OnFrame(speechFrame())
	}
	var reason EndReason
	for i := 0; i < 30; i++ {
		_, r := ep.OnFrame(quietFrame())
		if r != EndNone {
			reason = r
			break
		}
	}
	if reason != EndSilence {
		t.Fatalf("静音超时后应判定说完, 实际 %q", reason)
	}
}

func TestEndpointerForcesEndOnMaxDuration(t *testing.T) {
	cfg := DefaultEndpointerConfig()
	cfg.MaxUtteranceMS = 200 // 10 帧
	ep := NewEndpointer(DefaultVADConfig(), cfg)

	var reason EndReason
	for i := 0; i < 60; i++ {
		if _, r := ep.OnFrame(speechFrame()); r != EndNone {
			reason = r
			break
		}
	}
	if reason != EndMaxDuration {
		t.Fatalf("持续说话超过上限应强制收口, 实际 %q", reason)
	}
}

func TestEndpointerSemanticDetection(t *testing.T) {
	ep := NewEndpointer(DefaultVADConfig(), DefaultEndpointerConfig())

	cases := []struct {
		text  string
		final bool
		want  EndReason
	}{
		{"我的回答就是这些", false, EndNone},
		{"大概就这些", false, EndSemantic},
		{"暂时没有", false, EndSemantic},
		{"我把索引换成了 ZSet 结构。", false, EndSemantic},
		{"我还没说完, 让我再想想", false, EndNone},
		{"随便什么内容", true, EndASRFinal},
		{"", false, EndNone},
	}
	for _, c := range cases {
		if got := ep.OnText(c.text, c.final); got != c.want {
			t.Errorf("OnText(%q, final=%v) = %q, 期望 %q", c.text, c.final, got, c.want)
		}
	}
}

func TestSplitSentences(t *testing.T) {
	got := SplitSentences("你好, 我是本轮的 AI 面试官。开始前请做个自我介绍。重点讲最有代表性的项目。")
	want := []string{"你好, 我是本轮的 AI 面试官", "开始前请做个自我介绍", "重点讲最有代表性的项目"}
	if len(got) != len(want) {
		t.Fatalf("应切出 %d 句, 实际 %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 句 = %q, 期望 %q", i, got[i], want[i])
		}
	}
}

// 单句过长会拖长首字延迟, 必须按次级标点再切。
func TestSplitSentencesWrapsLongSentence(t *testing.T) {
	long := ""
	for i := 0; i < 12; i++ {
		long += "这是一个很长的分句, "
	}
	long += "最后收尾。"

	got := SplitSentences(long)
	if len(got) < 2 {
		t.Fatalf("长句应被切成多段, 实际 %d 段", len(got))
	}
	for i, s := range got {
		if runeLen(s) > maxSentenceRunes {
			t.Errorf("第 %d 段长度 %d 超过上限 %d: %q", i, runeLen(s), maxSentenceRunes, s)
		}
	}
}
