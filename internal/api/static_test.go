package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 这三个用例守的是同一件事: 换掉二进制之后, 浏览器必须能立刻拿到新前端。
//
// 如果没有 ETag 与强制重校验, 浏览器会继续用缓存里的旧 index.html,
// 而旧 HTML 引用的是旧版本的文件名 —— 表现是"刷新之后整页白屏,
// 而且怎么刷都不恢复"。这类问题在开发机上很难复现(开发时通常禁用了缓存),
// 一到真实使用就出现, 所以必须用测试固定住行为。
//
// 这些用例刻意**不启动真实 HTTP 服务**: 直接用 ResponseRecorder 调处理器。
// 一是更快, 二是很多受限环境(CI 沙箱、无网络权限的开发机)根本不允许绑定
// 端口, 而"静态资源的响应头对不对"跟"能不能监听 socket"毫无关系。

func getStatic(t *testing.T, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	StaticHandler().ServeHTTP(rec, req)
	return rec
}

func TestStaticHandlerSendsRevalidatingCacheHeaders(t *testing.T) {
	resp := getStatic(t, "/", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("首页应返回 200, 实际 %d", resp.Code)
	}
	etag := resp.Header().Get("ETag")
	if etag == "" {
		t.Fatal("必须带 ETag: 否则浏览器无法判断内嵌前端是否已更新")
	}
	// 入口 HTML 不允许任何缓存: 它引用的资源名会随版本变化。
	if cc := resp.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("入口 HTML 应禁止缓存, 实际 Cache-Control=%q", cc)
	}
	if body := resp.Body.String(); !strings.Contains(body, "AI Interview OS") {
		t.Fatal("首页应返回面试前端页面")
	}

	// 静态模块允许缓存但必须重校验。
	jsResp := getStatic(t, "/js/app.js", nil)
	if jsResp.Code != http.StatusOK {
		t.Fatalf("前端模块应返回 200, 实际 %d", jsResp.Code)
	}
	if cc := jsResp.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Fatalf("前端模块应要求重校验, 实际 Cache-Control=%q", cc)
	}
	if ct := jsResp.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("ES 模块必须以 javascript 类型返回, 否则浏览器拒绝执行: %q", ct)
	}
	if body, _ := io.ReadAll(jsResp.Body); len(body) == 0 {
		t.Fatal("前端模块内容为空")
	}
}

func TestStaticHandlerReturns304WhenContentUnchanged(t *testing.T) {
	first := getStatic(t, "/js/core.js", nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("静态模块也应该带 ETag")
	}
	second := getStatic(t, "/js/core.js", map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("资源未变时应返回 304, 实际 %d", second.Code)
	}
}

func TestStaticHandlerETagChangesWithContent(t *testing.T) {
	// ETag 必须由"整份内嵌资源"决定: 任何一次重新构建都会改变它。
	// 这里不直接改文件, 而是验证它与内容哈希一致(而不是某个固定的常量),
	// 否则内嵌前端更新后浏览器仍会命中 304。
	if staticETag == "" || !strings.HasPrefix(staticETag, `"`) {
		t.Fatalf("ETag 格式不正确: %q", staticETag)
	}
	if staticETag != computeStaticETag() {
		t.Fatal("ETag 必须由当前内嵌资源决定")
	}
}

func TestETagMatchingHandlesListsAndWeakTags(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{`"abc"`, true},
		{`"xxx", "abc"`, true},
		{`W/"abc"`, true},
		{`*`, true},
		{`"yyy"`, false},
	}
	for _, c := range cases {
		if got := etagMatches(c.header, `"abc"`); got != c.want {
			t.Errorf("etagMatches(%q) = %v, 期望 %v", c.header, got, c.want)
		}
	}
}
