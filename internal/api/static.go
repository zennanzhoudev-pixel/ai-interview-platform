package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"sort"
	"strings"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/web"
)

// 前端静态资源的响应头。
//
// 这里要解决的是一个真实发生过的问题: 前端被编译进二进制, 而浏览器会
// 按普通静态资源缓存 index.html。于是"换了二进制、重启了服务"之后,
// 浏览器可能仍然拿着旧 HTML, 而旧 HTML 引用的是旧的文件名(例如 /app.js),
// 那个文件在新版本里已经不存在 —— 结果是刷新之后整页白屏, 而且怎么刷
// 都不恢复, 因为浏览器压根没有再向服务端要新 HTML。
//
// 处理方式:
//  1. 用整份内嵌资源的哈希做 ETag。任何一次重新构建都会改变它,
//     因此"代码变了"和"浏览器知道代码变了"是同一件事;
//  2. Cache-Control 用 no-cache(而不是 no-store): 允许缓存, 但每次都必须
//     回来校验。未改动时命中 304, 只花几十字节; 改动后立刻拿到新文件。
//     no-store 虽然更暴力, 却会让我们每次刷新都重传全部模块。
//
// 生产环境如果接入 CDN, 更常见的做法是给静态文件名加内容哈希
// (app.<hash>.js) 并长期缓存。这里刻意不那么做: 本项目的前端是"零构建"
// 的, 引入文件名哈希就意味着引入构建步骤, 而那正是这套前端想避开的东西。
var staticETag = computeStaticETag()

func computeStaticETag() string {
	h := sha256.New()
	// 目录遍历顺序在不同平台上可能不同, 因此先收集路径并排序,
	// 保证同一份代码在任何机器上算出的 ETag 都一致。
	var paths []string
	_ = fs.WalkDir(web.FS, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	sort.Strings(paths)
	for _, path := range paths {
		data, err := fs.ReadFile(web.FS, path)
		if err != nil {
			continue
		}
		_, _ = h.Write([]byte(path))
		sum := sha256.Sum256(data)
		_, _ = h.Write(sum[:])
	}
	return `"` + hex.EncodeToString(h.Sum(nil))[:20] + `"`
}

// StaticHandler 返回内嵌前端资源的处理器。
func StaticHandler() http.Handler {
	fileServer := http.FileServer(http.FS(web.FS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HEAD 请求也要带上同样的响应头, 否则用 HEAD 做健康检查的
		// 客户端会以为这里没有 ETag。
		w.Header().Set("ETag", staticETag)
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")

		// 资源没变就直接 304, 省掉一次完整传输。
		if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, staticETag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		// index.html 是入口, 一旦被缓存成旧版就会引用到不存在的旧资源名,
		// 因此对它再收紧一档: 不允许任何中间层缓存。
		if r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, "/index.html") {
			w.Header().Set("Cache-Control", "no-store")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// etagMatches 按 RFC 9110 处理 If-None-Match: 支持逗号分隔列表与 W/ 前缀。
func etagMatches(header, etag string) bool {
	if strings.TrimSpace(header) == "*" {
		return true
	}
	weak := strings.TrimPrefix(etag, "W/")
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == etag || strings.TrimPrefix(candidate, "W/") == weak {
			return true
		}
	}
	return false
}
