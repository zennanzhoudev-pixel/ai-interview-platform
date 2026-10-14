package orchestrator

import "testing"

func TestFollowUpAnchoredAcceptsOverlap(t *testing.T) {
	if !followUpAnchored("并发标记", "并发标记这个环节你当时是怎么实现的?") {
		t.Fatal("追问与参考答案有重叠词, 应判定为锚定")
	}
}

func TestFollowUpAnchoredRejectsDrift(t *testing.T) {
	if followUpAnchored("并发标记", "那你平时喜欢吃什么?") {
		t.Fatal("追问与参考答案毫无重叠, 应判定为跑偏")
	}
}

func TestFollowUpAnchoredEmptyReferenceIsAllowed(t *testing.T) {
	if !followUpAnchored("", "随便问一句") {
		t.Fatal("参考答案为空时不做锚定校验, 应放行")
	}
}
