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
// 因此本包所有查询都显式带 tenant_id —— 即便在不分片的部署里,
// 这也是租户隔离的最后一道防线。
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

// DB 暴露底层连接句柄, 供同进程内的其他持久化组件(如 API Key 库)复用连接池。
func (m *MySQLStore) DB() *sql.DB { return m.db }

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
	if s.TenantID == "" {
		return errors.New("store: 租户 ID 不能为空")
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
			(session_id, tenant_id, position, company, candidate_name, candidate_ref, interviewer_name,
			 application_id, resume_json, round, minutes, stage, status, recommendation, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.TenantID, s.Position, s.Company, s.CandidateName, s.CandidateRef, s.InterviewerName,
		s.ApplicationID, nullableJSON(s.ResumeJSON), s.Round, s.Minutes, s.Stage,
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

// CreateSessionWithConsents 用事务保证"建会话 + 记授权"同生共死。
func (m *MySQLStore) CreateSessionWithConsents(ctx context.Context, s Session, consents []Consent) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.Status == "" {
		s.Status = StatusRunning
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO interview_session
			(session_id, tenant_id, position, company, candidate_name, candidate_ref, interviewer_name,
			 application_id, resume_json, round, minutes, stage, status, recommendation, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.TenantID, s.Position, s.Company, s.CandidateName, s.CandidateRef, s.InterviewerName,
		s.ApplicationID, nullableJSON(s.ResumeJSON), s.Round, s.Minutes, s.Stage,
		string(s.Status), s.Recommendation, s.CreatedAt.UTC(), now)
	if err != nil {
		var myErr *mysqldriver.MySQLError
		if errors.As(err, &myErr) && myErr.Number == 1062 {
			return fmt.Errorf("store: 会话 %s 已存在", s.ID)
		}
		return err
	}

	for _, c := range consents {
		agreedAt := c.AgreedAt
		if agreedAt.IsZero() {
			agreedAt = now
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT IGNORE INTO candidate_consent
				(tenant_id, session_id, candidate_id, scope, agreed_at, ip, user_agent)
			VALUES (?,?,?,?,?,?,?)`,
			c.TenantID, c.SessionID, c.CandidateID, c.Scope, agreedAt.UTC(), c.IP, c.UserAgent); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FinishSession 用事务保证"报告落库 + 会话置为完成"一致。
func (m *MySQLStore) FinishSession(ctx context.Context, s Session, r Report) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO interview_report (tenant_id, session_id, recommendation, confidence, score, payload, created_at)
		VALUES (?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			recommendation=VALUES(recommendation),
			confidence=VALUES(confidence),
			score=VALUES(score),
			payload=VALUES(payload)`,
		r.TenantID, r.SessionID, r.Recommendation, r.Confidence, r.Score, string(r.Payload), r.CreatedAt.UTC()); err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE interview_session
		SET stage=?, status=?, recommendation=?, minutes=?, updated_at=?
		WHERE session_id=? AND tenant_id=?`,
		s.Stage, string(s.Status), s.Recommendation, s.Minutes, now, s.ID, s.TenantID)
	if err != nil {
		return err
	}
	// MySQL 在"新值与旧值完全相同"时返回 0 行受影响, 这不代表记录不存在。
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM interview_session WHERE session_id=? AND tenant_id=?`,
			s.ID, s.TenantID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
	}
	return tx.Commit()
}

func (m *MySQLStore) UpdateSession(ctx context.Context, s Session) error {
	res, err := m.db.ExecContext(ctx, `
		UPDATE interview_session
		SET stage=?, status=?, recommendation=?, minutes=?, updated_at=?
		WHERE session_id=? AND tenant_id=?`,
		s.Stage, string(s.Status), s.Recommendation, s.Minutes, time.Now().UTC(), s.ID, s.TenantID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		if _, err := m.GetSession(ctx, s.TenantID, s.ID); err != nil {
			return err
		}
	}
	return nil
}

const sessionColumns = `session_id, tenant_id, position, company, candidate_name, candidate_ref,
	interviewer_name, application_id, resume_json, round, minutes, stage, status, recommendation,
	created_at, updated_at`

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var (
		s      Session
		status string
	)
	err := row.Scan(&s.ID, &s.TenantID, &s.Position, &s.Company, &s.CandidateName, &s.CandidateRef,
		&s.InterviewerName, &s.ApplicationID, &s.ResumeJSON, &s.Round, &s.Minutes, &s.Stage, &status,
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

func (m *MySQLStore) GetSession(ctx context.Context, tenantID, sessionID string) (Session, error) {
	return scanSession(m.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM interview_session WHERE session_id=? AND tenant_id=?`,
		sessionID, tenantID))
}

func (m *MySQLStore) ListSessions(ctx context.Context, tenantID string, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM interview_session
		 WHERE tenant_id=? ORDER BY created_at DESC, session_id DESC LIMIT ?`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (m *MySQLStore) AppendTurn(ctx context.Context, t Turn) error {
	// 先确认会话属于该租户: 否则就是跨租户写入。
	if _, err := m.GetSession(ctx, t.TenantID, t.SessionID); err != nil {
		return err
	}
	createdAt := t.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	var verdict any
	if len(t.Verdict) > 0 {
		verdict = string(t.Verdict)
	}

	// ON DUPLICATE KEY UPDATE 让重复投递变成无副作用的空操作。
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO qa_turn
			(tenant_id, session_id, turn_index, stage, question_id, competency, question, answer,
			 duration_ms, is_probe, scored, level, level_num, confidence, degraded_from, verdict, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE session_id = session_id`,
		t.TenantID, t.SessionID, t.Index, t.Stage, t.QuestionID, t.Competency, t.Question, t.Answer,
		t.DurationMS, t.IsProbe, t.Scored, t.Level, t.LevelNum, t.Confidence,
		t.DegradedFrom, verdict, createdAt.UTC())
	return err
}

func (m *MySQLStore) ListTurns(ctx context.Context, tenantID, sessionID string) ([]Turn, error) {
	if _, err := m.GetSession(ctx, tenantID, sessionID); err != nil {
		return nil, err
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT tenant_id, session_id, turn_index, stage, question_id, competency, question, answer,
		       duration_ms, is_probe, scored, level, level_num, confidence, degraded_from, verdict, created_at
		FROM qa_turn WHERE tenant_id=? AND session_id=? ORDER BY turn_index`, tenantID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Turn
	for rows.Next() {
		var (
			t       Turn
			verdict []byte
		)
		if err := rows.Scan(&t.TenantID, &t.SessionID, &t.Index, &t.Stage, &t.QuestionID, &t.Competency,
			&t.Question, &t.Answer, &t.DurationMS, &t.IsProbe, &t.Scored,
			&t.Level, &t.LevelNum, &t.Confidence, &t.DegradedFrom, &verdict, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Verdict = verdict
		out = append(out, t)
	}
	return out, rows.Err()
}

func (m *MySQLStore) SaveReport(ctx context.Context, r Report) error {
	if _, err := m.GetSession(ctx, r.TenantID, r.SessionID); err != nil {
		return err
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO interview_report (tenant_id, session_id, recommendation, confidence, score, payload, created_at)
		VALUES (?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			recommendation=VALUES(recommendation),
			confidence=VALUES(confidence),
			score=VALUES(score),
			payload=VALUES(payload)`,
		r.TenantID, r.SessionID, r.Recommendation, r.Confidence, r.Score, string(r.Payload), r.CreatedAt.UTC())
	return err
}

func (m *MySQLStore) GetReport(ctx context.Context, tenantID, sessionID string) (Report, error) {
	var (
		r       Report
		payload []byte
	)
	err := m.db.QueryRowContext(ctx, `
		SELECT tenant_id, session_id, recommendation, confidence, score, payload, created_at
		FROM interview_report WHERE tenant_id=? AND session_id=?`, tenantID, sessionID).
		Scan(&r.TenantID, &r.SessionID, &r.Recommendation, &r.Confidence, &r.Score, &payload, &r.CreatedAt)
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
	if _, err := m.GetSession(ctx, c.TenantID, c.SessionID); err != nil {
		return err
	}
	if c.AgreedAt.IsZero() {
		c.AgreedAt = time.Now().UTC()
	}
	// INSERT IGNORE + 唯一键(tenant_id, session_id, scope): 重复授权保留最早那条。
	_, err := m.db.ExecContext(ctx, `
		INSERT IGNORE INTO candidate_consent
			(tenant_id, session_id, candidate_id, scope, agreed_at, ip, user_agent)
		VALUES (?,?,?,?,?,?,?)`,
		c.TenantID, c.SessionID, c.CandidateID, c.Scope, c.AgreedAt.UTC(), c.IP, c.UserAgent)
	return err
}

func (m *MySQLStore) ListConsents(ctx context.Context, tenantID, sessionID string) ([]Consent, error) {
	if _, err := m.GetSession(ctx, tenantID, sessionID); err != nil {
		return nil, err
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT tenant_id, session_id, candidate_id, scope, agreed_at, ip, user_agent
		FROM candidate_consent WHERE tenant_id=? AND session_id=? ORDER BY agreed_at`, tenantID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Consent
	for rows.Next() {
		var c Consent
		if err := rows.Scan(&c.TenantID, &c.SessionID, &c.CandidateID, &c.Scope, &c.AgreedAt, &c.IP, &c.UserAgent); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (m *MySQLStore) AppendAudit(ctx context.Context, e AuditEntry) error {
	createdAt := e.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO audit_log (tenant_id, actor, action, target, detail, created_at)
		VALUES (?,?,?,?,?,?)`,
		e.TenantID, e.Actor, e.Action, e.Target, e.Detail, createdAt.UTC())
	return err
}

func (m *MySQLStore) ListAudit(ctx context.Context, tenantID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT id, tenant_id, actor, action, target, detail, created_at
		FROM audit_log WHERE tenant_id=? ORDER BY id DESC LIMIT ?`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.TenantID, &e.Actor, &e.Action, &e.Target, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ExportCandidate 导出候选人全部数据(可携带权)。
func (m *MySQLStore) ExportCandidate(ctx context.Context, tenantID, candidateRef string) (CandidateExport, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM interview_session
		 WHERE tenant_id=? AND candidate_ref=? ORDER BY created_at`, tenantID, candidateRef)
	if err != nil {
		return CandidateExport{}, err
	}
	defer rows.Close()

	bundle := CandidateExport{CandidateRef: candidateRef, TenantID: tenantID, ExportedAt: time.Now().UTC()}
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return CandidateExport{}, err
		}
		bundle.Sessions = append(bundle.Sessions, s)
	}
	if err := rows.Err(); err != nil {
		return CandidateExport{}, err
	}
	if len(bundle.Sessions) == 0 {
		return CandidateExport{}, ErrNotFound
	}

	for _, s := range bundle.Sessions {
		turns, err := m.ListTurns(ctx, tenantID, s.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return CandidateExport{}, err
		}
		bundle.Turns = append(bundle.Turns, turns...)

		if r, err := m.GetReport(ctx, tenantID, s.ID); err == nil {
			bundle.Reports = append(bundle.Reports, r)
		}
		if c, err := m.ListConsents(ctx, tenantID, s.ID); err == nil {
			bundle.Consents = append(bundle.Consents, c...)
		}
	}
	return bundle, nil
}

// EraseCandidate 删除候选人全部数据(删除权), 返回删除的会话数。
func (m *MySQLStore) EraseCandidate(ctx context.Context, tenantID, candidateRef string) (int, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT session_id FROM interview_session WHERE tenant_id=? AND candidate_ref=?`, tenantID, candidateRef)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, id := range ids {
		for _, table := range []string{"qa_turn", "interview_report", "candidate_consent", "interview_session"} {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM `+table+` WHERE tenant_id=? AND session_id=?`, tenantID, id); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// Analytics 用聚合 SQL 统计, 而不是把会话拉回进程里遍历。
// 看板会被频繁刷新, N+1 次查询在真实数据量下会把数据库拖慢。
func (m *MySQLStore) Analytics(ctx context.Context, tenantID string) (Analytics, error) {
	out := Analytics{
		ByRecommendation: map[string]int{},
		ByStatus:         map[string]int{},
		ByCompetency:     map[string]map[string]int{},
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT status, recommendation, COUNT(1)
		FROM interview_session WHERE tenant_id=? GROUP BY status, recommendation`, tenantID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var status, rec string
		var n int
		if err := rows.Scan(&status, &rec, &n); err != nil {
			rows.Close()
			return out, err
		}
		out.Sessions += n
		out.ByStatus[status] += n
		if SessionStatus(status) == StatusFinished {
			out.Finished += n
			out.ByRecommendation[rec] += n
		} else if SessionStatus(status) == StatusRunning {
			out.Running += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(AVG(score),0) FROM interview_report WHERE tenant_id=?`, tenantID).
		Scan(&out.AvgScore); err != nil {
		return out, err
	}

	if err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(1),
		       COALESCE(SUM(scored),0),
		       COALESCE(SUM(is_probe),0),
		       COALESCE(SUM(CASE WHEN degraded_from <> '' THEN 1 ELSE 0 END),0)
		FROM qa_turn WHERE tenant_id=?`, tenantID).
		Scan(&out.TotalTurns, &out.ScoredTurns, &out.ProbeTurns, &out.DegradedTurns); err != nil {
		return out, err
	}

	rows2, err := m.db.QueryContext(ctx, `
		SELECT competency, level, COUNT(1) FROM qa_turn
		WHERE tenant_id=? AND scored=1 AND competency<>'' AND level<>''
		GROUP BY competency, level`, tenantID)
	if err != nil {
		return out, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var comp, level string
		var n int
		if err := rows2.Scan(&comp, &level, &n); err != nil {
			return out, err
		}
		if out.ByCompetency[comp] == nil {
			out.ByCompetency[comp] = map[string]int{}
		}
		out.ByCompetency[comp][level] += n
	}
	return out, rows2.Err()
}

// nullableJSON 把空切片转成 nil, 让 MySQL 存 NULL 而不是空字符串。
func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
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
