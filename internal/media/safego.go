package media

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/platform"
)

var (
	panicLogger atomic.Pointer[slog.Logger]
	panicHook   atomic.Pointer[func(string)]
)

// SetPanicLogger 注入日志器。由接入层在启动时调用一次。
func SetPanicLogger(l *slog.Logger) { panicLogger.Store(l) }

// SetPanicHook 注入 panic 回调, 用于打点。
func SetPanicHook(fn func(string)) {
	if fn == nil {
		return
	}
	panicHook.Store(&fn)
}

// safeGo 启动带 panic 兜底的 goroutine。
//
// 本包里有 5 处自研 goroutine(事件泵、ASR 消费、TTS/ASR 流消费)。
// 它们一旦 panic, 整个进程直接退出 —— 一次第三方 SDK 的空指针会把
// 所有正在进行的面试一起打断。故障隔离是硬要求。
func safeGo(ctx context.Context, name string, fn func()) {
	var hook platform.PanicHandler
	if p := panicHook.Load(); p != nil {
		hook = *p
	}
	platform.Go(ctx, panicLogger.Load(), name, hook, fn)
}
