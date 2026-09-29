package sandbox

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// 这些用例跑的是真实进程, 不是 mock。
//
// 沙箱的价值全在"真的执行了不可信代码"这件事上, 用 mock 验证等于
// 什么都没验证 —— 超时、输出截断、退出码这些行为只有真跑才暴露。
// 为了让 CI 在没装对应工具链的机器上也能通过, 缺工具链时用 t.Skip。

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("本机没有 python3, 跳过沙箱执行测试")
	}
}

func TestLookupLanguageRejectsUnknown(t *testing.T) {
	if _, err := LookupLanguage("brainfuck"); !errors.Is(err, ErrUnsupportedLanguage) {
		t.Fatalf("未登记的语言应返回 ErrUnsupportedLanguage, 实际 %v", err)
	}
	lang, err := LookupLanguage("Go")
	if err != nil {
		t.Fatalf("语言 ID 应大小写不敏感: %v", err)
	}
	if lang.Filename != "main.go" {
		t.Fatalf("Go 的落盘文件名应为 main.go, 实际 %q", lang.Filename)
	}
	if len(Languages()) != 3 {
		t.Fatalf("应登记 3 种语言, 实际 %d", len(Languages()))
	}
}

func TestLocalRunnerRunsCodeAndReturnsStdout(t *testing.T) {
	requirePython(t)
	r := NewLocalRunner()
	if r.Isolated() {
		t.Fatal("本机执行器必须如实声明自己不具备隔离能力")
	}

	res, err := r.Run(context.Background(), Request{
		Language: "python",
		Code:     "import sys\nprint(int(sys.stdin.read().strip()) * 2)\n",
		Stdin:    "21\n",
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !res.Passed() {
		t.Fatalf("执行应成功: %+v", res)
	}
	if strings.TrimSpace(res.Stdout) != "42" {
		t.Fatalf("stdout 不正确: %q", res.Stdout)
	}
	if res.Isolated || res.Engine == "" {
		t.Fatalf("结果必须如实标注隔离状态与引擎: %+v", res)
	}
}

func TestLocalRunnerTimesOutInsteadOfHanging(t *testing.T) {
	requirePython(t)
	r := NewLocalRunner()

	start := time.Now()
	res, err := r.Run(context.Background(), Request{
		Language: "python",
		Code:     "while True:\n    pass\n",
		Timeout:  300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("死循环必须被判定为超时: %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("超时控制失效, 实际耗时 %s", elapsed)
	}
}

func TestLocalRunnerTruncatesHugeOutput(t *testing.T) {
	requirePython(t)
	r := NewLocalRunner()

	res, err := r.Run(context.Background(), Request{
		Language: "python",
		Code:     "print('x' * 1000000)\n",
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !res.Truncated {
		t.Fatalf("超长输出必须被截断并标注: len=%d", len(res.Stdout))
	}
	if len(res.Stdout) > MaxOutputBytes {
		t.Fatalf("输出未被限制在上限内: %d", len(res.Stdout))
	}
}

func TestRunnerRejectsOversizedSubmission(t *testing.T) {
	r := NewLocalRunner()
	_, err := r.Run(context.Background(), Request{
		Language: "python",
		Code:     strings.Repeat("#\n", MaxCodeBytes),
	})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超大提交应返回 ErrTooLarge, 实际 %v", err)
	}
}

func TestJudgeReportsPerCaseOutcomeWithoutLeakingHiddenExpected(t *testing.T) {
	requirePython(t)
	r := NewLocalRunner()

	cases := []TestCase{
		{Name: "示例", Stdin: "2\n", ExpectedStdout: "4"},
		{Name: "隐藏用例", Stdin: "5\n", ExpectedStdout: "10", Hidden: true},
		{Name: "会失败", Stdin: "3\n", ExpectedStdout: "999"},
	}
	results, last := Judge(context.Background(), r, Request{
		Language: "python",
		Code:     "import sys\nprint(2 * int(sys.stdin.read().strip()))\n",
	}, cases)

	if len(results) != 3 {
		t.Fatalf("应返回 3 条用例结果, 实际 %d", len(results))
	}
	if !results[0].Passed || !results[1].Passed {
		t.Fatalf("前两个用例应通过: %+v", results)
	}
	if results[2].Passed {
		t.Fatalf("第三个用例不应通过: %+v", results[2])
	}
	if results[2].Expected != "999" {
		t.Fatalf("非隐藏用例应回传期望输出: %+v", results[2])
	}
	if results[1].Expected != "" {
		t.Fatalf("隐藏用例绝不能回传期望输出(否则候选人可以照抄): %+v", results[1])
	}
	if last.Engine == "" {
		t.Fatal("最后一次执行结果应带引擎信息")
	}
}

func TestNormalizeOutputIgnoresTrailingWhitespace(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"42\n", "42", true},
		{"42\r\n", "42", true},
		{"42 \n\n", "42", true},
		{"42\n", "43", false},
		{"a b\n", "a  b", false},
	}
	for _, c := range cases {
		if got := normalizeOutput(c.a) == normalizeOutput(c.b); got != c.want {
			t.Errorf("normalizeOutput(%q) vs (%q): got %v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestAutoRunnerPrefersIsolation(t *testing.T) {
	local := AutoRunner(context.Background(), "local", nil)
	if local.Isolated() {
		t.Fatal("显式要求本机执行时不应声称隔离")
	}
	// 自动选择的结果取决于机器上是否有容器运行时, 这里只验证约定:
	// 声称隔离的必须是容器引擎。
	auto := AutoRunner(context.Background(), "", nil)
	if auto.Isolated() && !strings.HasPrefix(auto.Name(), "docker") {
		t.Fatalf("只有容器引擎可以声称隔离, 实际 %s", auto.Name())
	}
}
