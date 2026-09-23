-- AI 面试中台 · MySQL 表结构
--
-- 这是表结构的唯一来源: 集成测试通过 go:embed 直接执行本文件,
-- docker compose 也挂载本文件初始化数据库。不要另存一份副本,
-- 两份 DDL 一旦不一致, 本地能跑线上不能跑的问题就会反复出现。
--
-- 分片约定: 生产环境按 tenant_id 分库, interview_session 与其下游表
-- 都以 session_id 为分片键, 保证一场面试的全部数据落在同一个分片,
-- 避免跨分片事务。

CREATE TABLE IF NOT EXISTS interview_session (
  session_id     VARCHAR(64)  NOT NULL COMMENT '会话 ID',
  tenant_id      VARCHAR(64)  NOT NULL DEFAULT 'default' COMMENT '租户, 分库键',
  position       VARCHAR(128) NOT NULL DEFAULT '' COMMENT '应聘岗位, 用于报告标题',
  company        VARCHAR(128) NOT NULL DEFAULT '' COMMENT '公司名',
  candidate_name VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '候选人姓名(展示用, 与 candidate_id 分离)',
  interviewer_name VARCHAR(64) NOT NULL DEFAULT '' COMMENT 'AI 面试官名, 便于候选人感知"在跟谁说话"',
  resume_json     JSON         NULL COMMENT '结构化简历实体(带原文偏移), 由解析器产出',
  round          INT          NOT NULL DEFAULT 1 COMMENT '面试轮次 1..5',
  minutes        INT          NOT NULL DEFAULT 45 COMMENT '时长预算(分钟)',
  stage          VARCHAR(32)  NOT NULL DEFAULT 'INIT' COMMENT '当前阶段',
  status         VARCHAR(16)  NOT NULL DEFAULT 'running' COMMENT 'running/finished/aborted',
  recommendation VARCHAR(32)  NOT NULL DEFAULT '' COMMENT 'AI 建议结论',
  created_at     DATETIME(3)  NOT NULL,
  updated_at     DATETIME(3)  NOT NULL,
  PRIMARY KEY (session_id),
  KEY idx_tenant_created (tenant_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='面试会话';

CREATE TABLE IF NOT EXISTS qa_turn (
  session_id    VARCHAR(64)  NOT NULL COMMENT '会话 ID',
  turn_index    INT          NOT NULL COMMENT '轮次序号, 从 1 开始',
  stage         VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '所属阶段',
  question_id   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '题目 ID',
  competency    VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '考察能力项',
  question      TEXT         NOT NULL COMMENT '面试官提问',
  answer        MEDIUMTEXT   NOT NULL COMMENT '候选人回答(评分只用 final 文本)',
  duration_ms   BIGINT       NOT NULL DEFAULT 0 COMMENT '回答耗时',
  is_probe      TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '是否为追问',
  scored        TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '是否参与评分',
  level         VARCHAR(16)  NOT NULL DEFAULT '' COMMENT '等级展示名',
  level_num     INT          NOT NULL DEFAULT 0 COMMENT '1..5, 便于统计',
  confidence    DOUBLE       NOT NULL DEFAULT 0,
  degraded_from VARCHAR(255) NOT NULL DEFAULT '' COMMENT '非空表示来自备用评分器',
  verdict       JSON         NULL COMMENT '完整评分结论(含证据), 保证报告可复现',
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (session_id, turn_index),
  KEY idx_competency (session_id, competency)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='问答轮次, 高频写入表';

CREATE TABLE IF NOT EXISTS interview_report (
  session_id     VARCHAR(64) NOT NULL,
  recommendation VARCHAR(32) NOT NULL DEFAULT '',
  confidence     DOUBLE      NOT NULL DEFAULT 0,
  payload        JSON        NOT NULL COMMENT '完整报告, 保证结论可复现',
  created_at     DATETIME(3) NOT NULL,
  PRIMARY KEY (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='评估报告';

CREATE TABLE IF NOT EXISTS candidate_consent (
  session_id   VARCHAR(64)  NOT NULL,
  candidate_id VARCHAR(64)  NOT NULL,
  scope        VARCHAR(32)  NOT NULL COMMENT 'recording/scoring/retention',
  agreed_at    DATETIME(3)  NOT NULL COMMENT '授权时间, 保留最早一次',
  ip           VARCHAR(45)  NOT NULL DEFAULT '',
  user_agent   VARCHAR(255) NOT NULL DEFAULT '',
  PRIMARY KEY (session_id, scope)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='候选人数据授权留痕';

CREATE TABLE IF NOT EXISTS audit_log (
  id         BIGINT       NOT NULL AUTO_INCREMENT,
  tenant_id  VARCHAR(64)  NOT NULL DEFAULT '',
  actor      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '操作人',
  action     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'view_resume/override_score/...',
  target     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '被操作对象 ID',
  detail     VARCHAR(512) NOT NULL DEFAULT '',
  created_at DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_actor_created (tenant_id, actor, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='审计日志: 谁在何时看了谁的简历';
