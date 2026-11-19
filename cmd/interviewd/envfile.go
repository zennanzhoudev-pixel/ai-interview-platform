package main

import (
	"bufio"
	"os"
	"strings"
)

// loadDotEnv 从当前目录的 .env 读取 KEY=VALUE, 只设置尚未存在的环境变量。
//
// 为什么需要它: 配置写在终端里的 export 只对那个终端会话有效,
// 换一个终端重启服务, 所有密钥就"消失"了 —— 表现是面板显示未配置,
// 但用户明明配置过。把配置落到 .env(已在 .gitignore 中) 之后,
// 启动路径不再依赖某个终端的寿命。已存在的环境变量优先, 便于临时覆盖。
func loadDotEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		_ = os.Setenv(key, val)
	}
}
