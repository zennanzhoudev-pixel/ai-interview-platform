package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

//go:embed schema.sql
var schemaSQL string

// Schema 返回建表脚本, 便于外部工具(如 docker initdb)复用同一份定义。
func Schema() string { return schemaSQL }

// MySQLStore 是业务主数据的生产实现。
//
// 分库分表说明: 生产环境按 tenant_id 分库、按 session_id 分表,
// 一个 MySQLStore 实例只服务一个分片, 分片路由属于接入层职责。
// 这样存储层不需要知道集群拓扑, 也避免了"业务代码里到处拼分片键"
// 这个几乎必然会写错的模式。
type MySQLStore struct {
	db *sql.DB
}

// OpenMySQL 连接 MySQL 并验证连通性。
//
// 显式设置连接池参数而不是用默认值: database/sql 默认的最大连接数
// 是"无限制", 秋招峰值下会把数据库连接数瞬间吃光, 而故障表现是
// "所有服务都连不上库", 根因极难在第一时间定位。
func OpenMySQL(dsn string) (*MySQLStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: 连接 MySQL 失败: %w", err)
	}
	return &MySQLStore{db: db}, nil
}

// Migrate 执行建表语句。
//
// 生产环境应该用独立的迁移工具管理 schema 变更; 这里提供 Migrate
// 是为了让本地和集成测试能一键起库, 不必再引入一个依赖。
func (m *MySQLStore) Migrate(ctx context.Context) error {
	for _, stmt := range splitStatements(schemaSQL) {
		if _, err := m.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: 执行建表语句失败: %w\n%s", err, stmt)
		}
	}
	return nil
}

func (m *MySQLStore) Close() error { return m.db.Close() }

func (m *MySQLStore) CreateSession(ctx context.Context, s Session) error {
	if s.ID == "" {
		return errors.New("store: 会话 ID 不能为空")
	}
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.Status == "" {
		s.Status = StatusRunning
	}

	_, err := m.db.ExecContext(ctx, `
		INSERT INTO interview_session
			(session_id, tenant_id, round, minutes, stage, status, recommendation, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		s.ID, defaultTenant(s.TenantID), s.Round, s.Minutes, s.Stage,
		string(s.Status), s.Recommendation, s.CreatedAt.UTC(), now)
	if err != nil {
		var myErr *mysqldriver.MySQLError
		if errors.As(err, &myErr) && myErr.Number == 1062 {
			return fmt.Errorf("store: 会话 %s 已存在", s.ID)
		}
		return err
	}
	return nil
}

func (m *MySQLStore) UpdateSession(ctx context.Context, s Session) error {
	res, err := m.db.ExecContext(ctx, `
		UPDATE interview_session
		SET stage=?, status=?, recommendation=?, minutes=?, updated_at=?
		WHERE session_id=?`,
		s.Stage, string(s.Status), s.Recommendation, s.Minutes, time.Now().UTC(), s.ID)
	if err != nil {
		return err
	}

	// MySQL 在"新值与旧值完全相同"时返回 0 行受影响, 但这并不代表
	// 记录不存在。直接拿 RowsAffected 判存在性, 会把幂等重放误判成失败 ——
	// 这类 bug 在本地手工测试里几乎不可能复现, 只在线上重试时偶发。
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		if _, err := m.GetSession(ctx, s.ID); err != nil {
			return err
		}
	}
	return nil
}

func (m *MySQLStore) GetSession(ctx context.Context, id string) (Session, error) {
	var (
		s      Session
		status string
	)
	err := m.db.QueryRowContext(ctx, `
		SELECT session_id, tenant_id, round, minutes, stage, status,
		       recommendation, created_at, updated_at
		FROM interview_session WHERE session_id=?`, id).
		Scan(&s.ID, &s.TenantID, &s.Round, &s.Minutes, &s.Stage, &status,
			&s.Recommendation, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	s.Status = SessionStatus(status)
	return s, nil
}

func (m *MySQLStore) ListSessions(ctx context.Context, tenantID string, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
		SELECT session_id, tenant_id, round, minutes, stage, status,
		       recommendation, created_at, updated_at
		FROM interview_session`
	var args []any
	if tenantID != "" {
		query += " WHERE tenant_id=?"
		args = append(args, tenantID)
	}
	query += " ORDER BY created_at DESC, session_id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var (
			s      Session
			status string
		)
		if err := rows.Scan(&s.ID, &s.TenantID, &s.Round, &s.Minutes, &s.Stage,
			&status, &s.Recommendation, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.Status = SessionStatus(status)
		out = append(out, s)
	}
	return out, rows.Err()
}

func (m *MySQLStore) AppendTurn(ctx context.Context, t Turn) error {
	createdAt := t.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	var evidence any
	if len(t.Evidence) > 0 {
		evidence = string(t.Evidence)
	}

	// ON DUPLICATE KEY UPDATE 让重复投递变成无副作用的空操作:
	// 消息队列至少一次投递是常态, 靠应用层"先查再写"既慢又有竞态。
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO qa_turn
			(session_id, turn_index, stage, question_id, competency, question, answer,
			 duration_ms, is_probe, scored, level, level_num, confidence,
			 degraded_from, evidence, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE session_id = session_id`,
		t.SessionID, t.Index, t.Stage, t.QuestionID, t.Competency, t.Question, t.Answer,
		t.DurationMS, t.IsProbe, t.Scored, t.Level, t.LevelNum, t.Confidence,
		t.DegradedFrom, evidence, createdAt.UTC())
	return err
}

func (m *MySQLStore) ListTurns(ctx context.Context, sessionID string) ([]Turn, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT session_id, turn_index, stage, question_id, competency, question, answer,
		       duration_ms, is_probe, scored, level, level_num, confidence,
		       degraded_from, evidence, created_at
		FROM qa_turn WHERE session_id=? ORDER BY turn_index`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Turn
	for rows.Next() {
		var (
			t        Turn
			evidence []byte
		)
		if err := rows.Scan(&t.SessionID, &t.Index, &t.Stage, &t.QuestionID, &t.Competency,
			&t.Question, &t.Answer, &t.DurationMS, &t.IsProbe, &t.Scored,
			&t.Level, &t.LevelNum, &t.Confidence, &t.DegradedFrom, &evidence, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Evidence = evidence
		out = append(out, t)
	}
	return out, rows.Err()
}

func (m *MySQLStore) SaveReport(ctx context.Context, r Report) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO interview_report (session_id, recommendation, confidence, payload, created_at)
		VALUES (?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			recommendation=VALUES(recommendation),
			confidence=VALUES(confidence),
			payload=VALUES(payload)`,
		r.SessionID, r.Recommendation, r.Confidence, string(r.Payload), r.CreatedAt.UTC())
	return err
}

func (m *MySQLStore) GetReport(ctx context.Context, sessionID string) (Report, error) {
	var (
		r       Report
		payload []byte
	)
	err := m.db.QueryRowContext(ctx, `
		SELECT session_id, recommendation, confidence, payload, created_at
		FROM interview_report WHERE session_id=?`, sessionID).
		Scan(&r.SessionID, &r.Recommendation, &r.Confidence, &payload, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Report{}, ErrNotFound
	}
	if err != nil {
		return Report{}, err
	}
	r.Payload = payload
	return r, nil
}

func (m *MySQLStore) SaveConsent(ctx context.Context, c Consent) error {
	if c.AgreedAt.IsZero() {
		c.AgreedAt = time.Now().UTC()
	}
	// INSERT IGNORE + 主键(session_id, scope): 重复授权保留最早那一条。
	// 授权时间的先后本身有法律意义, 不能被后来的请求覆盖。
	_, err := m.db.ExecContext(ctx, `
		INSERT IGNORE INTO candidate_consent
			(session_id, candidate_id, scope, agreed_at, ip, user_agent)
		VALUES (?,?,?,?,?,?)`,
		c.SessionID, c.CandidateID, c.Scope, c.AgreedAt.UTC(), c.IP, c.UserAgent)
	return err
}

func (m *MySQLStore) ListConsents(ctx context.Context, sessionID string) ([]Consent, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT session_id, candidate_id, scope, agreed_at, ip, user_agent
		FROM candidate_consent WHERE session_id=? ORDER BY agreed_at`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Consent
	for rows.Next() {
		var c Consent
		if err := rows.Scan(&c.SessionID, &c.CandidateID, &c.Scope, &c.AgreedAt, &c.IP, &c.UserAgent); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func defaultTenant(t string) string {
	if t == "" {
		return "default"
	}
	return t
}

// splitStatements 把建表脚本拆成单条语句。
// 脚本里不包含存储过程或触发器, 所以按分号切分是安全的;
// 同时剥掉以 -- 开头的注释行, 让日志里打出来的语句可以直接拷贝执行。
func splitStatements(sqlText string) []string {
	var out []string
	for _, chunk := range strings.Split(sqlText, ";") {
		var lines []string
		for _, line := range strings.Split(chunk, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "--") {
				continue
			}
			lines = append(lines, line)
		}
		if stmt := strings.TrimSpace(strings.Join(lines, "\n")); stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}
