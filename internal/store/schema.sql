-- AI 面试中台 · MySQL 表结构
--
-- 这是表结构的唯一来源: 集成测试通过 go:embed 直接执行本文件,
-- docker compose 也挂载本文件初始化数据库。不要另存一份副本,
-- 两份 DDL 一旦不一致, 本地能跑线上不能跑的问题就会反复出现。
--
-- 分片约定: 生产环境按 tenant_id 分库, 所有表都以 tenant_id 为分片键前缀,
-- 保证一个租户的全部数据落在同一个分片, 避免跨分片事务。
--
-- 注意: 本文件是"当前状态"的建表脚本。生产环境变更 schema 应当使用迁移工具
-- (goose / atlas / flyway) 管理版本, 而不是依赖 CREATE TABLE IF NOT EXISTS ——
-- 后者对已存在的表不会做任何修改。

CREATE TABLE IF NOT EXISTS interview_session (
  session_id     VARCHAR(64)  NOT NULL COMMENT '会话 ID',
  tenant_id      VARCHAR(64)  NOT NULL COMMENT '租户, 分库键',
  position       VARCHAR(128) NOT NULL DEFAULT '' COMMENT '应聘岗位',
  company        VARCHAR(128) NOT NULL DEFAULT '' COMMENT '公司名',
  candidate_name VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '候选人展示名(界面用)',
  candidate_ref  VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '候选人假名引用值(HMAC), 原文不落库',
  interviewer_name VARCHAR(64) NOT NULL DEFAULT '' COMMENT 'AI 面试官名',
  application_id VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '所属投递(招聘管道)', 
  resume_json    JSON         NULL COMMENT '结构化简历实体(带原文偏移)',
  round          INT          NOT NULL DEFAULT 1 COMMENT '面试轮次 1..5',
  minutes        INT          NOT NULL DEFAULT 45 COMMENT '时长预算(分钟)',
  stage          VARCHAR(32)  NOT NULL DEFAULT 'INIT' COMMENT '当前阶段',
  status         VARCHAR(16)  NOT NULL DEFAULT 'running' COMMENT 'running/finished/aborted',
  recommendation VARCHAR(32)  NOT NULL DEFAULT '' COMMENT 'AI 建议结论',
  created_at     DATETIME(3)  NOT NULL,
  updated_at     DATETIME(3)  NOT NULL,
  PRIMARY KEY (session_id),
  KEY idx_tenant_created (tenant_id, created_at),
  KEY idx_tenant_candidate (tenant_id, candidate_ref),
  KEY idx_application (tenant_id, application_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='面试会话';

CREATE TABLE IF NOT EXISTS qa_turn (
  tenant_id     VARCHAR(64)  NOT NULL COMMENT '租户, 分片键',
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
  probe_focus     VARCHAR(96)  NOT NULL DEFAULT '' COMMENT '追问靶子(要点名), 非追问时为空',
  probe_reference VARCHAR(512) NOT NULL DEFAULT '' COMMENT '追问引用的参考答案原话',
  retrieval       JSON         NULL COMMENT '当轮检索命中快照(含分数与出处), 让追问可复核',
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (session_id, turn_index),
  KEY idx_tenant_session (tenant_id, session_id),
  KEY idx_competency (tenant_id, competency)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='问答轮次, 高频写入表';

CREATE TABLE IF NOT EXISTS interview_report (
  tenant_id      VARCHAR(64) NOT NULL,
  session_id     VARCHAR(64) NOT NULL,
  recommendation VARCHAR(32) NOT NULL DEFAULT '',
  confidence     DOUBLE      NOT NULL DEFAULT 0,
  score          INT         NOT NULL DEFAULT 0 COMMENT '0..100 综合分',
  payload        JSON        NOT NULL COMMENT '完整报告, 保证结论可复现',
  created_at     DATETIME(3) NOT NULL,
  PRIMARY KEY (session_id),
  KEY idx_tenant (tenant_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='评估报告';

CREATE TABLE IF NOT EXISTS candidate_consent (
  tenant_id    VARCHAR(64)  NOT NULL,
  session_id   VARCHAR(64)  NOT NULL,
  candidate_id VARCHAR(64)  NOT NULL COMMENT '候选人假名引用值',
  scope        VARCHAR(32)  NOT NULL COMMENT 'recording/scoring/retention',
  agreed_at    DATETIME(3)  NOT NULL COMMENT '授权时间, 保留最早一次',
  ip           VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '来源 IP, 已脱敏到网段',
  user_agent   VARCHAR(255) NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, session_id, scope)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='候选人数据授权留痕';

CREATE TABLE IF NOT EXISTS tenant_api_key (
  key_id     VARCHAR(64)  NOT NULL,
  tenant_id  VARCHAR(64)  NOT NULL,
  name       VARCHAR(128) NOT NULL DEFAULT '' COMMENT '用途备注, 如 "生产 ATS 集成"',
  role       VARCHAR(32)  NOT NULL COMMENT 'admin/interviewer/scheduler',
  key_hash   CHAR(64)     NOT NULL COMMENT 'sha256(明文), 明文不落库',
  created_at DATETIME(3)  NOT NULL,
  revoked_at DATETIME(3)  NULL,
  PRIMARY KEY (key_id),
  UNIQUE KEY uk_key_hash (key_hash),
  KEY idx_tenant (tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='租户 API Key(仅存哈希)';

CREATE TABLE IF NOT EXISTS audit_log (
  id         BIGINT       NOT NULL AUTO_INCREMENT,
  tenant_id  VARCHAR(64)  NOT NULL DEFAULT '',
  actor      VARCHAR(128) NOT NULL DEFAULT '' COMMENT '操作主体',
  action     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'report.view / score.override / ...',
  target     VARCHAR(128) NOT NULL DEFAULT '' COMMENT '被操作对象 ID',
  detail     VARCHAR(512) NOT NULL DEFAULT '',
  created_at DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_tenant_created (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='审计日志: 只追加, 不删除';

-- ---------------------------------------------------------------------------
-- 面试之外的业务域: 职位 / 候选人 / 投递管道 / 题库 / 面试安排 / 录制件
--
-- 它们与会话表在同一个库、同一套租户隔离规则下, 因为招聘流程里的判断
-- 依赖它们的联合视图(一个人在一个职位上的全部轮次结果)。
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS job (
  job_id           VARCHAR(64)  NOT NULL,
  tenant_id        VARCHAR(64)  NOT NULL COMMENT '租户, 分片键',
  title            VARCHAR(128) NOT NULL DEFAULT '',
  department       VARCHAR(128) NOT NULL DEFAULT '',
  level            VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '职级, 如 15A / P7',
  location         VARCHAR(64)  NOT NULL DEFAULT '',
  headcount        INT          NOT NULL DEFAULT 1,
  status           VARCHAR(16)  NOT NULL DEFAULT 'draft' COMMENT 'draft/open/paused/closed',
  competency_model JSON         NULL COMMENT '能力项权重 {"language_core":30,...}',
  rounds           JSON         NULL COMMENT '1..5 轮编排(是否 AI 主导/是否人类在场/模式)',
  owner            VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '招聘负责人',
  created_at       DATETIME(3)  NOT NULL,
  updated_at       DATETIME(3)  NOT NULL,
  PRIMARY KEY (job_id),
  KEY idx_tenant_status (tenant_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='招聘职位';

CREATE TABLE IF NOT EXISTS candidate (
  candidate_ref VARCHAR(64)  NOT NULL COMMENT '假名引用值(HMAC), 全系统关联用它',
  tenant_id     VARCHAR(64)  NOT NULL,
  name          VARCHAR(64)  NOT NULL DEFAULT '',
  email         VARCHAR(128) NOT NULL DEFAULT '',
  phone         VARCHAR(64)  NOT NULL DEFAULT '',
  source        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '来源渠道',
  tags          JSON         NULL,
  resume_source MEDIUMTEXT   NULL COMMENT '简历原文(受权限保护, 随候选人删除)',
  resume_json   JSON         NULL COMMENT '结构化实体 + 原文偏移',
  created_at    DATETIME(3)  NOT NULL,
  updated_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (candidate_ref),
  KEY idx_tenant_created (tenant_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='候选人档案';

CREATE TABLE IF NOT EXISTS application (
  application_id VARCHAR(64)  NOT NULL,
  tenant_id      VARCHAR(64)  NOT NULL,
  job_id         VARCHAR(64)  NOT NULL,
  job_title      VARCHAR(128) NOT NULL DEFAULT '' COMMENT '冗余展示字段, 避免看板 N+1 查询',
  candidate_ref  VARCHAR(64)  NOT NULL,
  candidate_name VARCHAR(64)  NOT NULL DEFAULT '',
  stage          VARCHAR(24)  NOT NULL DEFAULT 'screening',
  status         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active/hired/rejected/withdrawn',
  current_round  INT          NOT NULL DEFAULT 0,
  rounds         JSON         NULL COMMENT '每轮状态与结果',
  owner          VARCHAR(64)  NOT NULL DEFAULT '',
  source         VARCHAR(64)  NOT NULL DEFAULT '',
  created_at     DATETIME(3)  NOT NULL,
  updated_at     DATETIME(3)  NOT NULL,
  PRIMARY KEY (application_id),
  KEY idx_tenant_job (tenant_id, job_id, stage),
  KEY idx_tenant_candidate (tenant_id, candidate_ref)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='投递(职位 × 候选人), 招聘看板卡片';

CREATE TABLE IF NOT EXISTS question_item (
  question_id      VARCHAR(64)  NOT NULL,
  tenant_id        VARCHAR(64)  NOT NULL,
  stage            VARCHAR(32)  NOT NULL DEFAULT '' COMMENT 'GREETING / TECH_FUNDAMENTAL / ...',
  competency       VARCHAR(64)  NOT NULL DEFAULT '',
  text             TEXT         NOT NULL,
  keywords         JSON         NULL COMMENT '判定要点(评分用)',
  anti_patterns    JSON         NULL COMMENT '典型错误回答',
  reference_points JSON         NULL COMMENT '参考答案要点(RAG 语料)',
  difficulty       VARCHAR(16)  NOT NULL DEFAULT 'mid',
  importance       VARCHAR(16)  NOT NULL DEFAULT 'medium',
  max_probe        INT          NOT NULL DEFAULT 2,
  status           VARCHAR(16)  NOT NULL DEFAULT 'draft' COMMENT 'draft/reviewing/published/retired',
  version          INT          NOT NULL DEFAULT 1 COMMENT '每次修改自增, 让"这题改过几次"可追溯',
  tags             JSON         NULL,
  rounds           JSON         NULL COMMENT '适用轮次',
  created_at       DATETIME(3)  NOT NULL,
  updated_at       DATETIME(3)  NOT NULL,
  PRIMARY KEY (question_id),
  KEY idx_tenant_stage (tenant_id, stage, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='题库条目(提问脚本 + 评分要点 + 检索语料)';

CREATE TABLE IF NOT EXISTS interview_schedule (
  schedule_id    VARCHAR(64)  NOT NULL,
  tenant_id      VARCHAR(64)  NOT NULL,
  application_id VARCHAR(64)  NOT NULL,
  job_id         VARCHAR(64)  NOT NULL DEFAULT '',
  candidate_ref  VARCHAR(64)  NOT NULL DEFAULT '',
  round          INT          NOT NULL DEFAULT 1,
  mode           VARCHAR(16)  NOT NULL DEFAULT 'video' COMMENT 'video/audio/text/coding',
  scheduled_at   DATETIME(3)  NOT NULL,
  duration_min   INT          NOT NULL DEFAULT 45,
  interviewer    VARCHAR(64)  NOT NULL DEFAULT '',
  status         VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending/confirmed/running/done/cancelled/no_show',
  session_id     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '实际开面后回填',
  created_at     DATETIME(3)  NOT NULL,
  updated_at     DATETIME(3)  NOT NULL,
  PRIMARY KEY (schedule_id),
  KEY idx_tenant_time (tenant_id, scheduled_at),
  KEY idx_application (tenant_id, application_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='面试安排';

CREATE TABLE IF NOT EXISTS interview_recording (
  recording_id  VARCHAR(64)  NOT NULL,
  tenant_id     VARCHAR(64)  NOT NULL,
  session_id    VARCHAR(64)  NOT NULL,
  candidate_ref VARCHAR(64)  NOT NULL DEFAULT '',
  kind          VARCHAR(16)  NOT NULL DEFAULT 'video' COMMENT 'video/audio/screen',
  mime_type     VARCHAR(96)  NOT NULL DEFAULT '',
  storage_key   VARCHAR(255) NOT NULL COMMENT '对象存储/文件系统键, 二进制不入库',
  chunks        INT          NOT NULL DEFAULT 0,
  size_bytes    BIGINT       NOT NULL DEFAULT 0,
  duration_ms   BIGINT       NOT NULL DEFAULT 0,
  status        VARCHAR(16)  NOT NULL DEFAULT 'uploading' COMMENT 'uploading/complete/failed',
  delete_after  DATETIME(3)  NULL COMMENT '保留期到期时间, 到期后清理',
  created_at    DATETIME(3)  NOT NULL,
  updated_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (recording_id),
  KEY idx_tenant_session (tenant_id, session_id),
  KEY idx_retention (delete_after)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='面试录制件元数据';

-- ---------------------------------------------------------------------------
-- 账号体系: 注册、登录、人脸、登录会话
--
-- 与 tenant_api_key 的区别: API Key 是"机器身份"(客户的 ATS 集成),
-- 这张表是"人的身份"。两者不能合并 —— 人需要登录、退出、改密码、
-- 单独停权与追责, 而这些语义放在 API Key 上会很别扭(而且无法回答
-- "这次操作是哪个同事做的")。
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS account_user (
  user_id        VARCHAR(64)  NOT NULL,
  tenant_id      VARCHAR(64)  NOT NULL COMMENT '租户, 分片键',
  name           VARCHAR(64)  NOT NULL DEFAULT '',
  -- email/phone 用 NULL 而不是空串: (tenant_id, email) 是唯一键,
  -- 而 MySQL 唯一索引把空串当成一个具体值 —— 用空串会让第二个
  -- "只填手机号"的用户插入失败, 报的还是"邮箱已注册"。
  email          VARCHAR(191) NULL COMMENT '登录标识之一, 已规范化(小写去空格)',
  phone          VARCHAR(32)  NULL COMMENT '登录标识之二, 已规范化(+86...)',
  role           VARCHAR(32)  NOT NULL COMMENT 'admin/interviewer/scheduler/candidate',
  password_hash  VARCHAR(255) NOT NULL COMMENT 'pbkdf2-sha256$迭代次数$盐$派生密钥, 明文不落库',
  face_enrolled  TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '是否已录入人脸(模板在 account_face)',
  status         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active/disabled',
  created_at     DATETIME(3)  NOT NULL,
  updated_at     DATETIME(3)  NOT NULL,
  last_login_at  DATETIME(3)  NULL,
  PRIMARY KEY (user_id),
  UNIQUE KEY uk_tenant_email (tenant_id, email),
  UNIQUE KEY uk_tenant_phone (tenant_id, phone),
  KEY idx_tenant_created (tenant_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='账号(人)';

CREATE TABLE IF NOT EXISTS account_face (
  tenant_id  VARCHAR(64)  NOT NULL,
  user_id    VARCHAR(64)  NOT NULL,
  template   MEDIUMBLOB   NOT NULL COMMENT '特征向量(float32 小端连续), 不存原始照片',
  dim        INT          NOT NULL DEFAULT 0 COMMENT '单个样本的维度',
  samples    INT          NOT NULL DEFAULT 1 COMMENT '样本数: 多帧注册时保存多份, 提高跨会话通过率',
  matcher    VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '生成模板的匹配器',
  assurance  VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '可信级别, 如实标注',
  quality    DOUBLE       NOT NULL DEFAULT 0,
  frames     INT          NOT NULL DEFAULT 0 COMMENT '录入时使用的帧数',
  enrolled_at DATETIME(3) NOT NULL,
  updated_at  DATETIME(3) NOT NULL,
  PRIMARY KEY (tenant_id, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='人脸模板(生物特征)';

CREATE TABLE IF NOT EXISTS account_login (
  token_hash CHAR(43)     NOT NULL COMMENT 'sha256(令牌) 的 base64url, 明文只在签发时返回',
  tenant_id  VARCHAR(64)  NOT NULL,
  user_id    VARCHAR(64)  NOT NULL,
  role       VARCHAR(32)  NOT NULL,
  ip         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '来源网段, 已脱敏',
  user_agent VARCHAR(255) NOT NULL DEFAULT '',
  created_at DATETIME(3)  NOT NULL,
  expires_at DATETIME(3)  NOT NULL,
  PRIMARY KEY (token_hash),
  KEY idx_user (tenant_id, user_id),
  KEY idx_expires (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='网页登录会话(服务端可撤销)';
