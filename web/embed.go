// Package web 提供面试前端静态资源。
//
// 前端不使用任何构建工具、不依赖任何 CDN: 一个 HTML + 一个 JS + 一个 CSS,
// 通过 go:embed 打进二进制。这样做的好处很实际 —— 部署只有一个文件,
// 也不会因为外网 CDN 不可达导致面试页面白屏。面试场景下白屏就是事故,
// 而"前端依赖 CDN"恰恰是最容易被忽略的可用性单点。
package web

import "embed"

// 前端资源清单显式列出, 不使用 all:. 这样"多打了一个调试文件进二进制"
// 会立刻在 code review 里被发现, 而不是悄悄变大。
//
//go:embed index.html style.css js
var FS embed.FS
