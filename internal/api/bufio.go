package api

import "bufio"

// bufio_ReadWriter 是 http.Hijacker 返回类型的别名。
// 单独放一个文件是为了让 middleware.go 的 import 列表保持整洁。
type bufio_ReadWriter = bufio.ReadWriter
