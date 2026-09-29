package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

// 业务域(面试之外)的 MySQL 实现。
//
// 多值字段(轮次编排、关键词、参考要点)用 JSON 列而不是拆子表:
// 它们的访问模式永远是"跟着主记录一起读写", 没有"按关键词反查题目"这类
// 需求, 拆表只会换来一堆 JOIN 和一致性问题。真正的检索需求由
// internal/knowledge 把它建成内存索引(将来换 ES)来满足。

func jsonBytes(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 || string(b) == "null" {
		return nil
	}
	return string(b)
}

func decodeJSON(raw []byte, out any) {
	if len(raw) == 0 {
		return
	}
	_ = json.Unmarshal(raw, out)
}

func isDuplicateKey(err error) bool {
	var myErr *mysqldriver.MySQLError
	return errors.As(err, &myErr) && myErr.Number == 1062
}

/* ---------------- 职位 ---------------- */

func (m *MySQLStore) CreateJob(ctx context.Context, j Job) error {
	if j.ID == "" || j.TenantID == "" {
		return errors.New("store: 职位 ID 与租户不能为空")
	}
	now := time.Now().UTC()
	if j.CreatedAt.IsZero() {
		j.CreatedAt = now
	}
	j.Rounds = NormalizeRounds(j.Rounds)
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO job (job_id, tenant_id, title, department, level, location, headcount, status,
			competency_model, rounds, owner, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.TenantID, j.Title, j.Department, j.Level, j.Location, j.Headcount, string(j.Status),
		jsonBytes(j.CompetencyModel), jsonBytes(j.Rounds), j.Owner, j.CreatedAt.UTC(), now)
	if isDuplicateKey(err) {
		return ErrConflict
	}
	return err
}

func (m *MySQLStore) UpdateJob(ctx context.Context, j Job) error {
	j.Rounds = NormalizeRounds(j.Rounds)
	res, err := m.db.ExecContext(ctx, `
		UPDATE job SET title=?, department=?, level=?, location=?, headcount=?, status=?,
			competency_model=?, rounds=?, owner=?, updated_at=?
		WHERE job_id=? AND tenant_id=?`,
		j.Title, j.Department, j.Level, j.Location, j.Headcount, string(j.Status),
		jsonBytes(j.CompetencyModel), jsonBytes(j.Rounds), j.Owner, time.Now().UTC(), j.ID, j.TenantID)
	if err != nil {
		return err
	}
	return affectedNotFound(res)
}

func (m *MySQLStore) GetJob(ctx context.Context, tenantID, jobID string) (Job, error) {
	var (
		j      Job
		status string
		model  []byte
		rounds []byte
	)
	err := m.db.QueryRowContext(ctx, `
		SELECT job_id, tenant_id, title, department, level, location, headcount, status,
		       competency_model, rounds, owner, created_at, updated_at
		FROM job WHERE tenant_id=? AND job_id=?`, tenantID, jobID).
		Scan(&j.ID, &j.TenantID, &j.Title, &j.Department, &j.Level, &j.Location, &j.Headcount,
			&status, &model, &rounds, &j.Owner, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	j.Status = JobStatus(status)
	decodeJSON(model, &j.CompetencyModel)
	decodeJSON(rounds, &j.Rounds)
	if len(j.Rounds) == 0 {
		j.Rounds = NormalizeRounds(nil)
	}
	return j, nil
}

func (m *MySQLStore) ListJobs(ctx context.Context, tenantID string) ([]Job, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT job_id, tenant_id, title, department, level, location, headcount, status,
		       competency_model, rounds, owner, created_at, updated_at
		FROM job WHERE tenant_id=? ORDER BY created_at DESC, job_id DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Job{}
	for rows.Next() {
		var (
			j      Job
			status string
			model  []byte
			rounds []byte
		)
		if err := rows.Scan(&j.ID, &j.TenantID, &j.Title, &j.Department, &j.Level, &j.Location,
			&j.Headcount, &status, &model, &rounds, &j.Owner, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		j.Status = JobStatus(status)
		decodeJSON(model, &j.CompetencyModel)
		decodeJSON(rounds, &j.Rounds)
		if len(j.Rounds) == 0 {
			j.Rounds = NormalizeRounds(nil)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

/* ---------------- 候选人 ---------------- */

func (m *MySQLStore) CreateCandidate(ctx context.Context, c Candidate) error {
	if c.Ref == "" || c.TenantID == "" {
		return errors.New("store: 候选人引用值与租户不能为空")
	}
	now := time.Now().UTC()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	// UPSERT: ATS 反复推送同一份简历是常态, 重复推送应当覆盖而不是报错。
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO candidate (candidate_ref, tenant_id, name, email, phone, source, tags,
			resume_source, resume_json, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			name=VALUES(name), email=VALUES(email), phone=VALUES(phone), source=VALUES(source),
			tags=VALUES(tags), resume_source=VALUES(resume_source), resume_json=VALUES(resume_json),
			updated_at=VALUES(updated_at)`,
		c.Ref, c.TenantID, c.Name, c.Email, c.Phone, c.Source, jsonBytes(c.Tags),
		c.ResumeSource, nullableJSON(c.ResumeJSON), c.CreatedAt.UTC(), now)
	return err
}

func (m *MySQLStore) UpdateCandidate(ctx context.Context, c Candidate) error {
	res, err := m.db.ExecContext(ctx, `
		UPDATE candidate SET name=?, email=?, phone=?, source=?, tags=?,
			resume_source=?, resume_json=?, updated_at=?
		WHERE candidate_ref=? AND tenant_id=?`,
		c.Name, c.Email, c.Phone, c.Source, jsonBytes(c.Tags),
		c.ResumeSource, nullableJSON(c.ResumeJSON), time.Now().UTC(), c.Ref, c.TenantID)
	if err != nil {
		return err
	}
	return affectedNotFound(res)
}

func (m *MySQLStore) GetCandidate(ctx context.Context, tenantID, ref string) (Candidate, error) {
	var (
		c      Candidate
		tags   []byte
		resume []byte
	)
	err := m.db.QueryRowContext(ctx, `
		SELECT candidate_ref, tenant_id, name, email, phone, source, tags,
		       resume_source, resume_json, created_at, updated_at
		FROM candidate WHERE tenant_id=? AND candidate_ref=?`, tenantID, ref).
		Scan(&c.Ref, &c.TenantID, &c.Name, &c.Email, &c.Phone, &c.Source, &tags,
			&c.ResumeSource, &resume, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Candidate{}, ErrNotFound
	}
	if err != nil {
		return Candidate{}, err
	}
	decodeJSON(tags, &c.Tags)
	c.ResumeJSON = resume
	return c, nil
}

func (m *MySQLStore) ListCandidates(ctx context.Context, tenantID string, limit int) ([]Candidate, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT candidate_ref, tenant_id, name, email, phone, source, tags, created_at, updated_at
		FROM candidate WHERE tenant_id=? ORDER BY created_at DESC, candidate_ref DESC LIMIT ?`,
		tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Candidate{}
	for rows.Next() {
		var (
			c    Candidate
			tags []byte
		)
		if err := rows.Scan(&c.Ref, &c.TenantID, &c.Name, &c.Email, &c.Phone, &c.Source, &tags,
			&c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		decodeJSON(tags, &c.Tags)
		out = append(out, c)
	}
	return out, rows.Err()
}

/* ---------------- 投递 ---------------- */

func (m *MySQLStore) CreateApplication(ctx context.Context, a Application) error {
	if a.ID == "" || a.TenantID == "" {
		return errors.New("store: 投递 ID 与租户不能为空")
	}
	now := time.Now().UTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO application (application_id, tenant_id, job_id, job_title, candidate_ref,
			candidate_name, stage, status, current_round, rounds, owner, source, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			job_title=VALUES(job_title), candidate_name=VALUES(candidate_name),
			stage=VALUES(stage), status=VALUES(status), current_round=VALUES(current_round),
			rounds=VALUES(rounds), owner=VALUES(owner), updated_at=VALUES(updated_at)`,
		a.ID, a.TenantID, a.JobID, a.JobTitle, a.CandidateRef, a.CandidateName,
		string(a.Stage), a.Status, a.CurrentRound, jsonBytes(a.Rounds), a.Owner, a.Source,
		a.CreatedAt.UTC(), now)
	return err
}

func (m *MySQLStore) UpdateApplication(ctx context.Context, a Application) error {
	res, err := m.db.ExecContext(ctx, `
		UPDATE application SET job_id=?, job_title=?, candidate_ref=?, candidate_name=?,
			stage=?, status=?, current_round=?, rounds=?, owner=?, source=?, updated_at=?
		WHERE application_id=? AND tenant_id=?`,
		a.JobID, a.JobTitle, a.CandidateRef, a.CandidateName, string(a.Stage), a.Status,
		a.CurrentRound, jsonBytes(a.Rounds), a.Owner, a.Source, time.Now().UTC(), a.ID, a.TenantID)
	if err != nil {
		return err
	}
	return affectedNotFound(res)
}

func (m *MySQLStore) GetApplication(ctx context.Context, tenantID, id string) (Application, error) {
	var (
		a      Application
		stage  string
		rounds []byte
	)
	err := m.db.QueryRowContext(ctx, `
		SELECT application_id, tenant_id, job_id, job_title, candidate_ref, candidate_name,
		       stage, status, current_round, rounds, owner, source, created_at, updated_at
		FROM application WHERE tenant_id=? AND application_id=?`, tenantID, id).
		Scan(&a.ID, &a.TenantID, &a.JobID, &a.JobTitle, &a.CandidateRef, &a.CandidateName,
			&stage, &a.Status, &a.CurrentRound, &rounds, &a.Owner, &a.Source,
			&a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Application{}, ErrNotFound
	}
	if err != nil {
		return Application{}, err
	}
	a.Stage = AppStage(stage)
	decodeJSON(rounds, &a.Rounds)
	return a, nil
}

func (m *MySQLStore) ListApplications(ctx context.Context, tenantID, jobID string) ([]Application, error) {
	query := `
		SELECT application_id, tenant_id, job_id, job_title, candidate_ref, candidate_name,
		       stage, status, current_round, rounds, owner, source, created_at, updated_at
		FROM application WHERE tenant_id=?`
	args := []any{tenantID}
	if jobID != "" {
		query += " AND job_id=?"
		args = append(args, jobID)
	}
	query += " ORDER BY updated_at DESC, application_id"

	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Application{}
	for rows.Next() {
		var (
			a      Application
			stage  string
			rounds []byte
		)
		if err := rows.Scan(&a.ID, &a.TenantID, &a.JobID, &a.JobTitle, &a.CandidateRef,
			&a.CandidateName, &stage, &a.Status, &a.CurrentRound, &rounds, &a.Owner, &a.Source,
			&a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.Stage = AppStage(stage)
		decodeJSON(rounds, &a.Rounds)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (m *MySQLStore) ListApplicationsByCandidate(ctx context.Context, tenantID, candidateRef string) ([]Application, error) {
	all, err := m.ListApplications(ctx, tenantID, "")
	if err != nil {
		return nil, err
	}
	out := make([]Application, 0, 4)
	for _, a := range all {
		if a.CandidateRef == candidateRef {
			out = append(out, a)
		}
	}
	return out, nil
}

/* ---------------- 题库 ---------------- */

func (m *MySQLStore) CreateQuestion(ctx context.Context, q QuestionItem) error {
	if q.ID == "" || q.TenantID == "" {
		return errors.New("store: 题目 ID 与租户不能为空")
	}
	now := time.Now().UTC()
	if q.CreatedAt.IsZero() {
		q.CreatedAt = now
	}
	if q.Version <= 0 {
		q.Version = 1
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO question_item (question_id, tenant_id, stage, competency, text, keywords,
			anti_patterns, reference_points, difficulty, importance, max_probe, status, version,
			tags, rounds, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			stage=VALUES(stage), competency=VALUES(competency), text=VALUES(text),
			keywords=VALUES(keywords), anti_patterns=VALUES(anti_patterns),
			reference_points=VALUES(reference_points), difficulty=VALUES(difficulty),
			importance=VALUES(importance), max_probe=VALUES(max_probe), status=VALUES(status),
			version=version+1, tags=VALUES(tags), rounds=VALUES(rounds), updated_at=VALUES(updated_at)`,
		q.ID, q.TenantID, q.Stage, q.Competency, q.Text, jsonBytes(q.Keywords),
		jsonBytes(q.AntiPatterns), jsonBytes(q.ReferencePoints), q.Difficulty, q.Importance,
		q.MaxProbe, q.Status, q.Version, jsonBytes(q.Tags), jsonBytes(q.Rounds),
		q.CreatedAt.UTC(), now)
	return err
}

func (m *MySQLStore) UpdateQuestion(ctx context.Context, q QuestionItem) error {
	res, err := m.db.ExecContext(ctx, `
		UPDATE question_item SET stage=?, competency=?, text=?, keywords=?, anti_patterns=?,
			reference_points=?, difficulty=?, importance=?, max_probe=?, status=?,
			version=version+1, tags=?, rounds=?, updated_at=?
		WHERE question_id=? AND tenant_id=?`,
		q.Stage, q.Competency, q.Text, jsonBytes(q.Keywords), jsonBytes(q.AntiPatterns),
		jsonBytes(q.ReferencePoints), q.Difficulty, q.Importance, q.MaxProbe, q.Status,
		jsonBytes(q.Tags), jsonBytes(q.Rounds), time.Now().UTC(), q.ID, q.TenantID)
	if err != nil {
		return err
	}
	return affectedNotFound(res)
}

func (m *MySQLStore) DeleteQuestion(ctx context.Context, tenantID, id string) error {
	res, err := m.db.ExecContext(ctx,
		`DELETE FROM question_item WHERE question_id=? AND tenant_id=?`, id, tenantID)
	if err != nil {
		return err
	}
	return affectedNotFound(res)
}

func (m *MySQLStore) GetQuestion(ctx context.Context, tenantID, id string) (QuestionItem, error) {
	var (
		q      QuestionItem
		kw     []byte
		anti   []byte
		points []byte
		tags   []byte
		rounds []byte
	)
	err := m.db.QueryRowContext(ctx, `
		SELECT question_id, tenant_id, stage, competency, text, keywords, anti_patterns,
		       reference_points, difficulty, importance, max_probe, status, version, tags,
		       rounds, created_at, updated_at
		FROM question_item WHERE tenant_id=? AND question_id=?`, tenantID, id).
		Scan(&q.ID, &q.TenantID, &q.Stage, &q.Competency, &q.Text, &kw, &anti, &points,
			&q.Difficulty, &q.Importance, &q.MaxProbe, &q.Status, &q.Version, &tags,
			&rounds, &q.CreatedAt, &q.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return QuestionItem{}, ErrNotFound
	}
	if err != nil {
		return QuestionItem{}, err
	}
	decodeJSON(kw, &q.Keywords)
	decodeJSON(anti, &q.AntiPatterns)
	decodeJSON(points, &q.ReferencePoints)
	decodeJSON(tags, &q.Tags)
	decodeJSON(rounds, &q.Rounds)
	return q, nil
}

func (m *MySQLStore) ListQuestions(ctx context.Context, tenantID string) ([]QuestionItem, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT question_id, tenant_id, stage, competency, text, keywords, anti_patterns,
		       reference_points, difficulty, importance, max_probe, status, version, tags,
		       rounds, created_at, updated_at
		FROM question_item WHERE tenant_id=? ORDER BY stage, created_at, question_id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []QuestionItem{}
	for rows.Next() {
		var (
			q      QuestionItem
			kw     []byte
			anti   []byte
			points []byte
			tags   []byte
			rounds []byte
		)
		if err := rows.Scan(&q.ID, &q.TenantID, &q.Stage, &q.Competency, &q.Text, &kw, &anti,
			&points, &q.Difficulty, &q.Importance, &q.MaxProbe, &q.Status, &q.Version, &tags,
			&rounds, &q.CreatedAt, &q.UpdatedAt); err != nil {
			return nil, err
		}
		decodeJSON(kw, &q.Keywords)
		decodeJSON(anti, &q.AntiPatterns)
		decodeJSON(points, &q.ReferencePoints)
		decodeJSON(tags, &q.Tags)
		decodeJSON(rounds, &q.Rounds)
		out = append(out, q)
	}
	return out, rows.Err()
}

/* ---------------- 面试安排 ---------------- */

func (m *MySQLStore) CreateSchedule(ctx context.Context, s Schedule) error {
	if s.ID == "" || s.TenantID == "" {
		return errors.New("store: 安排 ID 与租户不能为空")
	}
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO interview_schedule (schedule_id, tenant_id, application_id, job_id, candidate_ref,
			round, mode, scheduled_at, duration_min, interviewer, status, session_id, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			round=VALUES(round), mode=VALUES(mode), scheduled_at=VALUES(scheduled_at),
			duration_min=VALUES(duration_min), interviewer=VALUES(interviewer),
			status=VALUES(status), session_id=VALUES(session_id), updated_at=VALUES(updated_at)`,
		s.ID, s.TenantID, s.ApplicationID, s.JobID, s.CandidateRef, s.Round, s.Mode,
		s.ScheduledAt.UTC(), s.DurationMin, s.Interviewer, s.Status, s.SessionID,
		s.CreatedAt.UTC(), now)
	return err
}

func (m *MySQLStore) UpdateSchedule(ctx context.Context, s Schedule) error {
	res, err := m.db.ExecContext(ctx, `
		UPDATE interview_schedule SET round=?, mode=?, scheduled_at=?, duration_min=?,
			interviewer=?, status=?, session_id=?, updated_at=?
		WHERE schedule_id=? AND tenant_id=?`,
		s.Round, s.Mode, s.ScheduledAt.UTC(), s.DurationMin, s.Interviewer, s.Status,
		s.SessionID, time.Now().UTC(), s.ID, s.TenantID)
	if err != nil {
		return err
	}
	return affectedNotFound(res)
}

func (m *MySQLStore) GetSchedule(ctx context.Context, tenantID, id string) (Schedule, error) {
	var s Schedule
	err := m.db.QueryRowContext(ctx, `
		SELECT schedule_id, tenant_id, application_id, job_id, candidate_ref, round, mode,
		       scheduled_at, duration_min, interviewer, status, session_id, created_at, updated_at
		FROM interview_schedule WHERE tenant_id=? AND schedule_id=?`, tenantID, id).
		Scan(&s.ID, &s.TenantID, &s.ApplicationID, &s.JobID, &s.CandidateRef, &s.Round, &s.Mode,
			&s.ScheduledAt, &s.DurationMin, &s.Interviewer, &s.Status, &s.SessionID,
			&s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, ErrNotFound
	}
	return s, err
}

func (m *MySQLStore) ListSchedules(ctx context.Context, tenantID string, from, to time.Time) ([]Schedule, error) {
	query := `
		SELECT schedule_id, tenant_id, application_id, job_id, candidate_ref, round, mode,
		       scheduled_at, duration_min, interviewer, status, session_id, created_at, updated_at
		FROM interview_schedule WHERE tenant_id=?`
	args := []any{tenantID}
	if !from.IsZero() {
		query += " AND scheduled_at >= ?"
		args = append(args, from.UTC())
	}
	if !to.IsZero() {
		query += " AND scheduled_at <= ?"
		args = append(args, to.UTC())
	}
	query += " ORDER BY scheduled_at, schedule_id"

	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Schedule{}
	for rows.Next() {
		var s Schedule
		if err := rows.Scan(&s.ID, &s.TenantID, &s.ApplicationID, &s.JobID, &s.CandidateRef,
			&s.Round, &s.Mode, &s.ScheduledAt, &s.DurationMin, &s.Interviewer, &s.Status,
			&s.SessionID, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

/* ---------------- 录制件 ---------------- */

func (m *MySQLStore) CreateRecording(ctx context.Context, r Recording) error {
	if r.ID == "" || r.TenantID == "" {
		return errors.New("store: 录制件 ID 与租户不能为空")
	}
	now := time.Now().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	if r.Status == "" {
		r.Status = "uploading"
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO interview_recording (recording_id, tenant_id, session_id, candidate_ref, kind,
			mime_type, storage_key, chunks, size_bytes, duration_ms, status, delete_after,
			created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			mime_type=VALUES(mime_type), storage_key=VALUES(storage_key), chunks=VALUES(chunks),
			size_bytes=VALUES(size_bytes), duration_ms=VALUES(duration_ms), status=VALUES(status),
			delete_after=VALUES(delete_after), updated_at=VALUES(updated_at)`,
		r.ID, r.TenantID, r.SessionID, r.CandidateRef, r.Kind, r.MimeType, r.StorageKey,
		r.Chunks, r.SizeBytes, r.DurationMS, r.Status, nullableTime(r.DeleteAfter),
		r.CreatedAt.UTC(), now)
	return err
}

func (m *MySQLStore) UpdateRecording(ctx context.Context, r Recording) error {
	res, err := m.db.ExecContext(ctx, `
		UPDATE interview_recording SET mime_type=?, chunks=?, size_bytes=?, duration_ms=?,
			status=?, delete_after=?, updated_at=?
		WHERE recording_id=? AND tenant_id=?`,
		r.MimeType, r.Chunks, r.SizeBytes, r.DurationMS, r.Status, nullableTime(r.DeleteAfter),
		time.Now().UTC(), r.ID, r.TenantID)
	if err != nil {
		return err
	}
	return affectedNotFound(res)
}

func (m *MySQLStore) GetRecording(ctx context.Context, tenantID, id string) (Recording, error) {
	return m.scanRecording(m.db.QueryRowContext(ctx, `
		SELECT recording_id, tenant_id, session_id, candidate_ref, kind, mime_type, storage_key,
		       chunks, size_bytes, duration_ms, status, delete_after, created_at, updated_at
		FROM interview_recording WHERE tenant_id=? AND recording_id=?`, tenantID, id))
}

func (m *MySQLStore) GetRecordingBySession(ctx context.Context, tenantID, sessionID, kind string) (Recording, error) {
	return m.scanRecording(m.db.QueryRowContext(ctx, `
		SELECT recording_id, tenant_id, session_id, candidate_ref, kind, mime_type, storage_key,
		       chunks, size_bytes, duration_ms, status, delete_after, created_at, updated_at
		FROM interview_recording WHERE tenant_id=? AND session_id=? AND kind=?
		ORDER BY created_at DESC LIMIT 1`, tenantID, sessionID, kind))
}

func (m *MySQLStore) ListRecordings(ctx context.Context, tenantID, sessionID string) ([]Recording, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT recording_id, tenant_id, session_id, candidate_ref, kind, mime_type, storage_key,
		       chunks, size_bytes, duration_ms, status, delete_after, created_at, updated_at
		FROM interview_recording WHERE tenant_id=? AND session_id=? ORDER BY recording_id`, tenantID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Recording{}
	for rows.Next() {
		r, err := scanRecordingRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (m *MySQLStore) DeleteRecording(ctx context.Context, tenantID, id string) error {
	res, err := m.db.ExecContext(ctx,
		`DELETE FROM interview_recording WHERE recording_id=? AND tenant_id=?`, id, tenantID)
	if err != nil {
		return err
	}
	return affectedNotFound(res)
}

func (m *MySQLStore) PurgeExpiredRecordings(ctx context.Context, now time.Time) ([]Recording, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT recording_id, tenant_id, session_id, candidate_ref, kind, mime_type, storage_key,
		       chunks, size_bytes, duration_ms, status, delete_after, created_at, updated_at
		FROM interview_recording WHERE delete_after IS NOT NULL AND delete_after < ?`, now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Recording{}
	for rows.Next() {
		r, err := scanRecordingRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(...any) error }

func scanRecordingRow(row rowScanner) (Recording, error) {
	var (
		r      Recording
		delete sql.NullTime
	)
	if err := row.Scan(&r.ID, &r.TenantID, &r.SessionID, &r.CandidateRef, &r.Kind, &r.MimeType,
		&r.StorageKey, &r.Chunks, &r.SizeBytes, &r.DurationMS, &r.Status, &delete,
		&r.CreatedAt, &r.UpdatedAt); err != nil {
		return Recording{}, err
	}
	if delete.Valid {
		r.DeleteAfter = delete.Time
	}
	return r, nil
}

func (m *MySQLStore) scanRecording(row rowScanner) (Recording, error) {
	r, err := scanRecordingRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Recording{}, ErrNotFound
	}
	return r, err
}

// affectedNotFound 把"影响行数为 0"翻译成 ErrNotFound。
//
// 不这么做的话, 更新一个不存在或不属于本租户的记录会静默成功 ——
// 调用方拿到 200, 数据却没变, 这类问题在管理界面里表现为"点了保存没反应"。
func affectedNotFound(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}
