// Package platform 放与具体业务无关的运行时保障能力: panic 兜底、结构化日志。
package platform

import (
	"context"
	"log/slog"
	"runtime/debug"
)

// PanicHandler 在捕获 panic 后回调, 用于打点(进程内 panic 计数)。
type PanicHandler func(name string)

// Go 启动一个带 panic 兜底的 goroutine。
//
// 为什么必须有这个包装: Go 的 http.Server 只会 recover 处理器函数里的 panic,
// 对业务自己 go 出去的 goroutine 毫无保护 —— 那些 goroutine 一旦 panic,
// 整个进程直接退出。放在面试系统里, 就是"一个候选人的一次异常把所有正在
// 进行的面试全部打断"。企业级服务必须做到故障隔离, 这层兜底是最低要求。
func Go(ctx context.Context, logger *slog.Logger, name string, onPanic PanicHandler, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if logger != nil {
					logger.ErrorContext(ctx, "goroutine panic recovered",
						slog.String("goroutine", name),
						slog.Any("panic", r),
						slog.String("stack", string(debug.Stack())),
					)
				}
				if onPanic != nil {
					onPanic(name)
				}
			}
		}()
		fn()
	}()
}

// Recover 把当前 goroutine 里的 panic 转成日志, 供 defer 直接调用。
// 返回 true 表示确实捕获到了 panic。
func Recover(ctx context.Context, logger *slog.Logger, name string, onPanic PanicHandler) bool {
	r := recover()
	if r == nil {
		return false
	}
	if logger != nil {
		logger.ErrorContext(ctx, "panic recovered",
			slog.String("scope", name),
			slog.Any("panic", r),
			slog.String("stack", string(debug.Stack())),
		)
	}
	if onPanic != nil {
		onPanic(name)
	}
	return true
}
