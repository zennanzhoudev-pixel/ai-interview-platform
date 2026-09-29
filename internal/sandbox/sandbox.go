// Package sandbox 提供候选人代码的隔离执行。
//
// 这是整个系统里唯一"主动执行外部输入"的地方 —— 候选人提交的代码是
// 不可信输入, 它会在我们的机器上跑。因此这个包的设计原则与其他包不同:
//
//  1. **默认选择最强隔离**: 只要有容器运行时就用容器, 并且显式关掉网络;
//  2. **把"不隔离"这件事说出来**: LocalRunner.Isolated() 返回 false,
//     调用方必须把它标注到响应与日志里, 而不是让人误以为已经安全;
//  3. **限制写死在代码里**: 超时、内存、进程数、输出长度都是常量,
//     不接受来自请求参数的覆盖 —— "让候选人自己决定给多少资源"
//     等于没有限制。
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 资源上限。硬编码而非配置: 这些值是安全边界, 不是性能调优参数。
const (
	DefaultTimeout  = 6 * time.Second
	DefaultMemoryMB = 256
	DefaultCPUs     = 0.5
	DefaultPids     = 128
	// MaxOutputBytes 限制捕获的输出。不加限制时, 一行 fmt.Println 死循环
	// 就能把服务的内存打满 —— 而且是在别人的面试进行到一半的时候。
	MaxOutputBytes = 64 << 10
	// MaxCodeBytes 限制提交体积, 避免把大文件写进临时目录。
	MaxCodeBytes = 256 << 10
	// MaxConcurrentRuns 限制同时在跑的沙箱数。
	MaxConcurrentRuns = 4
)

// ErrUnsupportedLanguage 表示请求了未登记的语言。
var ErrUnsupportedLanguage = errors.New("sandbox: 不支持的语言")

// ErrTooLarge 表示提交的代码超过上限。
var ErrTooLarge = errors.New("sandbox: 提交内容过大")

// Language 描述一种可执行语言。
type Language struct {
	ID           string
	Label        string
	Filename     string
	Image        string   // 容器镜像
	Command      []string // 容器内的执行命令
	LocalCommand []string // 宿主机上的执行命令(需要本机已装该工具链)
	Env          []string // 容器内环境变量
}

// languages 是登记在册的语言。
//
// 只放三种: 面试判题需要的是"能不能写出正确逻辑", 支持的语言越多,
// 每个镜像的体积与漏洞面就越大。加语言是一个显式决定, 不是一个配置项。
var languages = map[string]Language{
	"go": {
		ID: "go", Label: "Go 1.22", Filename: "main.go",
		Image:        "golang:1.22-alpine",
		Command:      []string{"go", "run", "main.go"},
		LocalCommand: []string{"go", "run", "main.go"},
		Env:          []string{"GOCACHE=/tmp/gocache", "GOPATH=/tmp/gopath", "GOFLAGS=-mod=mod", "GOPROXY=off"},
	},
	"python": {
		ID: "python", Label: "Python 3.12", Filename: "main.py",
		Image:        "python:3.12-alpine",
		Command:      []string{"python3", "main.py"},
		LocalCommand: []string{"python3", "main.py"},
	},
	"node": {
		ID: "node", Label: "Node 20", Filename: "main.js",
		Image:        "node:20-alpine",
		Command:      []string{"node", "main.js"},
		LocalCommand: []string{"node", "main.js"},
	},
}

// Languages 返回登记在册的语言(供前端下拉框与接口文档使用)。
func Languages() []Language {
	out := make([]Language, 0, len(languages))
	for _, id := range []string{"go", "python", "node"} {
		out = append(out, languages[id])
	}
	return out
}

// LookupLanguage 按 ID 查语言。
func LookupLanguage(id string) (Language, error) {
	lang, ok := languages[strings.ToLower(strings.TrimSpace(id))]
	if !ok {
		return Language{}, fmt.Errorf("%w: %s", ErrUnsupportedLanguage, id)
	}
	return lang, nil
}

// Request 是一次执行请求。
type Request struct {
	Language string
	Code     string
	Stdin    string
	// Timeout 允许调用方缩短超时(例如一组用例的剩余预算), 但无法超过默认上限。
	Timeout time.Duration
}

// Result 是一次执行结果。
type Result struct {
	Language   string `json:"language"`
	Engine     string `json:"engine"`
	Isolated   bool   `json:"isolated"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
}

// Passed 判断本次执行是否成功退出。
func (r Result) Passed() bool { return r.ExitCode == 0 && !r.TimedOut }

// TestCase 是一个判题用例。
type TestCase struct {
	Name           string `json:"name"`
	Stdin          string `json:"stdin"`
	ExpectedStdout string `json:"expected_stdout"`
	// Hidden 表示该用例不应把期望输出回传给候选人(防止"照抄期望输出")。
	Hidden bool `json:"hidden"`
}

// CaseResult 是用例判定结果。
type CaseResult struct {
	Name       string `json:"name"`
	Passed     bool   `json:"passed"`
	Actual     string `json:"actual"`
	Expected   string `json:"expected,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// Runner 执行不可信代码。
type Runner interface {
	// Name 返回执行引擎名(用于审计: "这份代码是在哪里跑的")。
	Name() string
	// Isolated 表示执行是否在真正的隔离环境里。返回 false 的实现
	// 只能用于本地开发与 CI, 绝不能在生产接受候选人代码。
	Isolated() bool
	Run(ctx context.Context, req Request) (Result, error)
}

/* ---------------- 容器执行 ---------------- */

// DockerRunner 用容器执行代码。
type DockerRunner struct {
	binary string
	sem    chan struct{}
	// pull 表示允许在本地缺失镜像时拉取。默认关闭: 面试进行中
	// 现场拉一个 800MB 的镜像, 会把整场面试拖死。
	pull bool
}

// NewDockerRunner 构造容器执行器。binary 为空时使用 docker。
func NewDockerRunner(binary string) *DockerRunner {
	if binary == "" {
		binary = "docker"
	}
	return &DockerRunner{binary: binary, sem: make(chan struct{}, MaxConcurrentRuns)}
}

// Name 返回执行引擎名。
func (d *DockerRunner) Name() string { return "docker" }

// Isolated 表示容器执行是隔离的。
func (d *DockerRunner) Isolated() bool { return true }

// Available 探测容器运行时是否可用。
//
// 探测的是 "docker info" 而不是 "docker version": 前者会真正连接
// daemon, 后者在 daemon 没起来时也会成功返回客户端版本, 于是
// "看起来可用、一跑就失败"。
func (d *DockerRunner) Available(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, d.binary, "info", "--format", "{{.ServerVersion}}").Run() == nil
}

// Run 在容器里执行代码。
func (d *DockerRunner) Run(ctx context.Context, req Request) (Result, error) {
	lang, err := LookupLanguage(req.Language)
	if err != nil {
		return Result{}, err
	}
	if len(req.Code) > MaxCodeBytes {
		return Result{}, ErrTooLarge
	}

	select {
	case d.sem <- struct{}{}:
		defer func() { <-d.sem }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}

	dir, err := os.MkdirTemp("", "interview-sandbox-*")
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, lang.Filename), []byte(req.Code), 0o644); err != nil {
		return Result{}, err
	}

	timeout := effectiveTimeout(req.Timeout, ctx)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"run", "--rm", "-i",
		"--network", "none",
		"--memory", fmt.Sprintf("%dm", DefaultMemoryMB),
		"--memory-swap", fmt.Sprintf("%dm", DefaultMemoryMB),
		"--cpus", fmt.Sprintf("%.2f", DefaultCPUs),
		"--pids-limit", fmt.Sprintf("%d", DefaultPids),
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"-v", dir + ":/box",
		"-w", "/box",
	}
	for _, env := range lang.Env {
		args = append(args, "-e", env)
	}
	args = append(args, lang.Image)
	args = append(args, lang.Command...)

	return execute(runCtx, req, Result{
		Language: lang.ID,
		Engine:   "docker:" + lang.Image,
		Isolated: true,
	}, d.binary, args, "")
}

/* ---------------- 本机执行(不隔离) ---------------- */

// LocalRunner 直接在本机执行代码。
//
// 它存在的唯一理由是"让开发机和 CI 在没有容器运行时也能跑通判题链路"。
// 它**不是安全边界**: 候选人可以读写本机文件、发网络请求、fork 进程。
// 因此 Isolated() 返回 false, 调用方必须把这条事实写进响应与日志,
// 生产环境也应当拒绝启用它。
type LocalRunner struct {
	sem chan struct{}
}

// NewLocalRunner 构造本机执行器。
func NewLocalRunner() *LocalRunner {
	return &LocalRunner{sem: make(chan struct{}, 2)}
}

// Name 返回执行引擎名。
func (l *LocalRunner) Name() string { return "local" }

// Isolated 恒为 false。这是本实现最重要的一个返回值。
func (l *LocalRunner) Isolated() bool { return false }

// Run 在本机执行代码。
func (l *LocalRunner) Run(ctx context.Context, req Request) (Result, error) {
	lang, err := LookupLanguage(req.Language)
	if err != nil {
		return Result{}, err
	}
	if len(req.Code) > MaxCodeBytes {
		return Result{}, ErrTooLarge
	}

	select {
	case l.sem <- struct{}{}:
		defer func() { <-l.sem }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}

	dir, err := os.MkdirTemp("", "interview-sandbox-*")
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, lang.Filename), []byte(req.Code), 0o644); err != nil {
		return Result{}, err
	}

	bin := lang.LocalCommand[0]
	if _, err := exec.LookPath(bin); err != nil {
		return Result{}, fmt.Errorf("sandbox: 本机没有 %s 工具链, 无法执行 %s", bin, lang.ID)
	}
	args := lang.LocalCommand[1:]

	timeout := effectiveTimeout(req.Timeout, ctx)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return execute(runCtx, req, Result{
		Language: lang.ID,
		Engine:   "local:" + bin,
		Isolated: false,
	}, bin, args, dir)
}

/* ---------------- 执行与判题 ---------------- */

func effectiveTimeout(requested time.Duration, ctx context.Context) time.Duration {
	if requested <= 0 || requested > DefaultTimeout {
		requested = DefaultTimeout
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < requested {
			return remaining
		}
	}
	return requested
}

func execute(ctx context.Context, req Request, base Result, bin string, args []string, dir string) (Result, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return base, err
	}
	stdout := newLimitedBuffer(MaxOutputBytes)
	stderr := newLimitedBuffer(MaxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// 新进程组: 超时时可以整组杀掉。只杀父进程的话, 候选人 fork 出来的
	// 子进程会继续跑, 而面试官看到的只是"程序超时了"。
	cmd.SysProcAttr = procAttr()
	// Cancel 覆盖默认的"只杀进程本身"; WaitDelay 保证即使子进程
	// 抓住 stdout 不放, Wait 也会在 2 秒后返回, 不会永久挂住。
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return base, err
	}
	// 写入 stdin 可能因为对端不读而阻塞(例如程序立刻退出), 因此单独一个
	// goroutine 写, 主流程只管等待退出与超时。
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = stdin.Write([]byte(req.Stdin))
		_ = stdin.Close()
	}()

	waitErr := cmd.Wait()
	<-done

	base.DurationMS = time.Since(start).Milliseconds()
	base.Stdout = stdout.String()
	base.Stderr = stderr.String()
	base.Truncated = stdout.Truncated() || stderr.Truncated()
	base.ExitCode = exitCode(cmd, waitErr)
	if ctx.Err() == context.DeadlineExceeded {
		base.TimedOut = true
	}
	if waitErr != nil && base.ExitCode < 0 {
		base.Stderr = strings.TrimSpace(base.Stderr + "\n" + waitErr.Error())
	}
	return base, nil
}

// Judge 用一组用例判定提交的代码。
//
// 每个用例单独跑一次进程: 用同一进程喂多个输入需要候选人自己写循环,
// 而面试题通常是"读输入-输出结果"的单次程序。多跑的进程开销换来的
// 是"哪个用例挂了"这种能直接反馈给候选人的信息。
func Judge(ctx context.Context, runner Runner, req Request, cases []TestCase) ([]CaseResult, Result) {
	results := make([]CaseResult, 0, len(cases))
	var last Result
	for _, tc := range cases {
		r, err := runner.Run(ctx, Request{
			Language: req.Language, Code: req.Code, Stdin: tc.Stdin, Timeout: req.Timeout,
		})
		if err != nil {
			results = append(results, CaseResult{
				Name:  tc.Name,
				Error: err.Error(),
			})
			continue
		}
		last = r
		cr := CaseResult{
			Name:       tc.Name,
			Actual:     r.Stdout,
			DurationMS: r.DurationMS,
		}
		switch {
		case r.TimedOut:
			cr.Error = "执行超时"
		case r.ExitCode != 0:
			cr.Error = strings.TrimSpace(r.Stderr)
			if cr.Error == "" {
				cr.Error = fmt.Sprintf("进程退出码 %d", r.ExitCode)
			}
		default:
			cr.Passed = normalizeOutput(r.Stdout) == normalizeOutput(tc.ExpectedStdout)
		}
		if !tc.Hidden {
			cr.Expected = tc.ExpectedStdout
		}
		results = append(results, cr)
	}
	return results, last
}

// normalizeOutput 让判题容忍末尾空白与 CRLF。
//
// 不加这一层, 候选人会反复栽在"答案是对的但行尾多了个空行"上,
// 而这跟考察目标毫无关系。
func normalizeOutput(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	out := strings.Join(lines, "\n")
	return strings.TrimRight(out, "\n")
}

// limitedBuffer 是带上限的写入缓冲。
type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newLimitedBuffer(limit int) *limitedBuffer {
	return &limitedBuffer{limit: limit}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		// 返回 len(p) 而不是错误: 绝不能因为"输出太多"让候选人拿到一个
		// 莫名其妙的 write error, 那会把考察点从逻辑变成猜我们的实现。
		return len(p), nil
	}
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *limitedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

// exitCode 从 ProcessState 提取退出码。
func exitCode(cmd *exec.Cmd, waitErr error) int {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	if waitErr != nil {
		return -1
	}
	return 0
}

// AutoRunner 按可用性自动选择执行引擎。
//
// 优先级: 显式指定的引擎 > 容器 > 本机(附告警)。把"用哪个引擎"
// 的决定集中在这里, 是为了让调用方无法无意中绕过容器。
func AutoRunner(ctx context.Context, prefer string, onFallback func(string)) Runner {
	switch strings.ToLower(prefer) {
	case "local":
		return NewLocalRunner()
	case "docker":
		return NewDockerRunner("")
	}
	d := NewDockerRunner("")
	if d.Available(ctx) {
		return d
	}
	if onFallback != nil {
		onFallback("容器运行时不可用, 判题降级为在本机执行(不具备隔离能力, 仅限本地开发)")
	}
	return NewLocalRunner()
}
