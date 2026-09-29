package account

import (
	"context"
	"os"
	"testing"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// MySQL 契约测试需要真实数据库, 通过 MYSQL_DSN 开启:
//
//	make docker-up
//	MYSQL_DSN='root:root@tcp(127.0.0.1:3306)/interview?parseTime=true&loc=UTC' \
//	  go test ./internal/account/... -run MySQL -v
//
// 默认跳过而不是失败: 单元测试必须在没有外部依赖的机器上也能跑通,
// 否则 CI 会因为环境差异一片红, 最后没人再认真看测试结果。
//
// 这份契约存在的意义: 账号的坑几乎全在"内存实现不会遇到、MySQL 会遇到"
// 的地方 —— 唯一键与 NULL 的语义、RowsAffected 的含义、浮点向量的往返。
// 只跑内存实现的测试会给出一份虚假的安全感。
func TestMySQLStoreSatisfiesAccountContract(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 MYSQL_DSN, 跳过账号的 MySQL 契约测试")
	}
	accountContract(t, func(t *testing.T) Store {
		t.Helper()
		// 复用 store 包的连接与建表逻辑: 表结构的唯一来源是 schema.sql,
		// 这里再写一份 DDL 就一定会和它漂移。
		my, err := store.OpenMySQL(dsn)
		if err != nil {
			t.Fatalf("连接 MySQL 失败: %v", err)
		}
		ctx := context.Background()
		if err := my.Migrate(ctx); err != nil {
			t.Fatalf("建表失败: %v", err)
		}
		resetAccountTables(t, my)
		t.Cleanup(func() { _ = my.Close() })
		return NewMySQLStore(my.DB())
	})
}

// resetAccountTables 在每组用例前清空账号相关的表。
// 外键不存在, 但删除顺序仍然按"从属到主"来, 便于以后加外键时不用改这里。
func resetAccountTables(t *testing.T, my *store.MySQLStore) {
	t.Helper()
	for _, table := range []string{"account_login", "account_face", "account_user"} {
		if _, err := my.DB().Exec("DELETE FROM " + table); err != nil {
			t.Fatalf("清理表 %s 失败: %v", table, err)
		}
	}
}
