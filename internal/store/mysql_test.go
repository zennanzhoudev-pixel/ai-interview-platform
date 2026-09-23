package store

import (
	"context"
	"os"
	"testing"
)

// MySQL 集成测试需要真实数据库, 通过 MYSQL_DSN 开启:
//
//	docker compose up -d mysql
//	MYSQL_DSN='root:root@tcp(127.0.0.1:3306)/interview?parseTime=true&loc=UTC' \
//	  go test ./internal/store/... -run MySQL -v
//
// 默认跳过而不是失败: 单元测试必须在没有外部依赖的机器上也能跑通,
// 否则 CI 会因为环境差异一片红, 最后没人再认真看测试结果。
func TestMySQLStoreSatisfiesContract(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 MYSQL_DSN, 跳过 MySQL 集成测试")
	}

	storeContract(t, func(t *testing.T) SessionStore {
		s, err := OpenMySQL(dsn)
		if err != nil {
			t.Fatalf("连接 MySQL 失败: %v", err)
		}
		ctx := context.Background()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("初始化表结构失败: %v", err)
		}
		resetMySQL(t, s)
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

// resetMySQL 在每组用例前清空数据, 保证用例之间互不影响。
func resetMySQL(t *testing.T, s *MySQLStore) {
	t.Helper()
	for _, table := range []string{"qa_turn", "interview_report", "candidate_consent", "interview_session"} {
		if _, err := s.db.Exec("DELETE FROM " + table); err != nil {
			t.Fatalf("清理表 %s 失败: %v", table, err)
		}
	}
}

func TestSplitStatementsYieldsExecutableSQL(t *testing.T) {
	stmts := splitStatements(Schema())
	if len(stmts) < 5 {
		t.Fatalf("建表脚本应至少包含 5 条语句, 实际 %d 条", len(stmts))
	}
	for i, stmt := range stmts {
		if stmt == "" {
			t.Fatalf("第 %d 条语句为空", i)
		}
		if len(stmt) < 20 {
			t.Fatalf("第 %d 条语句过短, 可能是拆分错误: %q", i, stmt)
		}
	}
}
