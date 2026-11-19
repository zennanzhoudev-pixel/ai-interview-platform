package account

import "testing"

func TestLivenessRejectsIdenticalFrames(t *testing.T) {
	s := New(Config{Store: NewMemoryStore(), TenantID: "t"})
	v := []float32{1, 0, 0, 0}
	if score := s.livenessScore([][]float32{v, v, v}); score >= minLivenessMotion {
		t.Fatalf("完全相同的帧应判定为无活体, score=%v", score)
	}
}

func TestLivenessAcceptsChangingFrames(t *testing.T) {
	s := New(Config{Store: NewMemoryStore(), TenantID: "t"})
	a := []float32{1, 0, 0, 0}
	b := []float32{0, 1, 0, 0}
	if score := s.livenessScore([][]float32{a, b}); score <= minLivenessMotion {
		t.Fatalf("有变化的帧应通过, score=%v", score)
	}
}
