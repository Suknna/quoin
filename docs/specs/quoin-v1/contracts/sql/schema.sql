-- ============================================================================
-- Quoin v1 — 当前完整 SQLite Schema 的唯一机器权威
-- 路径: contracts/sql/schema.sql
-- 票:   https://github.com/Suknna/quoin/issues/9（Q9.1 A / Q9.2 A）
-- 语义: docs/specs/quoin-v1/persistence.md（CATEGORY=DATA，DATA-* 条款解释本文件
--       所承载的约束；本文件不复制 Markdown 语义，Markdown 不复制完整字段清单）
--
-- 约定（规范性，详见 persistence.md）:
--   * 生成式技术 locator: INTEGER PRIMARY KEY CHECK (id > 0)；领域 locator 与
--     单调序列使用 AUTOINCREMENT（跨删除不复用）；单行/投影/连接表可不用。
--     HTTP 十进制字符串表示由 http-api.md (#10) 定义。
--   * 用户稳定 key（business_system.key、alert_sources.source_key、
--     connections.name、discovery/plan/check key 等）与复合领域身份
--     （alert_occurrences、observed_resources 的 UNIQUE 约束）是相等性权威；
--     locator 只承担 FK / URL / 审计引用。
--   * Alertmanager fingerprint 是上游 64-bit 无符号值的大端 8 字节 BLOB，
--     不是有符号整数、不是 SHA-256。
--   * 全部时间列 TEXT，RFC3339Nano UTC（来源时间无损规范化见 DATA-ALERT-*）。
--   * 持久历史禁止级联删除：所有外键 ON UPDATE RESTRICT ON DELETE RESTRICT；
--     不可变/追加表用触发器拒绝改写（可清理的派生表除外）。
--   * 所有普通表 STRICT；JSON 列带 json_valid CHECK（NULL 视为合法）。
--   * 本文件只描述"当前 schema"；历史迁移是独立不可变过渡程序
--     （DATA-MIGRATION-*），本文件不包含迁移种子。
-- ============================================================================

PRAGMA foreign_keys = ON;

-- ============================================================================
-- 1. 账号、会话与服务身份
-- ============================================================================

CREATE TABLE users (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  username                   TEXT NOT NULL UNIQUE,          -- 稳定登录名，只禁用不删除
  display_name               TEXT NOT NULL,
  role                       TEXT NOT NULL CHECK (role IN ('admin','operator')),
  enabled                    INTEGER NOT NULL CHECK (enabled IN (0,1)),
  auth_revision              INTEGER NOT NULL DEFAULT 1 CHECK (auth_revision > 0),
  password_phc               TEXT NOT NULL CHECK (length(password_phc) > 0), -- Argon2id PHC，格式见 security.md
  password_change_required   INTEGER NOT NULL DEFAULT 0 CHECK (password_change_required IN (0,1)), -- 首次/强制改密（离线创建、Admin 重置、备份恢复置位）
  password_change_required_at TEXT,                        -- 置位时间；成功改密在同一事务清除标志（DATA-AUTH-001）
  row_version                INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- 用户行并发前提；与 auth_revision 独立（DATA-AUTH-004）
  created_at                 TEXT NOT NULL,
  updated_at                 TEXT NOT NULL,
  CHECK (
    (password_change_required = 1 AND password_change_required_at IS NOT NULL)
    OR (password_change_required = 0 AND password_change_required_at IS NULL)
  )
) STRICT;

CREATE TABLE sessions (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  user_id              INTEGER NOT NULL REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  session_token_digest BLOB NOT NULL UNIQUE CHECK (length(session_token_digest) = 32), -- raw 32-byte bearer 的 SHA-256 digest；raw bearer 只存在于 Cookie 与认证瞬间内存（DATA-AUTH-003）
  auth_revision_at_issue INTEGER NOT NULL CHECK (auth_revision_at_issue > 0),
  created_at           TEXT NOT NULL,
  last_active_at       TEXT NOT NULL,
  idle_expires_at      TEXT NOT NULL,     -- 空闲 12 小时
  absolute_expires_at  TEXT NOT NULL,     -- 绝对 7 天
  revoked_at           TEXT
) STRICT;
CREATE INDEX idx_sessions_user ON sessions (user_id);
CREATE INDEX idx_sessions_expiry ON sessions (absolute_expires_at);

CREATE TABLE user_viewed (
  id          INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  user_id     INTEGER NOT NULL REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  object_type TEXT NOT NULL CHECK (object_type IN ('initial_analysis','investigation','inspection_run','knowledge_import_batch')),
  object_id   INTEGER NOT NULL,
  viewed_at   TEXT NOT NULL,
  UNIQUE (user_id, object_type, object_id)
) STRICT;
CREATE INDEX idx_user_viewed_user ON user_viewed (user_id);

-- Runtime 注册与长期服务 token 凭据（CONTEXT「服务身份」）：注册状态与 Admin 并发前提
-- （row_version）是持久权威；在线连接、boot/epoch、心跳 last_seen 是瞬时投影（内存），不落库
-- ——避免心跳改写 Admin row_version（DATA-RUNTIME-001）。当前 active 长期 token 的唯一权威是
-- runtime_slots.current_credential_id（单一 owner-side current authority，DATA-RUNTIME-001）；
-- 两阶段轮换的待确认 token 由 pending_credential_id 单行表达。runtime_credentials 行只记录
-- 不可变 generation 生命周期历史（confirmed_at/retired_at 事实），不存在与指针并列的第二权威。
CREATE TABLE runtime_slots (
  slot                  TEXT PRIMARY KEY CHECK (slot IN ('plinth','lintel')),
  state                 TEXT NOT NULL DEFAULT 'unregistered' CHECK (state IN ('unregistered','registered','revoked')),
  current_credential_id INTEGER REFERENCES runtime_credentials(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  pending_credential_id INTEGER REFERENCES runtime_credentials(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  row_version           INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- 注册/替换/轮换命令并发前提（DATA-ROWVER-001）
  created_at            TEXT NOT NULL,
  CHECK (
    (state IN ('unregistered','revoked') AND current_credential_id IS NULL AND pending_credential_id IS NULL)
    OR (state = 'registered' AND current_credential_id IS NOT NULL)
  ),
  CHECK (current_credential_id IS NULL OR pending_credential_id IS NULL OR current_credential_id <> pending_credential_id) -- current/pending 不得指向同一行
) STRICT;

-- Runtime 长期服务 token 的不可变 credential generation 历史（两阶段轮换：下发新 token -> Runtime
-- 持久化确认 -> 原子切换指针并吊销旧 token，CONTEXT「服务身份」）。本表只保存不可变来源字段
-- （slot/generation/token_digest/created_at）与两个一次性生命周期事实：confirmed_at（NULL ->
-- 时间戳，一次，Runtime 持久化确认）与 retired_at（NULL -> 时间戳，一次，吊销）；两个时间
-- 均不可回退、不可改写。current/pending 选择完全由 runtime_slots 指针承载，本表不宣称任何
-- 状态（DATA-RUNTIME-002）。一次性注册令牌不落库（内存短生命周期、单次使用，HTTP-COMMAND-012）；
-- 本表只保存长期 token digest。
CREATE TABLE runtime_credentials (
  id           INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  slot         TEXT NOT NULL REFERENCES runtime_slots(slot) ON UPDATE RESTRICT ON DELETE RESTRICT,
  generation   INTEGER NOT NULL CHECK (generation >= 1),
  token_digest BLOB NOT NULL CHECK (length(token_digest) = 32), -- 长期 token 只存 digest
  created_at   TEXT NOT NULL,
  confirmed_at TEXT,   -- Runtime 持久化确认时间（NULL -> 时间戳一次；DATA-RUNTIME-002）
  retired_at   TEXT,   -- 吊销时间（NULL -> 时间戳一次；DATA-RUNTIME-002）
  row_version  INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- DATA-ROWVER-001
  UNIQUE (slot, generation)
) STRICT;
CREATE INDEX idx_runtime_credentials_slot ON runtime_credentials (slot, generation);
CREATE INDEX idx_runtime_credentials_confirmed ON runtime_credentials (slot, confirmed_at);

-- ============================================================================
-- 2. 领域写命令账本与审计
-- ============================================================================

CREATE TABLE client_commands (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  principal_type     TEXT NOT NULL CHECK (principal_type IN ('user','service')),
  principal_id       INTEGER NOT NULL,           -- users.id 或 service principal id（service 无 users 行，不设 FK）
  client_command_id  TEXT NOT NULL,
  command_type       TEXT NOT NULL,
  request_digest     TEXT NOT NULL CHECK (length(request_digest) = 64), -- 全部语义字段（含 expected_* 并发前提）规范化后 SHA-256
  outcome            TEXT NOT NULL CHECK (outcome IN ('committed','rejected_known')),
  result_object_type TEXT,
  result_object_id   INTEGER,
  result_payload_json TEXT CHECK (result_payload_json IS NULL OR json_valid(result_payload_json)),
  created_at         TEXT NOT NULL,
  UNIQUE (principal_type, principal_id, client_command_id)
) STRICT;
CREATE INDEX idx_client_commands_principal ON client_commands (principal_type, principal_id, created_at);

CREATE TABLE audit_events (
  id                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  actor_type        TEXT NOT NULL CHECK (actor_type IN ('user','service','system')),
  actor_id          INTEGER NOT NULL,
  action            TEXT NOT NULL,
  client_command_id TEXT,
  outcome           TEXT NOT NULL CHECK (outcome IN ('success','failure','rejected','unknown')),
  domain_ref_type   TEXT,
  domain_ref_id     INTEGER,
  created_at        TEXT NOT NULL
) STRICT;
CREATE INDEX idx_audit_events_created ON audit_events (created_at);
CREATE INDEX idx_audit_events_actor ON audit_events (actor_type, actor_id);

CREATE TABLE audit_event_targets (
  id             INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  audit_event_id INTEGER NOT NULL REFERENCES audit_events(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  target_type    TEXT NOT NULL,
  target_id      INTEGER NOT NULL,
  target_version INTEGER
) STRICT;
CREATE INDEX idx_audit_event_targets_target ON audit_event_targets (target_type, target_id);

-- ============================================================================
-- 3. 告警接入
-- ============================================================================

CREATE TABLE alert_sources (
  id          INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  source_key  TEXT NOT NULL UNIQUE,                 -- 稳定用户 key，退役不复用
  protocol    TEXT NOT NULL CHECK (protocol IN ('alertmanager')), -- v1 仅 alertmanager
  enabled     INTEGER NOT NULL CHECK (enabled IN (0,1)),
  row_version INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- enable/disable 命令并发前提（DATA-ALERT-010）
  created_at  TEXT NOT NULL,
  disabled_at TEXT,
  CHECK (enabled = 1 OR disabled_at IS NOT NULL)
) STRICT;

CREATE TABLE alert_source_credentials (
  id         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  source_id  INTEGER NOT NULL REFERENCES alert_sources(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  digest     BLOB NOT NULL CHECK (length(digest) = 32), -- 32-byte Bearer 只存 digest
  state      TEXT NOT NULL CHECK (state IN ('Active','Retired')),
  row_version INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- 吊销命令并发前提（DATA-ALERT-009）
  created_at TEXT NOT NULL,
  retired_at TEXT,
  CHECK ((state = 'Retired' AND retired_at IS NOT NULL) OR (state = 'Active' AND retired_at IS NULL))
) STRICT;
CREATE INDEX idx_alert_source_credentials_source ON alert_source_credentials (source_id);

CREATE TABLE alert_deliveries (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  relay_id                   TEXT NOT NULL UNIQUE,   -- Stele relay id，重试幂等键
  source_id                  INTEGER NOT NULL REFERENCES alert_sources(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  credential_id              INTEGER NOT NULL REFERENCES alert_source_credentials(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 每次 Delivery 必带认证元数据（DATA-ALERT-008）
  credential_snapshot_version INTEGER NOT NULL CHECK (credential_snapshot_version >= 1), -- Stele 提交的只读快照版本
  protocol                   TEXT NOT NULL CHECK (protocol IN ('alertmanager')),
  body                       BLOB NOT NULL,          -- 精确原始 body 字节（可能非 UTF-8），直接存 SQLite（非 Artifact）
  body_size_bytes            INTEGER NOT NULL CHECK (body_size_bytes >= 0),
  integrity                  TEXT NOT NULL CHECK (integrity IN ('complete','truncated','rejected')),
  status                     TEXT NOT NULL CHECK (status IN ('processed','rejected')),
  group_key                  TEXT,
  received_at                TEXT NOT NULL,          -- Stele 接收时间
  committed_at               TEXT NOT NULL,           -- Quoin 提交时间（提交顺序裁决依据）
  CHECK (length(body) = body_size_bytes),             -- BLOB 长度以字节计（SQLite length() 对 BLOB 返回字节数）
  -- integrity 与 status 必须一致（DATA-ALERT-001/003）：不可枚举记 Rejected Delivery（rejected/rejected）；
  -- 顶层可解析或截断仍正常处理（complete|truncated 只配 processed）。
  CHECK (
    (integrity = 'rejected' AND status = 'rejected')
    OR (integrity IN ('complete','truncated') AND status = 'processed')
  )
) STRICT;
CREATE INDEX idx_alert_deliveries_source ON alert_deliveries (source_id, committed_at DESC);

CREATE TABLE alert_occurrences (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  source_id            INTEGER NOT NULL REFERENCES alert_sources(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  fingerprint          BLOB NOT NULL CHECK (length(fingerprint) = 8), -- 上游 64-bit 无符号指纹，大端 8 字节
  starts_at            TEXT NOT NULL,                -- 规范化 startsAt（UTC RFC3339Nano，无损）
  state                TEXT NOT NULL CHECK (state IN ('Firing','Resolved')),
  row_version          INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  labels_canonical     TEXT NOT NULL CHECK (json_valid(labels_canonical)), -- 不可变完整 labels 快照
  labels_digest        TEXT NOT NULL CHECK (length(labels_digest) = 64),
  business_system_id   INTEGER REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  first_seen_at        TEXT NOT NULL,
  last_state_change_at TEXT NOT NULL,
  resolved_at          TEXT,
  UNIQUE (source_id, fingerprint, starts_at),
  CHECK ((state = 'Resolved' AND resolved_at IS NOT NULL) OR (state = 'Firing' AND resolved_at IS NULL))
) STRICT;
CREATE INDEX idx_alert_occurrences_firing ON alert_occurrences (state, last_state_change_at DESC);
CREATE INDEX idx_alert_occurrences_business ON alert_occurrences (business_system_id);

CREATE TABLE alert_occurrence_labels (
  id            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  occurrence_id INTEGER NOT NULL REFERENCES alert_occurrences(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  name          TEXT NOT NULL,
  value         TEXT NOT NULL,
  UNIQUE (occurrence_id, name)
) STRICT;
CREATE INDEX idx_alert_occurrence_labels_name ON alert_occurrence_labels (name);

CREATE TABLE alert_observations (
  id               INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  delivery_id      INTEGER NOT NULL REFERENCES alert_deliveries(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  delivery_item_id INTEGER NOT NULL UNIQUE REFERENCES alert_delivery_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  occurrence_id    INTEGER NOT NULL REFERENCES alert_occurrences(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  observed_state   TEXT NOT NULL CHECK (observed_state IN ('firing','resolved')),
  starts_at_source TEXT NOT NULL,
  ends_at_source   TEXT,
  received_at      TEXT NOT NULL,
  committed_at     TEXT NOT NULL,
  effect           TEXT NOT NULL CHECK (effect IN ('initial_firing','repeat_firing','resolved','resolved_first','late_firing_after_resolved')),
  CHECK (
    (observed_state = 'firing' AND effect IN ('initial_firing','repeat_firing','late_firing_after_resolved'))
    OR (observed_state = 'resolved' AND effect IN ('resolved','resolved_first'))
  )
) STRICT;
CREATE INDEX idx_alert_observations_occurrence ON alert_observations (occurrence_id);

CREATE TABLE alert_delivery_items (
  id               INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  delivery_id      INTEGER NOT NULL REFERENCES alert_deliveries(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  item_index       INTEGER NOT NULL CHECK (item_index >= 0),
  status           TEXT NOT NULL CHECK (status IN ('ok','identity_conflict','fingerprint_mismatch')),
  fingerprint      BLOB NOT NULL CHECK (length(fingerprint) = 8),
  starts_at        TEXT NOT NULL,                    -- 来源声明的 startsAt
  ends_at          TEXT,
  labels_canonical TEXT NOT NULL CHECK (json_valid(labels_canonical)),
  error_detail     TEXT,
  UNIQUE (delivery_id, item_index)
) STRICT;
CREATE INDEX idx_alert_delivery_items_delivery ON alert_delivery_items (delivery_id);

CREATE TABLE alert_intake_issues (
  id                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  source_id         INTEGER NOT NULL REFERENCES alert_sources(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  delivery_id       INTEGER REFERENCES alert_deliveries(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  delivery_item_id  INTEGER REFERENCES alert_delivery_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  kind              TEXT NOT NULL CHECK (kind IN ('identity_conflict','fingerprint_mismatch','delivery_truncated')),
  detail_json       TEXT CHECK (detail_json IS NULL OR json_valid(detail_json)),
  acknowledged_at   TEXT,
  acknowledged_by   INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  row_version       INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  created_at        TEXT NOT NULL,
  CHECK (kind <> 'delivery_truncated' OR delivery_id IS NOT NULL),
  CHECK (kind = 'delivery_truncated' OR delivery_item_id IS NOT NULL),
  CHECK ((acknowledged_at IS NULL AND acknowledged_by IS NULL) OR (acknowledged_at IS NOT NULL AND acknowledged_by IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX ux_alert_intake_issue_truncated ON alert_intake_issues (delivery_id) WHERE kind = 'delivery_truncated';
CREATE INDEX idx_alert_intake_issues_source ON alert_intake_issues (source_id, created_at);

-- 有界派生变更日志：id 即单调递增 change_seq（AUTOINCREMENT，同事务分配、永不复用）；
-- 可清理（保留窗口由部署配置），不是告警历史权威源。最新行（MAX(id)）是回放 high-water，
-- 由触发器强制保留、永不删除（trg_alert_change_log_no_delete_latest，DATA-SSE-009）。
CREATE TABLE alert_change_log (
  id            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  occurrence_id INTEGER NOT NULL REFERENCES alert_occurrences(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  change_type   TEXT NOT NULL CHECK (change_type IN ('created','state_changed')),
  row_version   INTEGER NOT NULL CHECK (row_version >= 1),
  committed_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
) STRICT;
CREATE INDEX idx_alert_change_log_occurrence ON alert_change_log (occurrence_id);

-- 有界派生任务变更日志：id 即单调递增 task_change_seq（AUTOINCREMENT，同事务分配、永不复用）；
-- 与权威对象的状态/阶段变化同一事务写入；可清理（保留窗口由部署配置）、可丢弃、可重建，
-- 不是任务历史权威源（DATA-SSE-004/005/006）。object_type/object_id 为多态引用，由应用类型化校验。
CREATE TABLE task_change_log (
  id           INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  object_type  TEXT NOT NULL CHECK (object_type IN
    ('initial_analysis','execution_attempt','inspection_run','inspection_report',
     'tool_call','knowledge_import_batch','knowledge_candidate','browser_operation')),
  object_id    INTEGER NOT NULL,
  change_type  TEXT NOT NULL CHECK (change_type IN ('created','state_changed')),
  row_version  INTEGER NOT NULL CHECK (row_version >= 1),
  committed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
) STRICT;
CREATE INDEX idx_task_change_log_object ON task_change_log (object_type, object_id);

-- ============================================================================
-- 4. 初步分析、调查与附件
-- ============================================================================

CREATE TABLE initial_analyses (
  id                    INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  occurrence_id         INTEGER NOT NULL REFERENCES alert_occurrences(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  state                 TEXT NOT NULL CHECK (state IN ('Queued','Running','Succeeded','Failed','Cancelled','Interrupted')),
  row_version           INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  input_snapshot_digest TEXT NOT NULL CHECK (length(input_snapshot_digest) = 64),
  created_by            INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at            TEXT NOT NULL
) STRICT;
CREATE UNIQUE INDEX ux_initial_analysis_active ON initial_analyses (occurrence_id) WHERE state IN ('Queued','Running');
CREATE INDEX idx_initial_analysis_occurrence ON initial_analyses (occurrence_id, created_at DESC);

CREATE TABLE initial_analysis_outputs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  analysis_id INTEGER NOT NULL UNIQUE REFERENCES initial_analyses(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 首个成功封存，一个分析最多一个输出
  attempt_id  INTEGER REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  model_id    TEXT NOT NULL,
  content     TEXT NOT NULL,
  created_at  TEXT NOT NULL
) STRICT;

CREATE TABLE investigations (
  id                      INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  created_by              INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  current_head_message_id INTEGER REFERENCES investigation_messages(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at              TEXT NOT NULL
) STRICT;

CREATE TABLE investigation_messages (
  id                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  investigation_id  INTEGER NOT NULL REFERENCES investigations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  seq               INTEGER NOT NULL CHECK (seq >= 1),
  role              TEXT NOT NULL CHECK (role IN ('user','assistant')),
  status            TEXT NOT NULL CHECK (status IN ('active','withdrawn')),
  content           TEXT NOT NULL,
  client_command_id TEXT,
  parent_message_id INTEGER REFERENCES investigation_messages(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at        TEXT NOT NULL,
  UNIQUE (investigation_id, seq)
) STRICT;
CREATE INDEX idx_investigation_messages_parent ON investigation_messages (parent_message_id);

CREATE TABLE text_attachments (
  id                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  message_id        INTEGER NOT NULL UNIQUE REFERENCES investigation_messages(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 每条消息最多一个
  source_material_id INTEGER NOT NULL UNIQUE REFERENCES source_materials(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  artifact_id       INTEGER NOT NULL REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 正文存 Artifact
  original_filename TEXT NOT NULL,
  size_bytes        INTEGER NOT NULL CHECK (size_bytes >= 0),
  digest            TEXT NOT NULL CHECK (length(digest) = 64),
  uploaded_by       INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  uploaded_at       TEXT NOT NULL
) STRICT;

-- ============================================================================
-- 5. 来源材料、配置与观测资源
-- ============================================================================

CREATE TABLE source_materials (
  id         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  kind       TEXT NOT NULL CHECK (kind IN ('text_attachment','knowledge_import')),
  digest     TEXT NOT NULL CHECK (length(digest) = 64),
  size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
  content    TEXT,   -- knowledge_import 原文；text_attachment 正文存 Artifact，content 为 NULL
  created_by INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at TEXT NOT NULL,
  CHECK ((kind = 'knowledge_import' AND content IS NOT NULL) OR (kind = 'text_attachment' AND content IS NULL))
) STRICT;

CREATE TABLE label_contracts (
  id            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  version       INTEGER NOT NULL CHECK (version >= 1),
  contract_json TEXT NOT NULL CHECK (json_valid(contract_json)),
  digest        TEXT NOT NULL CHECK (length(digest) = 64),
  state         TEXT NOT NULL CHECK (state IN ('draft','active','retired')),
  row_version   INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- 激活命令并发前提（DATA-CONFIG-005）
  created_at    TEXT NOT NULL,
  activated_at  TEXT,
  activated_by  INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  UNIQUE (version)
) STRICT;
CREATE UNIQUE INDEX ux_label_contract_active ON label_contracts (state) WHERE state = 'active';

-- Label Contract 当前指针单行聚合：激活命令的并发前提权威（DATA-CONFIG-005/006）。
-- current_contract_id 必须指向 active 契约（触发器强制）。
CREATE TABLE label_contract_state (
  id                  INTEGER PRIMARY KEY CHECK (id = 1),
  current_contract_id INTEGER REFERENCES label_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  row_version         INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  updated_at          TEXT NOT NULL
) STRICT;

CREATE TABLE business_systems (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  key                       TEXT NOT NULL UNIQUE,  -- 稳定用户 key，退役不复用
  display_name              TEXT NOT NULL,
  enabled                   INTEGER NOT NULL CHECK (enabled IN (0,1)),
  row_version               INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- 系统行并发前提（DATA-CONFIG-005）
  current_config_version_id INTEGER REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at                TEXT NOT NULL
) STRICT;

CREATE TABLE business_system_config_versions (
  id                       INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  business_system_id       INTEGER NOT NULL REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  version_seq              INTEGER NOT NULL CHECK (version_seq >= 1),
  state                    TEXT NOT NULL CHECK (state IN ('draft','published','superseded')),
  yaml_body                TEXT NOT NULL,
  parser_version           TEXT NOT NULL,
  schema_version           TEXT NOT NULL,
  label_contract_version_id INTEGER REFERENCES label_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  digest                   TEXT NOT NULL CHECK (length(digest) = 64),
  base_version_id          INTEGER REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_by               INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at               TEXT NOT NULL,
  published_at             TEXT,
  published_by             INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  UNIQUE (business_system_id, version_seq)
) STRICT;
CREATE UNIQUE INDEX ux_business_config_published ON business_system_config_versions (business_system_id) WHERE state = 'published';

CREATE TABLE config_discoveries (
  id                       INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  config_version_id        INTEGER NOT NULL REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  discovery_key            TEXT NOT NULL,          -- 跨版本稳定 key
  display_name             TEXT NOT NULL,
  selector                 TEXT NOT NULL,          -- 单个 instant vector selector
  identity_labels_json     TEXT NOT NULL CHECK (json_valid(identity_labels_json)),
  refresh_interval_seconds INTEGER NOT NULL CHECK (refresh_interval_seconds > 0),
  UNIQUE (config_version_id, discovery_key)
) STRICT;

CREATE TABLE config_plans (
  id                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  config_version_id INTEGER NOT NULL REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  plan_key          TEXT NOT NULL,                  -- 跨版本稳定 key
  display_name      TEXT NOT NULL,
  cron              TEXT,                          -- 标准五字段 cron；NULL = 仅人工运行
  timezone          TEXT NOT NULL,
  UNIQUE (config_version_id, plan_key)
) STRICT;

CREATE TABLE config_checks (
  id                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  plan_id           INTEGER NOT NULL REFERENCES config_plans(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  check_key         TEXT NOT NULL,                  -- 跨版本稳定 key
  display_name      TEXT NOT NULL,
  analysis_question TEXT NOT NULL,
  kind              TEXT NOT NULL CHECK (kind IN ('promql','browser')),
  expression        TEXT,                          -- promql：表达式字面量；browser：相对路径/类型化参数
  journey_id        TEXT,
  UNIQUE (plan_id, check_key),
  CHECK ((kind = 'promql' AND expression IS NOT NULL) OR (kind = 'browser' AND journey_id IS NOT NULL))
) STRICT;

CREATE TABLE observed_resources (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  business_system_id        INTEGER NOT NULL REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  discovery_key             TEXT NOT NULL,
  identity_key              TEXT NOT NULL,          -- 按 label 名排序的 identity label/value 规范编码（相等性权威）
  identity_digest           TEXT CHECK (identity_digest IS NULL OR length(identity_digest) = 64),
  display_name              TEXT,
  labels_json               TEXT NOT NULL CHECK (json_valid(labels_json)),
  observed_at               TEXT,
  current                   INTEGER NOT NULL DEFAULT 0 CHECK (current IN (0,1)),
  last_successful_refresh_at TEXT,
  stale                     INTEGER NOT NULL DEFAULT 0 CHECK (stale IN (0,1)),
  created_at                TEXT NOT NULL,
  UNIQUE (business_system_id, discovery_key, identity_key)
) STRICT;
CREATE INDEX idx_observed_resources_bs ON observed_resources (business_system_id, discovery_key);

CREATE TABLE observed_resource_identity_labels (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  observed_resource_id INTEGER NOT NULL REFERENCES observed_resources(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  name                 TEXT NOT NULL,
  value                TEXT NOT NULL,
  UNIQUE (observed_resource_id, name)
) STRICT;

CREATE TABLE observed_refresh_log (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  business_system_id INTEGER NOT NULL REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  discovery_key      TEXT NOT NULL,
  started_at         TEXT NOT NULL,
  completed_at       TEXT,
  complete           INTEGER NOT NULL DEFAULT 0 CHECK (complete IN (0,1)),
  warnings_json      TEXT CHECK (warnings_json IS NULL OR json_valid(warnings_json)),
  error_detail       TEXT
) STRICT;

-- ============================================================================
-- 6. 连接、凭据与浏览器身份
-- ============================================================================

CREATE TABLE connections (
  id                                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  name                              TEXT NOT NULL UNIQUE,  -- 稳定用户 key，退役不复用
  type                              TEXT NOT NULL CHECK (type IN ('thanos','kubernetes','model_provider')),
  enabled                           INTEGER NOT NULL CHECK (enabled IN (0,1)),
  row_version                       INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- enable/disable/rotate 命令并发前提（DATA-CONN-005）
  revalidation_required             INTEGER NOT NULL DEFAULT 0 CHECK (revalidation_required IN (0,1)),
  current_revision_id               INTEGER REFERENCES connection_revisions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  current_credential_generation_id  INTEGER REFERENCES credential_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at                        TEXT NOT NULL
) STRICT;

CREATE TABLE connection_revisions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  connection_id INTEGER NOT NULL REFERENCES connections(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  revision_seq  INTEGER NOT NULL CHECK (revision_seq >= 1),
  config_json   TEXT NOT NULL CHECK (json_valid(config_json)), -- 地址/TLS/CA/用户名等非秘密配置
  created_by    INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at    TEXT NOT NULL,
  UNIQUE (connection_id, revision_seq)
) STRICT;

CREATE TABLE credential_generations (
  id             INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  connection_id  INTEGER NOT NULL REFERENCES connections(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  generation_seq INTEGER NOT NULL CHECK (generation_seq >= 1),
  ciphertext     BLOB NOT NULL,                     -- AEAD 密文（envelope 见 security.md）
  created_by     INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at     TEXT NOT NULL,
  UNIQUE (connection_id, generation_seq)
) STRICT;

CREATE TABLE browser_identities (
  id                            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  business_system_id            INTEGER NOT NULL UNIQUE REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 每业务系统恰好一个
  name                          TEXT NOT NULL,
  start_url                     TEXT NOT NULL,
  state                         TEXT NOT NULL CHECK (state IN ('Ready','AuthenticationRequired')),
  row_version                   INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- 配置/发布 generation 命令并发前提（DATA-BROWSER-004）
  current_profile_generation_id INTEGER REFERENCES browser_profile_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at                    TEXT NOT NULL
) STRICT;

CREATE TABLE browser_profile_generations (
  id            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  identity_id   INTEGER NOT NULL REFERENCES browser_identities(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  generation    INTEGER NOT NULL CHECK (generation >= 0),
  probe_version TEXT,
  published_at  TEXT NOT NULL,
  published_by  INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  UNIQUE (identity_id, generation)
) STRICT;

CREATE TABLE browser_operations (
  id                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  identity_id       INTEGER NOT NULL REFERENCES browser_identities(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  attempt_id        INTEGER REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- exploration/journey 对应执行尝试；manual_login 无（DATA-BROWSER-003）
  kind              TEXT NOT NULL CHECK (kind IN ('manual_login','exploration','journey')),
  actor_type        TEXT NOT NULL CHECK (actor_type IN ('user','service','system')),
  actor_id          INTEGER NOT NULL,
  started_at        TEXT NOT NULL,
  ended_at          TEXT,
  result            TEXT CHECK (result IS NULL OR result IN ('success','failed','cancelled','interrupted')),
  row_version       INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  new_generation_id INTEGER REFERENCES browser_profile_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  trace_artifact_id INTEGER REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  log_json          TEXT CHECK (log_json IS NULL OR json_valid(log_json)),
  CHECK (kind = 'manual_login' OR attempt_id IS NOT NULL), -- 非人工登录的浏览器操作必须绑定执行尝试
  CHECK (result IS NULL OR ended_at IS NOT NULL) -- 结果一旦产生即结束，必须带结束时间
) STRICT;
CREATE INDEX idx_browser_operations_identity ON browser_operations (identity_id, started_at);

-- ============================================================================
-- 7. 巡检运行与检查结果
-- ============================================================================

CREATE TABLE inspection_runs (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  business_system_id        INTEGER NOT NULL REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  plan_key                  TEXT NOT NULL,
  config_version_id         INTEGER NOT NULL REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  label_contract_version_id INTEGER NOT NULL REFERENCES label_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  trigger_kind              TEXT NOT NULL CHECK (trigger_kind IN ('schedule','manual')),
  scheduled_for             TEXT,                    -- UTC；NULL = 人工触发
  state                     TEXT NOT NULL CHECK (state IN ('Queued','WaitingForCapacity','Running','Completed','CompletedWithGaps','Failed','Cancelled','Interrupted','SkippedOverlap')),
  row_version               INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  evidence_at               TEXT,                    -- 真正采证开始时生成
  rerun_of_id               INTEGER REFERENCES inspection_runs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  journey_catalog_digest    TEXT NOT NULL CHECK (length(journey_catalog_digest) = 64),
  journey_catalog_version   TEXT NOT NULL,
  created_at                TEXT NOT NULL
) STRICT;
CREATE UNIQUE INDEX ux_inspection_run_scheduled ON inspection_runs (business_system_id, plan_key, scheduled_for) WHERE scheduled_for IS NOT NULL;
CREATE UNIQUE INDEX ux_inspection_run_active ON inspection_runs (business_system_id, plan_key)
  WHERE state IN ('Queued','WaitingForCapacity','Running');
CREATE INDEX idx_inspection_runs_plan ON inspection_runs (business_system_id, plan_key, created_at DESC);

CREATE TABLE inspection_check_results (
  id          INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  run_id      INTEGER NOT NULL REFERENCES inspection_runs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  check_key   TEXT NOT NULL,
  status      TEXT NOT NULL CHECK (status IN ('ok','error','gap')),
  evidence_id INTEGER REFERENCES evidence(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  gap_reason  TEXT,
  created_at  TEXT NOT NULL,
  UNIQUE (run_id, check_key),
  CHECK (
    (status = 'ok' AND evidence_id IS NOT NULL)
    OR (status IN ('error','gap') AND gap_reason IS NOT NULL)
  )
) STRICT;

-- ============================================================================
-- 8. 执行尝试、模型/工具调用、证据与报告
-- ============================================================================

CREATE TABLE execution_attempts (
  id                       INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  attempt_type             TEXT NOT NULL CHECK (attempt_type IN ('initial_analysis','investigation','inspection_analysis','inspection_collection','browser_exploration')),
  scope_type               TEXT NOT NULL CHECK (scope_type IN ('analysis','investigation','run','run_check')),
  scope_id                 INTEGER NOT NULL,
  check_key                TEXT,   -- 非空 = 子 Attempt（run_check，不参与 active 唯一约束）；空 = 参与
  state                    TEXT NOT NULL CHECK (state IN ('Queued','Assigned','Running','Cancelling','Succeeded','Failed','Cancelled','Interrupted')),
  row_version              INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  runtime_slot             TEXT CHECK (runtime_slot IN ('plinth','lintel')), -- 派发绑定的 Runtime；一旦绑定不可改（DATA-ATTEMPT-001/007）
  requested_by_tool_call_id INTEGER REFERENCES tool_calls(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 跨 Runtime 子执行：Plinth Tool Call 请求 Lintel（DATA-ATTEMPT-007）
  connection_revision_id   INTEGER REFERENCES connection_revisions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  credential_generation_id INTEGER REFERENCES credential_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  connection_epoch         INTEGER,
  boot_id                  TEXT,
  lease_until              TEXT,
  accepted_at              TEXT,
  started_at               TEXT,
  ended_at                 TEXT,
  termination_reason       TEXT CHECK (termination_reason IS NULL OR termination_reason IN
                             ('timeout','rate_limited','provider_unavailable','invalid_response','tool_error','artifact_commit_failed',
                              'cancelled','connection_disabled','business_system_disabled','lease_expired','replaced','revoked')),
  created_at               TEXT NOT NULL,
  CHECK (
    (state = 'Queued' AND runtime_slot IS NULL AND boot_id IS NULL AND connection_epoch IS NULL AND lease_until IS NULL AND accepted_at IS NULL)
    OR (state = 'Assigned' AND runtime_slot IS NOT NULL AND boot_id IS NOT NULL AND connection_epoch IS NOT NULL AND lease_until IS NOT NULL AND accepted_at IS NULL)
    OR (state IN ('Running','Cancelling') AND runtime_slot IS NOT NULL AND boot_id IS NOT NULL AND connection_epoch IS NOT NULL AND lease_until IS NOT NULL AND accepted_at IS NOT NULL)
    OR (state IN ('Succeeded','Failed','Cancelled','Interrupted') AND (
      -- 终态只允许两种完整形态（DATA-ATTEMPT-001）：从未派发（绑定五字段全空），
      -- 或曾派发（runtime_slot/boot_id/connection_epoch/lease_until 完整非空；accepted_at 可空=
      -- Assigned 直接终结，非空=曾 Running/Cancelling）。绝不允许任意部分绑定。
      (runtime_slot IS NULL AND boot_id IS NULL AND connection_epoch IS NULL AND lease_until IS NULL AND accepted_at IS NULL)
      OR (runtime_slot IS NOT NULL AND boot_id IS NOT NULL AND connection_epoch IS NOT NULL AND lease_until IS NOT NULL)
    ))
  ),
  CHECK (connection_epoch IS NULL OR connection_epoch >= 1), -- 递增 connection epoch 必须为正（DATA-ATTEMPT-001）
  CHECK (requested_by_tool_call_id IS NULL OR attempt_type = 'browser_exploration'), -- 只有浏览器探索可作为跨 Runtime 子执行
  CHECK ( -- Attempt 类型与 Runtime slot 的固定映射（DATA-ATTEMPT-001）：浏览器类只派发 lintel，模型/Agent 类只派发 plinth；未派发（runtime_slot IS NULL）不受限
    runtime_slot IS NULL
    OR (attempt_type IN ('browser_exploration','inspection_collection') AND runtime_slot = 'lintel')
    OR (attempt_type IN ('initial_analysis','investigation','inspection_analysis') AND runtime_slot = 'plinth')
  )
) STRICT;
CREATE UNIQUE INDEX ux_execution_attempt_active_scope ON execution_attempts (scope_type, scope_id)
  WHERE state IN ('Queued','Assigned','Running','Cancelling') AND check_key IS NULL;
CREATE INDEX idx_execution_attempts_scope ON execution_attempts (scope_type, scope_id);
CREATE INDEX idx_execution_attempts_lease ON execution_attempts (state, lease_until);

CREATE TABLE model_calls (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  attempt_id                INTEGER NOT NULL REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  call_seq                  INTEGER NOT NULL CHECK (call_seq >= 1),
  model_id                  TEXT NOT NULL,
  connection_revision_id    INTEGER REFERENCES connection_revisions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  credential_generation_id  INTEGER REFERENCES credential_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  prompt_digest             TEXT CHECK (prompt_digest IS NULL OR length(prompt_digest) = 64),
  tool_schema_digest        TEXT CHECK (tool_schema_digest IS NULL OR length(tool_schema_digest) = 64),
  input_snapshot_digest     TEXT CHECK (input_snapshot_digest IS NULL OR length(input_snapshot_digest) = 64),
  usage_json                TEXT CHECK (usage_json IS NULL OR json_valid(usage_json)),
  latency_ms                INTEGER,
  retry_seq                 INTEGER NOT NULL DEFAULT 0 CHECK (retry_seq >= 0),
  status                    TEXT NOT NULL CHECK (status IN ('running','succeeded','failed','cancelled')),
  termination_reason        TEXT CHECK (termination_reason IS NULL OR termination_reason IN
                              ('timeout','rate_limited','provider_unavailable','invalid_response','tool_error','artifact_commit_failed','cancelled')),
  started_at                TEXT NOT NULL,
  ended_at                  TEXT,
  UNIQUE (attempt_id, call_seq)
) STRICT;

CREATE TABLE tool_calls (
  id             INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  attempt_id     INTEGER NOT NULL REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  call_seq       INTEGER NOT NULL CHECK (call_seq >= 1),
  tool_name      TEXT NOT NULL,
  tool_version   TEXT,
  arguments_json TEXT NOT NULL CHECK (json_valid(arguments_json)),
  status         TEXT NOT NULL CHECK (status IN ('pending','succeeded','failed','cancelled')),
  row_version    INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  result_json    TEXT CHECK (result_json IS NULL OR json_valid(result_json)),
  error_detail   TEXT,
  created_at     TEXT NOT NULL,
  ended_at       TEXT,
  UNIQUE (attempt_id, call_seq)
) STRICT;

CREATE TABLE evidence (
  id            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  attempt_id    INTEGER REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  tool_call_id  INTEGER REFERENCES tool_calls(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  target_type   TEXT NOT NULL,
  target_id     INTEGER NOT NULL,
  params_json   TEXT NOT NULL CHECK (json_valid(params_json)),
  observed_at   TEXT NOT NULL,
  result_json   TEXT CHECK (result_json IS NULL OR json_valid(result_json)),
  artifact_id   INTEGER REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  warnings_json TEXT CHECK (warnings_json IS NULL OR json_valid(warnings_json)),
  errors_json   TEXT CHECK (errors_json IS NULL OR json_valid(errors_json)),
  integrity     TEXT NOT NULL CHECK (integrity IN ('complete','incomplete')),
  created_at    TEXT NOT NULL,
  CHECK ((result_json IS NOT NULL AND artifact_id IS NULL) OR (artifact_id IS NOT NULL AND result_json IS NULL)) -- 正文位置恰好一个
) STRICT;
CREATE INDEX idx_evidence_attempt ON evidence (attempt_id);
CREATE INDEX idx_evidence_target ON evidence (target_type, target_id);

CREATE TABLE inspection_reports (
  id             INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  run_id         INTEGER NOT NULL REFERENCES inspection_runs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  version        INTEGER NOT NULL CHECK (version >= 1),
  attempt_id     INTEGER REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  evidence_digest TEXT NOT NULL CHECK (length(evidence_digest) = 64),
  model_id       TEXT NOT NULL,
  prompt_digest  TEXT CHECK (prompt_digest IS NULL OR length(prompt_digest) = 64),
  content        TEXT NOT NULL,
  created_at     TEXT NOT NULL,
  UNIQUE (run_id, version)
) STRICT;

-- ============================================================================
-- 9. Artifact 与来源材料引用
-- ============================================================================

-- 物理 blob：每份内容唯一规范持久副本；sha256/storage_key 全局唯一（DATA-ARTIFACT-002）。
-- 本表不可改写；物理文件只在无任何逻辑引用且与备份/GC 互斥时清理（DATA-ARTIFACT-004）。
CREATE TABLE artifact_blobs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  sha256      TEXT NOT NULL UNIQUE CHECK (length(sha256) = 64),
  size_bytes  INTEGER NOT NULL CHECK (size_bytes >= 0),
  storage_key TEXT NOT NULL UNIQUE,              -- 仅由 hash 推导的路径
  created_at  TEXT NOT NULL
) STRICT;

-- 逻辑 Artifact：同一 blob 可被多个逻辑引用，以不同 owner/kind/sensitive/retention 表达
-- 访问与保留规则（DATA-ARTIFACT-003）；访问、过期、下载授权与审计按本表裁决。
CREATE TABLE artifacts (
  id             INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  blob_id        INTEGER NOT NULL REFERENCES artifact_blobs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  kind           TEXT NOT NULL CHECK (kind IN ('attachment','screenshot','trace','tool_result','report_file')),
  sensitive      INTEGER NOT NULL DEFAULT 0 CHECK (sensitive IN (0,1)), -- raw trace 固定 sensitive=1
  retention_kind TEXT NOT NULL CHECK (retention_kind IN ('long_term','generated')),
  owner_type     TEXT NOT NULL,
  owner_id       INTEGER NOT NULL,
  expires_at     TEXT,
  body_expired   INTEGER NOT NULL DEFAULT 0 CHECK (body_expired IN (0,1)),
  created_at     TEXT NOT NULL,
  created_by     INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CHECK (kind <> 'trace' OR sensitive = 1)
) STRICT;
CREATE INDEX idx_artifacts_owner ON artifacts (owner_type, owner_id);
CREATE INDEX idx_artifacts_blob ON artifacts (blob_id);

-- Runtime Artifact 上传 ledger：上传身份与重试幂等权威（DATA-ARTIFACT-006）。upload_id 由
-- Runtime 生成并在整个重试生命周期保持稳定；同 upload_id 同摘要重试返回原 artifact_id，
-- 同 upload_id 不同摘要/owner 冲突拒绝；v1 整单重传，不做 offset 续传。
CREATE TABLE runtime_artifact_uploads (
  id             INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  upload_id      TEXT NOT NULL UNIQUE,
  attempt_id     INTEGER NOT NULL REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  boot_id        TEXT NOT NULL,  -- 与 Attempt 派发绑定的 boot_id；不可改（DATA-ARTIFACT-006）
  connection_epoch INTEGER NOT NULL CHECK (connection_epoch >= 1), -- 旧 epoch 上传只审计、拒绝提交
  owner_type     TEXT NOT NULL,
  owner_id       INTEGER NOT NULL,
  kind           TEXT NOT NULL CHECK (kind IN ('attachment','screenshot','trace','tool_result','report_file')),
  retention_kind TEXT NOT NULL CHECK (retention_kind IN ('long_term','generated')),
  sensitive      INTEGER NOT NULL DEFAULT 0 CHECK (sensitive IN (0,1)),
  size_bytes     INTEGER NOT NULL CHECK (size_bytes >= 0),
  sha256         TEXT NOT NULL CHECK (length(sha256) = 64),
  state          TEXT NOT NULL CHECK (state IN ('uploading','committed','rejected')),
  row_version    INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  artifact_id    INTEGER REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at     TEXT NOT NULL,
  committed_at   TEXT,
  -- 状态-结果双向一致（DATA-ARTIFACT-006）：committed 必须带 artifact_id 与 committed_at；
  -- uploading/rejected 必须两者皆无（不允许引用已提交 Artifact 或伪造提交时间）。
  CHECK (state <> 'committed' OR (artifact_id IS NOT NULL AND committed_at IS NOT NULL)),
  CHECK (state = 'committed' OR (artifact_id IS NULL AND committed_at IS NULL)),
  CHECK (kind <> 'trace' OR sensitive = 1)
) STRICT;
CREATE INDEX idx_runtime_artifact_uploads_attempt ON runtime_artifact_uploads (attempt_id, created_at);

-- ============================================================================
-- 10. 知识沉淀
-- ============================================================================

CREATE TABLE knowledge_import_batches (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  source_material_id INTEGER NOT NULL UNIQUE REFERENCES source_materials(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  state              TEXT NOT NULL CHECK (state IN ('Processing','AwaitingConfirmation','Failed','Completed','Cancelled')),
  row_version        INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  generation         INTEGER NOT NULL DEFAULT 1 CHECK (generation >= 1),
  created_by         INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at         TEXT NOT NULL
) STRICT;

CREATE TABLE knowledge_candidates (
  id                      INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  import_batch_id         INTEGER REFERENCES knowledge_import_batches(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  source_type             TEXT NOT NULL CHECK (source_type IN ('initial_analysis_output','inspection_report','investigation_message','source_material')),
  source_id               INTEGER NOT NULL,
  generation              INTEGER NOT NULL DEFAULT 1 CHECK (generation >= 1),
  state                   TEXT NOT NULL CHECK (state IN ('AwaitingConfirmation','Confirmed','Excluded','Superseded','SourceInvalid')),
  row_version             INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  original_suggestion_json TEXT NOT NULL CHECK (json_valid(original_suggestion_json)), -- 模型原始建议不可变
  draft_title             TEXT,
  draft_body              TEXT,
  draft_revision          INTEGER NOT NULL DEFAULT 0 CHECK (draft_revision >= 0),
  confirmed_knowledge_id  INTEGER REFERENCES reusable_knowledge(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_by              INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at              TEXT NOT NULL,
  CHECK (
    (source_type = 'source_material' AND import_batch_id IS NOT NULL)
    OR (source_type IN ('initial_analysis_output','inspection_report','investigation_message') AND import_batch_id IS NULL)
  )
) STRICT;
CREATE UNIQUE INDEX ux_knowledge_candidate_confirmed ON knowledge_candidates (confirmed_knowledge_id) WHERE confirmed_knowledge_id IS NOT NULL;
CREATE INDEX idx_knowledge_candidates_batch ON knowledge_candidates (import_batch_id);
CREATE INDEX idx_knowledge_candidates_source ON knowledge_candidates (source_type, source_id);

CREATE TABLE reusable_knowledge (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  current_version_id INTEGER REFERENCES knowledge_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 同时最多一个 current
  row_version        INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- current 指针切换并发前提（DATA-KNOWLEDGE-007）
  created_by         INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at         TEXT NOT NULL
) STRICT;

CREATE TABLE knowledge_versions (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  knowledge_id        INTEGER NOT NULL REFERENCES reusable_knowledge(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  version_seq         INTEGER NOT NULL CHECK (version_seq >= 1),
  title               TEXT NOT NULL,
  body                TEXT NOT NULL,
  scope_json          TEXT CHECK (scope_json IS NULL OR json_valid(scope_json)),
  conditions_json     TEXT CHECK (conditions_json IS NULL OR json_valid(conditions_json)),
  limitations_json    TEXT CHECK (limitations_json IS NULL OR json_valid(limitations_json)), -- 限制/条件（DATA-KNOWLEDGE-001）
  source_candidate_id INTEGER REFERENCES knowledge_candidates(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_by          INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at          TEXT NOT NULL,
  UNIQUE (knowledge_id, version_seq)
) STRICT;

-- 检索资格投影：exit 一旦发生即单调粘性，永不自动复活（DATA-KNOWLEDGE-*）。
CREATE TABLE knowledge_version_retrieval_state (
  knowledge_version_id INTEGER PRIMARY KEY REFERENCES knowledge_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  exited       INTEGER NOT NULL DEFAULT 0 CHECK (exited IN (0,1)),
  row_version  INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- stop-reuse 命令并发前提（DATA-KNOWLEDGE-007）
  exited_at    TEXT,
  exit_reason  TEXT CHECK (exit_reason IS NULL OR exit_reason IN ('source_rejected','stopped')),
  updated_at   TEXT NOT NULL,
  CHECK ((exited = 0 AND exited_at IS NULL AND exit_reason IS NULL) OR (exited = 1 AND exited_at IS NOT NULL AND exit_reason IS NOT NULL))
) STRICT;

-- 派生检索文档：只包含当前合格（current ∧ 未停用 ∧ 来源有效 ∧ 未 exit）版本；
-- 资格变化在同一事务增删文档，并经由触发器同步 FTS5。可校验、可重建。
CREATE TABLE knowledge_search_docs (
  knowledge_version_id INTEGER PRIMARY KEY REFERENCES knowledge_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 稳定显式整数 rowid
  title TEXT NOT NULL,
  body  TEXT NOT NULL
) STRICT;

CREATE VIRTUAL TABLE knowledge_fts USING fts5(
  title, body,
  content = 'knowledge_search_docs',
  content_rowid = 'knowledge_version_id',
  tokenize = 'trigram'
);

CREATE TABLE embedding_generations (
  id            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  model_name    TEXT NOT NULL,
  model_version TEXT NOT NULL,
  generation    INTEGER NOT NULL CHECK (generation >= 1),
  state         TEXT NOT NULL DEFAULT 'building' CHECK (state IN ('building','current','retired')),
  vector_dim    INTEGER CHECK (vector_dim IS NULL OR vector_dim > 0),
  built_at      TEXT,
  validated_at  TEXT,
  created_at    TEXT NOT NULL,
  UNIQUE (generation),
  CHECK (state <> 'current' OR vector_dim IS NOT NULL)
) STRICT;
CREATE UNIQUE INDEX ux_embedding_generation_current ON embedding_generations (state) WHERE state = 'current';

CREATE TABLE embeddings (
  knowledge_version_id    INTEGER NOT NULL REFERENCES knowledge_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  embedding_generation_id INTEGER NOT NULL REFERENCES embedding_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  state      TEXT NOT NULL CHECK (state IN ('pending','ready','failed')),
  vector     BLOB,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (knowledge_version_id, embedding_generation_id),
  CHECK (
    (state = 'ready' AND vector IS NOT NULL AND length(vector) % 4 = 0)
    OR (state IN ('pending','failed') AND vector IS NULL)
  )
) STRICT;
CREATE INDEX idx_embeddings_generation ON embeddings (embedding_generation_id);

CREATE TABLE diagnosis_feedback (
  id          INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  target_type TEXT NOT NULL CHECK (target_type IN ('initial_analysis_output','inspection_report','investigation_message')),
  target_id   INTEGER NOT NULL,
  value       TEXT NOT NULL CHECK (value IN ('adopted','executed','verified_effective','rejected')),
  created_by  INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at  TEXT NOT NULL
) STRICT;
CREATE INDEX idx_diagnosis_feedback_target ON diagnosis_feedback (target_type, target_id);

-- ============================================================================
-- 11. 备份、Schema 状态与迁移账本
-- ============================================================================

CREATE TABLE backup_settings (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  enabled         INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  schedule_cron   TEXT,
  timezone        TEXT NOT NULL DEFAULT 'UTC',
  retention_count INTEGER NOT NULL DEFAULT 30 CHECK (retention_count >= 1),
  row_version     INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- 设置更新并发前提（DATA-BACKUP-008）
  updated_by      INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  updated_at      TEXT NOT NULL
) STRICT;

CREATE TABLE backups (
  id              INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  status          TEXT NOT NULL CHECK (status IN ('succeeded','failed')),
  db_sha256       TEXT CHECK (db_sha256 IS NULL OR length(db_sha256) = 64),
  manifest_sha256 TEXT CHECK (manifest_sha256 IS NULL OR length(manifest_sha256) = 64),
  artifact_count  INTEGER CHECK (artifact_count IS NULL OR artifact_count >= 0),
  manifest_path   TEXT,
  error_detail    TEXT,
  created_at      TEXT NOT NULL,
  triggered_by    INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CHECK ((status = 'succeeded' AND db_sha256 IS NOT NULL AND manifest_sha256 IS NOT NULL AND manifest_path IS NOT NULL) OR status = 'failed')
) STRICT;
CREATE INDEX idx_backups_created ON backups (created_at);

CREATE TABLE schema_state (
  id             INTEGER PRIMARY KEY CHECK (id = 1),
  schema_version TEXT NOT NULL,
  schema_digest  TEXT NOT NULL CHECK (length(schema_digest) = 64),
  upgraded_at    TEXT NOT NULL
) STRICT;

CREATE TABLE migration_ledger (
  id           INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  migration_id TEXT NOT NULL UNIQUE,
  digest       TEXT NOT NULL CHECK (length(digest) = 64),
  applied_at   TEXT NOT NULL
) STRICT;

-- ============================================================================
-- 12. 触发器（机器可表达的不变量）
-- ============================================================================

-- 12.1 追加表：禁止 UPDATE / DELETE（持久历史不可改写）
CREATE TRIGGER trg_audit_events_no_update BEFORE UPDATE ON audit_events
BEGIN SELECT RAISE(ABORT, 'audit_events is append-only'); END;
CREATE TRIGGER trg_audit_events_no_delete BEFORE DELETE ON audit_events
BEGIN SELECT RAISE(ABORT, 'audit_events is append-only'); END;
CREATE TRIGGER trg_audit_event_targets_no_update BEFORE UPDATE ON audit_event_targets
BEGIN SELECT RAISE(ABORT, 'audit_event_targets is append-only'); END;
CREATE TRIGGER trg_audit_event_targets_no_delete BEFORE DELETE ON audit_event_targets
BEGIN SELECT RAISE(ABORT, 'audit_event_targets is append-only'); END;
CREATE TRIGGER trg_client_commands_no_update BEFORE UPDATE ON client_commands
BEGIN SELECT RAISE(ABORT, 'client_commands is append-only'); END;
CREATE TRIGGER trg_client_commands_no_delete BEFORE DELETE ON client_commands
BEGIN SELECT RAISE(ABORT, 'client_commands is append-only'); END;
CREATE TRIGGER trg_alert_deliveries_no_update BEFORE UPDATE ON alert_deliveries
BEGIN SELECT RAISE(ABORT, 'alert_deliveries is append-only'); END;
CREATE TRIGGER trg_alert_deliveries_no_delete BEFORE DELETE ON alert_deliveries
BEGIN SELECT RAISE(ABORT, 'alert_deliveries is append-only'); END;
CREATE TRIGGER trg_alert_delivery_items_no_update BEFORE UPDATE ON alert_delivery_items
BEGIN SELECT RAISE(ABORT, 'alert_delivery_items is append-only'); END;
CREATE TRIGGER trg_alert_delivery_items_no_delete BEFORE DELETE ON alert_delivery_items
BEGIN SELECT RAISE(ABORT, 'alert_delivery_items is append-only'); END;
CREATE TRIGGER trg_alert_observations_no_update BEFORE UPDATE ON alert_observations
BEGIN SELECT RAISE(ABORT, 'alert_observations is append-only'); END;
CREATE TRIGGER trg_alert_observations_no_delete BEFORE DELETE ON alert_observations
BEGIN SELECT RAISE(ABORT, 'alert_observations is append-only'); END;
CREATE TRIGGER trg_evidence_no_update BEFORE UPDATE ON evidence
BEGIN SELECT RAISE(ABORT, 'evidence is append-only'); END;
CREATE TRIGGER trg_evidence_no_delete BEFORE DELETE ON evidence
BEGIN SELECT RAISE(ABORT, 'evidence is append-only'); END;
CREATE TRIGGER trg_inspection_reports_no_update BEFORE UPDATE ON inspection_reports
BEGIN SELECT RAISE(ABORT, 'inspection_reports is append-only'); END;
CREATE TRIGGER trg_inspection_reports_no_delete BEFORE DELETE ON inspection_reports
BEGIN SELECT RAISE(ABORT, 'inspection_reports is append-only'); END;
CREATE TRIGGER trg_knowledge_versions_no_update BEFORE UPDATE ON knowledge_versions
BEGIN SELECT RAISE(ABORT, 'knowledge_versions is append-only'); END;
CREATE TRIGGER trg_knowledge_versions_no_delete BEFORE DELETE ON knowledge_versions
BEGIN SELECT RAISE(ABORT, 'knowledge_versions is append-only'); END;
CREATE TRIGGER trg_diagnosis_feedback_no_update BEFORE UPDATE ON diagnosis_feedback
BEGIN SELECT RAISE(ABORT, 'diagnosis_feedback is append-only'); END;
CREATE TRIGGER trg_diagnosis_feedback_no_delete BEFORE DELETE ON diagnosis_feedback
BEGIN SELECT RAISE(ABORT, 'diagnosis_feedback is append-only'); END;
CREATE TRIGGER trg_text_attachments_no_update BEFORE UPDATE ON text_attachments
BEGIN SELECT RAISE(ABORT, 'text_attachments is append-only'); END;
CREATE TRIGGER trg_text_attachments_no_delete BEFORE DELETE ON text_attachments
BEGIN SELECT RAISE(ABORT, 'text_attachments is append-only'); END;
CREATE TRIGGER trg_source_materials_no_update BEFORE UPDATE ON source_materials
BEGIN SELECT RAISE(ABORT, 'source_materials is append-only'); END;
CREATE TRIGGER trg_source_materials_no_delete BEFORE DELETE ON source_materials
BEGIN SELECT RAISE(ABORT, 'source_materials is append-only'); END;
CREATE TRIGGER trg_connection_revisions_no_update BEFORE UPDATE ON connection_revisions
BEGIN SELECT RAISE(ABORT, 'connection_revisions is append-only'); END;
CREATE TRIGGER trg_connection_revisions_no_delete BEFORE DELETE ON connection_revisions
BEGIN SELECT RAISE(ABORT, 'connection_revisions is append-only'); END;
CREATE TRIGGER trg_credential_generations_no_update BEFORE UPDATE ON credential_generations
BEGIN SELECT RAISE(ABORT, 'credential_generations is append-only'); END;
CREATE TRIGGER trg_credential_generations_no_delete BEFORE DELETE ON credential_generations
BEGIN SELECT RAISE(ABORT, 'credential_generations is append-only'); END;
CREATE TRIGGER trg_browser_profile_generations_no_update BEFORE UPDATE ON browser_profile_generations
BEGIN SELECT RAISE(ABORT, 'browser_profile_generations is append-only'); END;
CREATE TRIGGER trg_browser_profile_generations_no_delete BEFORE DELETE ON browser_profile_generations
BEGIN SELECT RAISE(ABORT, 'browser_profile_generations is append-only'); END;
CREATE TRIGGER trg_config_discoveries_no_update BEFORE UPDATE ON config_discoveries
BEGIN SELECT RAISE(ABORT, 'config_discoveries is append-only'); END;
CREATE TRIGGER trg_config_discoveries_no_delete BEFORE DELETE ON config_discoveries
BEGIN SELECT RAISE(ABORT, 'config_discoveries is append-only'); END;
CREATE TRIGGER trg_config_plans_no_update BEFORE UPDATE ON config_plans
BEGIN SELECT RAISE(ABORT, 'config_plans is append-only'); END;
CREATE TRIGGER trg_config_plans_no_delete BEFORE DELETE ON config_plans
BEGIN SELECT RAISE(ABORT, 'config_plans is append-only'); END;
CREATE TRIGGER trg_config_checks_no_update BEFORE UPDATE ON config_checks
BEGIN SELECT RAISE(ABORT, 'config_checks is append-only'); END;
CREATE TRIGGER trg_config_checks_no_delete BEFORE DELETE ON config_checks
BEGIN SELECT RAISE(ABORT, 'config_checks is append-only'); END;
CREATE TRIGGER trg_initial_analysis_outputs_no_update BEFORE UPDATE ON initial_analysis_outputs
BEGIN SELECT RAISE(ABORT, 'initial_analysis_outputs is append-only'); END;
CREATE TRIGGER trg_initial_analysis_outputs_no_delete BEFORE DELETE ON initial_analysis_outputs
BEGIN SELECT RAISE(ABORT, 'initial_analysis_outputs is append-only'); END;
CREATE TRIGGER trg_observed_refresh_log_no_update BEFORE UPDATE ON observed_refresh_log
BEGIN SELECT RAISE(ABORT, 'observed_refresh_log is append-only'); END;
CREATE TRIGGER trg_observed_refresh_log_no_delete BEFORE DELETE ON observed_refresh_log
BEGIN SELECT RAISE(ABORT, 'observed_refresh_log is append-only'); END;
CREATE TRIGGER trg_inspection_check_results_no_update BEFORE UPDATE ON inspection_check_results
BEGIN SELECT RAISE(ABORT, 'inspection_check_results is append-only'); END;
CREATE TRIGGER trg_inspection_check_results_no_delete BEFORE DELETE ON inspection_check_results
BEGIN SELECT RAISE(ABORT, 'inspection_check_results is append-only'); END;
CREATE TRIGGER trg_migration_ledger_no_update BEFORE UPDATE ON migration_ledger
BEGIN SELECT RAISE(ABORT, 'migration_ledger is append-only'); END;
CREATE TRIGGER trg_migration_ledger_no_delete BEFORE DELETE ON migration_ledger
BEGIN SELECT RAISE(ABORT, 'migration_ledger is append-only'); END;

-- 12.2 有界派生变更日志：禁止 UPDATE，允许 DELETE（保留窗口 GC；可丢弃可重建）
CREATE TRIGGER trg_alert_change_log_no_update BEFORE UPDATE ON alert_change_log
BEGIN SELECT RAISE(ABORT, 'alert_change_log is append-only (deletion allowed for retention GC)'); END;

-- 12.3 接入问题：只允许确认字段变化，历史不可删除
CREATE TRIGGER trg_alert_intake_issues_no_content_update BEFORE UPDATE OF
  source_id, delivery_id, delivery_item_id, kind, detail_json, created_at ON alert_intake_issues
BEGIN SELECT RAISE(ABORT, 'alert_intake_issues content is immutable'); END;
CREATE TRIGGER trg_alert_intake_issues_no_delete BEFORE DELETE ON alert_intake_issues
BEGIN SELECT RAISE(ABORT, 'alert_intake_issues history is not deletable'); END;

-- 12.4 调查消息：正文/角色/顺序不可变，只允许 active -> withdrawn
CREATE TRIGGER trg_investigation_messages_no_content_update BEFORE UPDATE OF
  investigation_id, seq, role, content, client_command_id, parent_message_id, created_at ON investigation_messages
BEGIN SELECT RAISE(ABORT, 'investigation_message content is immutable'); END;
CREATE TRIGGER trg_investigation_messages_no_delete BEFORE DELETE ON investigation_messages
BEGIN SELECT RAISE(ABORT, 'investigation_messages history is not deletable'); END;

-- 12.5 知识候选：原始建议与归属不可变；状态/草稿可更新；历史不可删除
CREATE TRIGGER trg_knowledge_candidates_no_origin_update BEFORE UPDATE OF
  import_batch_id, source_type, source_id, generation, original_suggestion_json, created_by, created_at ON knowledge_candidates
BEGIN SELECT RAISE(ABORT, 'knowledge_candidate origin is immutable'); END;
CREATE TRIGGER trg_knowledge_candidates_no_delete BEFORE DELETE ON knowledge_candidates
BEGIN SELECT RAISE(ABORT, 'knowledge_candidates history is not deletable'); END;

-- 12.6 导入批次：状态可更新，其余不可变，历史不可删除
CREATE TRIGGER trg_knowledge_import_batches_no_origin_update BEFORE UPDATE OF
  source_material_id, generation, created_by, created_at ON knowledge_import_batches
BEGIN SELECT RAISE(ABORT, 'knowledge_import_batch origin is immutable'); END;
CREATE TRIGGER trg_knowledge_import_batches_no_delete BEFORE DELETE ON knowledge_import_batches
BEGIN SELECT RAISE(ABORT, 'knowledge_import_batches history is not deletable'); END;

-- 12.7 配置版本：只允许 state/published 字段变化，正文不可变
CREATE TRIGGER trg_business_config_versions_no_content_update BEFORE UPDATE OF
  business_system_id, version_seq, yaml_body, parser_version, schema_version,
  label_contract_version_id, digest, base_version_id, created_by, created_at ON business_system_config_versions
BEGIN SELECT RAISE(ABORT, 'business_system_config_version content is immutable'); END;

-- 12.8 Label Contract：只允许 state/activation 变化，正文不可变
CREATE TRIGGER trg_label_contracts_no_content_update BEFORE UPDATE OF
  version, contract_json, digest, created_at ON label_contracts
BEGIN SELECT RAISE(ABORT, 'label_contract content is immutable'); END;

-- 12.9 连接：name/type 不可变
CREATE TRIGGER trg_connections_no_identity_update BEFORE UPDATE OF name, type, created_at ON connections
BEGIN SELECT RAISE(ABORT, 'connection identity is immutable'); END;

-- 12.10 浏览器身份：业务系统绑定不可变
CREATE TRIGGER trg_browser_identities_no_system_update BEFORE UPDATE OF business_system_id, created_at ON browser_identities
BEGIN SELECT RAISE(ABORT, 'browser_identity system binding is immutable'); END;

-- 12.11 观测资源：身份字段不可变（identity_key 是相等性权威）
CREATE TRIGGER trg_observed_resources_no_identity_update BEFORE UPDATE OF
  business_system_id, discovery_key, identity_key, identity_digest, created_at ON observed_resources
BEGIN SELECT RAISE(ABORT, 'observed_resource identity is immutable'); END;

-- 12.12 Artifact：物理 blob 身份不可改写；逻辑 Artifact 的来源字段不可改写，
-- 只允许保留字段（expires_at/body_expired）变化（DATA-ARTIFACT-003/005）
CREATE TRIGGER trg_artifact_blobs_no_update BEFORE UPDATE ON artifact_blobs
BEGIN SELECT RAISE(ABORT, 'artifact_blob content addressing is immutable'); END;
CREATE TRIGGER trg_artifacts_origin_immutable BEFORE UPDATE OF
  blob_id, kind, sensitive, retention_kind, owner_type, owner_id, created_by, created_at ON artifacts
BEGIN SELECT RAISE(ABORT, 'artifact logical origin is immutable'); END;

-- 12.13 浏览器操作：边界字段不可变，结果/结束时间可更新
CREATE TRIGGER trg_browser_operations_no_origin_update BEFORE UPDATE OF
  identity_id, kind, actor_type, actor_id, started_at ON browser_operations
BEGIN SELECT RAISE(ABORT, 'browser_operation origin is immutable'); END;

-- 12.14 模型/工具调用：归属与签名不可变，状态/结果可更新
CREATE TRIGGER trg_model_calls_no_origin_update BEFORE UPDATE OF
  attempt_id, call_seq, model_id, prompt_digest, tool_schema_digest, input_snapshot_digest, started_at ON model_calls
BEGIN SELECT RAISE(ABORT, 'model_call origin is immutable'); END;
CREATE TRIGGER trg_tool_calls_no_origin_update BEFORE UPDATE OF
  attempt_id, call_seq, tool_name, tool_version, arguments_json, created_at ON tool_calls
BEGIN SELECT RAISE(ABORT, 'tool_call origin is immutable'); END;

-- 12.15 用户：auth_revision 必须严格递增（账号变更裁决依据）
CREATE TRIGGER trg_users_auth_revision_monotonic BEFORE UPDATE OF auth_revision ON users
WHEN NEW.auth_revision <= OLD.auth_revision
BEGIN SELECT RAISE(ABORT, 'auth_revision must increase'); END;

-- 12.16 告警源凭据：最多两个 Active；Retired 不可复活；退休时记录时间
CREATE TRIGGER trg_alert_source_credentials_max2_insert BEFORE INSERT ON alert_source_credentials
WHEN NEW.state = 'Active' AND (SELECT COUNT(*) FROM alert_source_credentials
     WHERE source_id = NEW.source_id AND state = 'Active') >= 2
BEGIN SELECT RAISE(ABORT, 'at most two active credentials per source'); END;
CREATE TRIGGER trg_alert_source_credentials_max2_update BEFORE UPDATE OF state ON alert_source_credentials
WHEN NEW.state = 'Active' AND (SELECT COUNT(*) FROM alert_source_credentials
     WHERE source_id = NEW.source_id AND state = 'Active') >= 2
BEGIN SELECT RAISE(ABORT, 'at most two active credentials per source'); END;
-- 唯一合法 UPDATE 是 Active -> Retired；这直接表达完整前向状态机，而非依赖多个否定守卫。
CREATE TRIGGER trg_alert_source_credentials_update_is_retirement BEFORE UPDATE ON alert_source_credentials
WHEN NOT (OLD.state = 'Active' AND NEW.state = 'Retired')
BEGIN SELECT RAISE(ABORT, 'credential update must be Active -> Retired'); END;
-- 合法退休必须恰好递增 row_version（吊销命令在同一 UPDATE 中递增；DATA-ALERT-009）。
CREATE TRIGGER trg_alert_source_credentials_row_version_increment BEFORE UPDATE ON alert_source_credentials
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'credential row_version must increase exactly by 1'); END;
-- retired_at 由应用在同一 UPDATE 中写入（SQLite 触发器不支持 SET NEW）。

-- 12.17 告警发生：禁止已恢复再打开；row_version/last_state_change_at/resolved_at 由应用在
-- 同一条 UPDATE 中维护（SQLite 触发器不支持 SET NEW；DATA-ALERT-* 定义其语义）。
CREATE TRIGGER trg_alert_occurrence_no_reopen BEFORE UPDATE OF state ON alert_occurrences
WHEN NEW.state = 'Firing' AND OLD.state = 'Resolved'
BEGIN SELECT RAISE(ABORT, 'resolved occurrence cannot reopen'); END;

-- 12.18 告警变更日志：与影响列表的 Occurrence 变更同一事务派生（可丢弃、可重建）
CREATE TRIGGER trg_alert_change_log_insert AFTER INSERT ON alert_occurrences
BEGIN
  INSERT INTO alert_change_log (occurrence_id, change_type, row_version)
  VALUES (NEW.id, 'created', NEW.row_version);
END;
CREATE TRIGGER trg_alert_change_log_state AFTER UPDATE OF state ON alert_occurrences
WHEN NEW.state <> OLD.state
BEGIN
  INSERT INTO alert_change_log (occurrence_id, change_type, row_version)
  VALUES (NEW.id, 'state_changed', NEW.row_version);
END;

-- 12.19 row_version 由应用在每次 UPDATE 中显式递增（SQLite 触发器不支持 SET NEW；
-- 并发前提 expected_row_version 的比较与递增在同一 UPDATE 的 WHERE 与 SET 中完成）。

-- 12.20 知识检索退出是粘性的：永不自动复活（恢复复用必须创建并确认新版本）
CREATE TRIGGER trg_knowledge_retrieval_exit_sticky BEFORE UPDATE OF exited ON knowledge_version_retrieval_state
WHEN OLD.exited = 1 AND NEW.exited = 0
BEGIN SELECT RAISE(ABORT, 'retrieval exit is sticky; recovery requires a new confirmed version'); END;
-- exited_at / updated_at 由应用在同一 UPDATE 中写入。

-- 12.21 FTS5 external-content 同步：knowledge_search_docs 与 knowledge_fts 同一事务保持一致
CREATE TRIGGER trg_knowledge_fts_insert AFTER INSERT ON knowledge_search_docs
BEGIN
  INSERT INTO knowledge_fts (rowid, title, body) VALUES (NEW.knowledge_version_id, NEW.title, NEW.body);
END;
CREATE TRIGGER trg_knowledge_fts_delete AFTER DELETE ON knowledge_search_docs
BEGIN
  INSERT INTO knowledge_fts (knowledge_fts, rowid, title, body)
  VALUES ('delete', OLD.knowledge_version_id, OLD.title, OLD.body);
END;

-- 12.22 稳定身份 key / 登录名：不可改写
CREATE TRIGGER trg_users_username_immutable BEFORE UPDATE OF username ON users
BEGIN SELECT RAISE(ABORT, 'username is stable and cannot be rewritten'); END;
CREATE TRIGGER trg_business_systems_identity_immutable BEFORE UPDATE OF key, created_at ON business_systems
BEGIN SELECT RAISE(ABORT, 'business_system stable key is immutable'); END;
CREATE TRIGGER trg_alert_sources_identity_immutable BEFORE UPDATE OF source_key, protocol, created_at ON alert_sources
BEGIN SELECT RAISE(ABORT, 'alert_source stable key is immutable'); END;

-- 12.23 生命周期对象的归属/身份字段不可改写（状态/时间等可更新）
CREATE TRIGGER trg_alert_occurrences_identity_immutable BEFORE UPDATE OF
  source_id, fingerprint, starts_at, labels_canonical, labels_digest, first_seen_at ON alert_occurrences
BEGIN SELECT RAISE(ABORT, 'alert_occurrence identity/labels snapshot is immutable'); END;
CREATE TRIGGER trg_initial_analyses_origin_immutable BEFORE UPDATE OF
  occurrence_id, input_snapshot_digest, created_by, created_at ON initial_analyses
BEGIN SELECT RAISE(ABORT, 'initial_analysis origin/input snapshot is immutable'); END;
CREATE TRIGGER trg_inspection_runs_origin_immutable BEFORE UPDATE OF
  business_system_id, plan_key, config_version_id, label_contract_version_id,
  trigger_kind, scheduled_for, rerun_of_id, journey_catalog_digest, journey_catalog_version, created_at ON inspection_runs
BEGIN SELECT RAISE(ABORT, 'inspection_run binding is immutable'); END;
CREATE TRIGGER trg_execution_attempts_origin_immutable BEFORE UPDATE OF
  attempt_type, scope_type, scope_id, check_key, created_at ON execution_attempts
BEGIN SELECT RAISE(ABORT, 'execution_attempt origin is immutable'); END;
CREATE TRIGGER trg_alert_occurrence_labels_no_update BEFORE UPDATE ON alert_occurrence_labels
BEGIN SELECT RAISE(ABORT, 'alert_occurrence_labels are immutable'); END;
CREATE TRIGGER trg_alert_occurrence_labels_no_delete BEFORE DELETE ON alert_occurrence_labels
BEGIN SELECT RAISE(ABORT, 'alert_occurrence_labels are immutable'); END;
CREATE TRIGGER trg_observed_resource_identity_labels_no_update BEFORE UPDATE ON observed_resource_identity_labels
BEGIN SELECT RAISE(ABORT, 'observed_resource_identity_labels are immutable'); END;
CREATE TRIGGER trg_observed_resource_identity_labels_no_delete BEFORE DELETE ON observed_resource_identity_labels
BEGIN SELECT RAISE(ABORT, 'observed_resource_identity_labels are immutable'); END;

-- 12.23b 可变聚合行版本：任何 UPDATE 必须恰好递增 row_version（应用在同一条 UPDATE 中递增；
-- 陈旧 expected 值在 WHERE 中比较后命中 0 行，由应用映射 409；DATA-ROWVER-001）
CREATE TRIGGER trg_users_row_version_increment BEFORE UPDATE ON users
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'users row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_runtime_slots_row_version_increment BEFORE UPDATE ON runtime_slots
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'runtime_slots row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_alert_sources_row_version_increment BEFORE UPDATE ON alert_sources
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'alert_sources row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_label_contracts_row_version_increment BEFORE UPDATE ON label_contracts
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'label_contracts row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_business_systems_row_version_increment BEFORE UPDATE ON business_systems
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'business_systems row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_connections_row_version_increment BEFORE UPDATE ON connections
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'connections row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_browser_identities_row_version_increment BEFORE UPDATE ON browser_identities
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'browser_identities row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_knowledge_version_retrieval_state_row_version_increment BEFORE UPDATE ON knowledge_version_retrieval_state
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'knowledge_version_retrieval_state row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_reusable_knowledge_row_version_increment BEFORE UPDATE ON reusable_knowledge
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'reusable_knowledge row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_backup_settings_row_version_increment BEFORE UPDATE ON backup_settings
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'backup_settings row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_runtime_credentials_row_version_increment BEFORE UPDATE ON runtime_credentials
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'runtime_credentials row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_runtime_artifact_uploads_row_version_increment BEFORE UPDATE ON runtime_artifact_uploads
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'runtime_artifact_uploads row_version must increase exactly by 1'); END;

-- 12.23c 既有可观察/可取消对象：应用在同一条 UPDATE 中递增 row_version（SQLite 触发器无法 SET NEW），
-- 触发器强制恰好 +1（DATA-ROWVER-001 / DATA-SSE-005）。变更日志表（alert_change_log/task_change_log）
-- 的 row_version 列是事件载荷（记录事件时对象版本），日志本身不可变，不适用本规则。
CREATE TRIGGER trg_alert_occurrences_row_version_increment BEFORE UPDATE ON alert_occurrences
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'alert_occurrences row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_initial_analyses_row_version_increment BEFORE UPDATE ON initial_analyses
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'initial_analyses row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_browser_operations_row_version_increment BEFORE UPDATE ON browser_operations
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'browser_operations row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_inspection_runs_row_version_increment BEFORE UPDATE ON inspection_runs
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'inspection_runs row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_execution_attempts_row_version_increment BEFORE UPDATE ON execution_attempts
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'execution_attempts row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_tool_calls_row_version_increment BEFORE UPDATE ON tool_calls
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'tool_calls row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_knowledge_import_batches_row_version_increment BEFORE UPDATE ON knowledge_import_batches
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'knowledge_import_batches row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_knowledge_candidates_row_version_increment BEFORE UPDATE ON knowledge_candidates
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'knowledge_candidates row_version must increase exactly by 1'); END;

-- 12.23d 告警接入问题确认：单向粘性状态机（未确认 -> 已确认；不可取消确认、不可改派），
-- 确认与取消确认字段成对出现（CHECK），任何 UPDATE 恰好递增 row_version（DATA-ALERT-011）。
CREATE TRIGGER trg_alert_intake_issues_row_version_increment BEFORE UPDATE ON alert_intake_issues
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'alert_intake_issues row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_alert_intake_issues_ack_sticky BEFORE UPDATE OF acknowledged_at, acknowledged_by ON alert_intake_issues
WHEN OLD.acknowledged_at IS NOT NULL AND
     (NEW.acknowledged_at IS NULL OR NEW.acknowledged_at <> OLD.acknowledged_at OR NEW.acknowledged_by <> OLD.acknowledged_by)
BEGIN SELECT RAISE(ABORT, 'intake issue acknowledgement is sticky'); END;

-- 12.23e Label Contract 当前指针单行聚合：恰好 +1、不可删除、指针必须指向 active 契约（DATA-CONFIG-005/006）。
CREATE TRIGGER trg_label_contract_state_row_version_increment BEFORE UPDATE ON label_contract_state
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'label_contract_state row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_label_contract_state_no_delete BEFORE DELETE ON label_contract_state
BEGIN SELECT RAISE(ABORT, 'label_contract_state is a single-row table'); END;
CREATE TRIGGER trg_label_contract_state_pointer_insert AFTER INSERT ON label_contract_state
WHEN NEW.current_contract_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM label_contracts lc WHERE lc.id = NEW.current_contract_id AND lc.state = 'active')
BEGIN SELECT RAISE(ABORT, 'label_contract_state.current_contract_id must reference an active contract'); END;
CREATE TRIGGER trg_label_contract_state_pointer_update AFTER UPDATE OF current_contract_id ON label_contract_state
WHEN NEW.current_contract_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM label_contracts lc WHERE lc.id = NEW.current_contract_id AND lc.state = 'active')
BEGIN SELECT RAISE(ABORT, 'label_contract_state.current_contract_id must reference an active contract'); END;

-- 12.23f 变更流回放水位（DATA-SSE-009）：不建第二张水位表。日志最新行（MAX(id)）即
-- high-water，由 BEFORE DELETE 触发器强制保留（禁止删除最新行）；旧行仍可 GC 删除。
-- high_water = COALESCE(MAX(change_log.id),0)，oldest_available = COALESCE(MIN(change_log.id),0)，
-- 均直接取自日志本身；最新行保留保证 high_water 自首个事件后永不回退。
-- 客户端游标过期判定见 HTTP-SSE-009 两条件谓词（cursor < high_water AND cursor < oldest - 1）。
CREATE TRIGGER trg_alert_change_log_no_delete_latest BEFORE DELETE ON alert_change_log
WHEN OLD.id = (SELECT MAX(id) FROM alert_change_log)
BEGIN SELECT RAISE(ABORT, 'alert_change_log latest row is the replay high-water and cannot be deleted'); END;
CREATE TRIGGER trg_task_change_log_no_delete_latest BEFORE DELETE ON task_change_log
WHEN OLD.id = (SELECT MAX(id) FROM task_change_log)
BEGIN SELECT RAISE(ABORT, 'task_change_log latest row is the replay high-water and cannot be deleted'); END;

-- 12.24 持久历史禁止物理删除（tombstone-only；可清理的派生/会话表除外）
CREATE TRIGGER trg_users_no_delete BEFORE DELETE ON users
BEGIN SELECT RAISE(ABORT, 'users are tombstone-only (disable, never delete)'); END;
CREATE TRIGGER trg_alert_sources_no_delete BEFORE DELETE ON alert_sources
BEGIN SELECT RAISE(ABORT, 'alert_sources are tombstone-only'); END;
CREATE TRIGGER trg_alert_source_credentials_no_delete BEFORE DELETE ON alert_source_credentials
BEGIN SELECT RAISE(ABORT, 'alert_source_credentials history is not deletable'); END;
CREATE TRIGGER trg_alert_occurrences_no_delete BEFORE DELETE ON alert_occurrences
BEGIN SELECT RAISE(ABORT, 'alert_occurrences history is not deletable'); END;
CREATE TRIGGER trg_initial_analyses_no_delete BEFORE DELETE ON initial_analyses
BEGIN SELECT RAISE(ABORT, 'initial_analyses history is not deletable'); END;
CREATE TRIGGER trg_investigations_no_delete BEFORE DELETE ON investigations
BEGIN SELECT RAISE(ABORT, 'investigations history is not deletable'); END;
CREATE TRIGGER trg_label_contracts_no_delete BEFORE DELETE ON label_contracts
BEGIN SELECT RAISE(ABORT, 'label_contracts history is not deletable'); END;
CREATE TRIGGER trg_business_systems_no_delete BEFORE DELETE ON business_systems
BEGIN SELECT RAISE(ABORT, 'business_systems are tombstone-only'); END;
CREATE TRIGGER trg_business_system_config_versions_no_delete BEFORE DELETE ON business_system_config_versions
BEGIN SELECT RAISE(ABORT, 'business_system_config_versions history is not deletable'); END;
CREATE TRIGGER trg_observed_resources_no_delete BEFORE DELETE ON observed_resources
BEGIN SELECT RAISE(ABORT, 'observed_resources history is not deletable'); END;
CREATE TRIGGER trg_connections_no_delete BEFORE DELETE ON connections
BEGIN SELECT RAISE(ABORT, 'connections are tombstone-only'); END;
CREATE TRIGGER trg_browser_identities_no_delete BEFORE DELETE ON browser_identities
BEGIN SELECT RAISE(ABORT, 'browser_identities history is not deletable'); END;
CREATE TRIGGER trg_browser_operations_no_delete BEFORE DELETE ON browser_operations
BEGIN SELECT RAISE(ABORT, 'browser_operations history is not deletable'); END;
CREATE TRIGGER trg_artifacts_no_delete BEFORE DELETE ON artifacts
BEGIN SELECT RAISE(ABORT, 'artifact metadata is permanent; only physical blobs may be GC-cleaned'); END;
CREATE TRIGGER trg_inspection_runs_no_delete BEFORE DELETE ON inspection_runs
BEGIN SELECT RAISE(ABORT, 'inspection_runs history is not deletable'); END;
CREATE TRIGGER trg_execution_attempts_no_delete BEFORE DELETE ON execution_attempts
BEGIN SELECT RAISE(ABORT, 'execution_attempts history is not deletable'); END;
CREATE TRIGGER trg_model_calls_no_delete BEFORE DELETE ON model_calls
BEGIN SELECT RAISE(ABORT, 'model_calls trace is not deletable'); END;
CREATE TRIGGER trg_tool_calls_no_delete BEFORE DELETE ON tool_calls
BEGIN SELECT RAISE(ABORT, 'tool_calls trace is not deletable'); END;
CREATE TRIGGER trg_reusable_knowledge_no_delete BEFORE DELETE ON reusable_knowledge
BEGIN SELECT RAISE(ABORT, 'reusable_knowledge history is not deletable'); END;
CREATE TRIGGER trg_knowledge_retrieval_state_no_delete BEFORE DELETE ON knowledge_version_retrieval_state
BEGIN SELECT RAISE(ABORT, 'retrieval exit state is sticky and not deletable'); END;
CREATE TRIGGER trg_embedding_generations_no_delete BEFORE DELETE ON embedding_generations
BEGIN SELECT RAISE(ABORT, 'embedding_generations history is not deletable'); END;
CREATE TRIGGER trg_backups_no_delete BEFORE DELETE ON backups
BEGIN SELECT RAISE(ABORT, 'backups history is not deletable'); END;
CREATE TRIGGER trg_runtime_slots_no_delete BEFORE DELETE ON runtime_slots
BEGIN SELECT RAISE(ABORT, 'runtime_slots are fixed and not deletable'); END;
CREATE TRIGGER trg_schema_state_no_delete BEFORE DELETE ON schema_state
BEGIN SELECT RAISE(ABORT, 'schema_state is a single-row table'); END;
CREATE TRIGGER trg_backup_settings_no_delete BEFORE DELETE ON backup_settings
BEGIN SELECT RAISE(ABORT, 'backup_settings is a single-row table'); END;

-- 12.25 当前指针必须指向同一聚合的对象（归属校验）
CREATE TRIGGER trg_connections_revision_owner_insert AFTER INSERT ON connections
WHEN NEW.current_revision_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM connection_revisions r WHERE r.id = NEW.current_revision_id AND r.connection_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'current_revision_id must belong to the same connection'); END;
CREATE TRIGGER trg_connections_revision_owner_update AFTER UPDATE OF current_revision_id ON connections
WHEN NEW.current_revision_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM connection_revisions r WHERE r.id = NEW.current_revision_id AND r.connection_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'current_revision_id must belong to the same connection'); END;
CREATE TRIGGER trg_connections_credential_owner_insert AFTER INSERT ON connections
WHEN NEW.current_credential_generation_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM credential_generations g WHERE g.id = NEW.current_credential_generation_id AND g.connection_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'current_credential_generation_id must belong to the same connection'); END;
CREATE TRIGGER trg_connections_credential_owner_update AFTER UPDATE OF current_credential_generation_id ON connections
WHEN NEW.current_credential_generation_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM credential_generations g WHERE g.id = NEW.current_credential_generation_id AND g.connection_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'current_credential_generation_id must belong to the same connection'); END;
CREATE TRIGGER trg_business_systems_config_owner_insert AFTER INSERT ON business_systems
WHEN NEW.current_config_version_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM business_system_config_versions v WHERE v.id = NEW.current_config_version_id AND v.business_system_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'current_config_version_id must belong to the same business system'); END;
CREATE TRIGGER trg_business_systems_config_owner_update AFTER UPDATE OF current_config_version_id ON business_systems
WHEN NEW.current_config_version_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM business_system_config_versions v WHERE v.id = NEW.current_config_version_id AND v.business_system_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'current_config_version_id must belong to the same business system'); END;
CREATE TRIGGER trg_browser_identities_profile_owner_insert AFTER INSERT ON browser_identities
WHEN NEW.current_profile_generation_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM browser_profile_generations p WHERE p.id = NEW.current_profile_generation_id AND p.identity_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'current_profile_generation_id must belong to the same identity'); END;
CREATE TRIGGER trg_browser_identities_profile_owner_update AFTER UPDATE OF current_profile_generation_id ON browser_identities
WHEN NEW.current_profile_generation_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM browser_profile_generations p WHERE p.id = NEW.current_profile_generation_id AND p.identity_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'current_profile_generation_id must belong to the same identity'); END;
CREATE TRIGGER trg_reusable_knowledge_current_owner_insert AFTER INSERT ON reusable_knowledge
WHEN NEW.current_version_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM knowledge_versions v WHERE v.id = NEW.current_version_id AND v.knowledge_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'current_version_id must belong to the same knowledge'); END;
CREATE TRIGGER trg_reusable_knowledge_current_owner_update AFTER UPDATE OF current_version_id ON reusable_knowledge
WHEN NEW.current_version_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM knowledge_versions v WHERE v.id = NEW.current_version_id AND v.knowledge_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'current_version_id must belong to the same knowledge'); END;
CREATE TRIGGER trg_investigations_head_owner_insert AFTER INSERT ON investigations
WHEN NEW.current_head_message_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM investigation_messages m WHERE m.id = NEW.current_head_message_id AND m.investigation_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'current_head_message_id must belong to the same investigation'); END;
CREATE TRIGGER trg_investigations_head_owner_update AFTER UPDATE OF current_head_message_id ON investigations
WHEN NEW.current_head_message_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM investigation_messages m WHERE m.id = NEW.current_head_message_id AND m.investigation_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'current_head_message_id must belong to the same investigation'); END;

-- 12.26 Embedding 状态/向量/维度一致性
CREATE TRIGGER trg_embeddings_vector_dim_insert AFTER INSERT ON embeddings
WHEN NEW.vector IS NOT NULL AND EXISTS
  (SELECT 1 FROM embedding_generations g WHERE g.id = NEW.embedding_generation_id AND g.vector_dim IS NOT NULL AND length(NEW.vector) <> g.vector_dim * 4)
BEGIN SELECT RAISE(ABORT, 'embedding vector byte length must equal generation vector_dim * 4'); END;
CREATE TRIGGER trg_embeddings_vector_dim_update AFTER UPDATE OF vector ON embeddings
WHEN NEW.vector IS NOT NULL AND EXISTS
  (SELECT 1 FROM embedding_generations g WHERE g.id = NEW.embedding_generation_id AND g.vector_dim IS NOT NULL AND length(NEW.vector) <> g.vector_dim * 4)
BEGIN SELECT RAISE(ABORT, 'embedding vector byte length must equal generation vector_dim * 4'); END;

-- 12.27 不可变补充（第二轮对抗性审阅）：来源/绑定/历史记录不可改写
-- 撤回消息：只允许 active -> withdrawn，withdrawn 永不可复活（DATA-INVEST-004）
CREATE TRIGGER trg_investigation_messages_no_unwithdraw BEFORE UPDATE OF status ON investigation_messages
WHEN OLD.status = 'withdrawn' AND NEW.status <> 'withdrawn'
BEGIN SELECT RAISE(ABORT, 'withdrawn message cannot be reactivated'); END;
-- 告警源凭据：digest/source_id 等来源字段不可改写（轮换只创建新凭据）（DATA-ALERT-009）
CREATE TRIGGER trg_alert_source_credentials_origin_immutable BEFORE UPDATE OF
  source_id, digest, created_at ON alert_source_credentials
BEGIN SELECT RAISE(ABORT, 'alert_source_credential origin is immutable'); END;
-- 连接绑定的 revision/generation 一旦非空即不可改写（派发时绑定；DATA-CONN-002）
CREATE TRIGGER trg_execution_attempts_binding_immutable BEFORE UPDATE OF connection_revision_id ON execution_attempts
WHEN OLD.connection_revision_id IS NOT NULL AND NEW.connection_revision_id <> OLD.connection_revision_id
BEGIN SELECT RAISE(ABORT, 'attempt connection binding is immutable once set'); END;
CREATE TRIGGER trg_execution_attempts_credential_immutable BEFORE UPDATE OF credential_generation_id ON execution_attempts
WHEN OLD.credential_generation_id IS NOT NULL AND NEW.credential_generation_id <> OLD.credential_generation_id
BEGIN SELECT RAISE(ABORT, 'attempt credential binding is immutable once set'); END;
-- 备份记录是审计等价历史：禁止 UPDATE（DATA-BACKUP-007）
CREATE TRIGGER trg_backups_no_update BEFORE UPDATE ON backups
BEGIN SELECT RAISE(ABORT, 'backups is append-only'); END;
-- Runtime slot 是固定键（CONTEXT「服务身份」）
CREATE TRIGGER trg_runtime_slots_slot_immutable BEFORE UPDATE OF slot ON runtime_slots
BEGIN SELECT RAISE(ABORT, 'runtime_slot key is fixed'); END;
-- 浏览器操作：result 一旦产生即终态不可变，且必须带 ended_at（DATA-BROWSER-003）。
-- result 非空后，result 与 ended_at 都完全冻结：清空（result=NULL/ended_at=NULL）或改写均拒绝——
-- 用 IS NOT 做 NULL-safe 比较，避免 NULL 比较短路放行“复活”回 active（重新阻塞 lintel 替换 fence）。
CREATE TRIGGER trg_browser_operations_result_immutable BEFORE UPDATE OF result, ended_at ON browser_operations
WHEN OLD.result IS NOT NULL AND (NEW.result IS NOT OLD.result OR NEW.ended_at IS NOT OLD.ended_at)
BEGIN SELECT RAISE(ABORT, 'browser_operation result is final once set; result/ended_at cannot be cleared or rewritten'); END;
-- Embedding generation 来源字段不可变；vector_dim 一旦设置或该 generation 已有 embeddings 即不可变更
CREATE TRIGGER trg_embedding_generations_origin_immutable BEFORE UPDATE OF
  model_name, model_version, generation, created_at ON embedding_generations
BEGIN SELECT RAISE(ABORT, 'embedding_generation origin is immutable'); END;
CREATE TRIGGER trg_embedding_generations_vector_dim_immutable BEFORE UPDATE OF vector_dim ON embedding_generations
WHEN (OLD.vector_dim IS NOT NULL AND NEW.vector_dim <> OLD.vector_dim)
  OR (EXISTS (SELECT 1 FROM embeddings e WHERE e.embedding_generation_id = OLD.id) AND NEW.vector_dim IS NOT OLD.vector_dim)
BEGIN SELECT RAISE(ABORT, 'embedding_generation vector_dim is immutable once set or embeddings exist'); END;

-- 12.28 生命周期终态不可变：终态只可到达、不可离开（非终态间转换由应用按状态机推进）
CREATE TRIGGER trg_initial_analyses_terminal_immutable BEFORE UPDATE OF state ON initial_analyses
WHEN OLD.state IN ('Succeeded','Failed','Cancelled','Interrupted') AND NEW.state <> OLD.state
BEGIN SELECT RAISE(ABORT, 'initial_analysis terminal state is immutable'); END;
CREATE TRIGGER trg_inspection_runs_terminal_immutable BEFORE UPDATE OF state ON inspection_runs
WHEN OLD.state IN ('Completed','CompletedWithGaps','Failed','Cancelled','Interrupted','SkippedOverlap') AND NEW.state <> OLD.state
BEGIN SELECT RAISE(ABORT, 'inspection_run terminal state is immutable'); END;
CREATE TRIGGER trg_execution_attempts_terminal_immutable BEFORE UPDATE OF state ON execution_attempts
WHEN OLD.state IN ('Succeeded','Failed','Cancelled','Interrupted') AND NEW.state <> OLD.state
BEGIN SELECT RAISE(ABORT, 'execution_attempt terminal state is immutable'); END;
CREATE TRIGGER trg_model_calls_terminal_immutable BEFORE UPDATE OF status ON model_calls
WHEN OLD.status IN ('succeeded','failed','cancelled') AND NEW.status <> OLD.status
BEGIN SELECT RAISE(ABORT, 'model_call terminal status is immutable'); END;
CREATE TRIGGER trg_tool_calls_terminal_immutable BEFORE UPDATE OF status ON tool_calls
WHEN OLD.status IN ('succeeded','failed','cancelled') AND NEW.status <> OLD.status
BEGIN SELECT RAISE(ABORT, 'tool_call terminal status is immutable'); END;
CREATE TRIGGER trg_knowledge_import_batches_terminal_immutable BEFORE UPDATE OF state ON knowledge_import_batches
WHEN OLD.state IN ('Failed','Completed','Cancelled') AND NEW.state <> OLD.state
BEGIN SELECT RAISE(ABORT, 'knowledge_import_batch terminal state is immutable'); END;
CREATE TRIGGER trg_knowledge_candidates_terminal_immutable BEFORE UPDATE OF state ON knowledge_candidates
WHEN OLD.state IN ('Confirmed','Excluded','Superseded','SourceInvalid') AND NEW.state <> OLD.state
BEGIN SELECT RAISE(ABORT, 'knowledge_candidate terminal state is immutable'); END;
CREATE TRIGGER trg_label_contracts_retired_immutable BEFORE UPDATE OF state ON label_contracts
WHEN OLD.state = 'retired' AND NEW.state <> 'retired'
BEGIN SELECT RAISE(ABORT, 'label_contract retired is terminal'); END;
CREATE TRIGGER trg_business_config_versions_superseded_immutable BEFORE UPDATE OF state ON business_system_config_versions
WHEN OLD.state = 'superseded' AND NEW.state <> 'superseded'
BEGIN SELECT RAISE(ABORT, 'business_system_config_version superseded is terminal'); END;
CREATE TRIGGER trg_business_config_versions_no_unpublish BEFORE UPDATE OF state ON business_system_config_versions
WHEN OLD.state = 'published' AND NEW.state = 'draft'
BEGIN SELECT RAISE(ABORT, 'published config version cannot return to draft'); END;

-- 12.29 任务变更日志：与权威对象状态/阶段变化同一事务派生（可丢弃、可重建；
-- DELETE 允许保留窗口 GC，但最新行（MAX(id)，回放 high-water）不可删除
-- （trg_task_change_log_no_delete_latest，DATA-SSE-009）；UPDATE 禁止。
-- row_version 由应用在同一 UPDATE 中递增（SQLite 触发器不支持 SET NEW；DATA-SSE-005）。
CREATE TRIGGER trg_task_change_log_no_update BEFORE UPDATE ON task_change_log
BEGIN SELECT RAISE(ABORT, 'task_change_log is append-only (deletion allowed for retention GC)'); END;
CREATE TRIGGER trg_task_change_log_initial_analysis_insert AFTER INSERT ON initial_analyses
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('initial_analysis', NEW.id, 'created', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_initial_analysis_state AFTER UPDATE OF state ON initial_analyses
WHEN NEW.state <> OLD.state
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('initial_analysis', NEW.id, 'state_changed', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_attempt_insert AFTER INSERT ON execution_attempts
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('execution_attempt', NEW.id, 'created', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_attempt_state AFTER UPDATE OF state ON execution_attempts
WHEN NEW.state <> OLD.state
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('execution_attempt', NEW.id, 'state_changed', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_run_insert AFTER INSERT ON inspection_runs
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('inspection_run', NEW.id, 'created', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_run_state AFTER UPDATE OF state ON inspection_runs
WHEN NEW.state <> OLD.state
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('inspection_run', NEW.id, 'state_changed', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_report_insert AFTER INSERT ON inspection_reports
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('inspection_report', NEW.id, 'created', 1);
END;
CREATE TRIGGER trg_task_change_log_tool_insert AFTER INSERT ON tool_calls
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('tool_call', NEW.id, 'created', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_tool_status AFTER UPDATE OF status ON tool_calls
WHEN NEW.status <> OLD.status
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('tool_call', NEW.id, 'state_changed', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_batch_insert AFTER INSERT ON knowledge_import_batches
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('knowledge_import_batch', NEW.id, 'created', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_batch_state AFTER UPDATE OF state ON knowledge_import_batches
WHEN NEW.state <> OLD.state
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('knowledge_import_batch', NEW.id, 'state_changed', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_candidate_insert AFTER INSERT ON knowledge_candidates
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('knowledge_candidate', NEW.id, 'created', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_candidate_state AFTER UPDATE OF state ON knowledge_candidates
WHEN NEW.state <> OLD.state
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('knowledge_candidate', NEW.id, 'state_changed', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_browser_insert AFTER INSERT ON browser_operations
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('browser_operation', NEW.id, 'created', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_browser_result AFTER UPDATE OF result ON browser_operations
WHEN NEW.result IS NOT NULL AND (OLD.result IS NULL OR NEW.result <> OLD.result)
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('browser_operation', NEW.id, 'state_changed', NEW.row_version);
END;

-- 12.30 Runtime 注册/凭据状态机与 Artifact 上传 ledger（DATA-RUNTIME-001/002、DATA-ARTIFACT-006）
-- runtime_slots 持久状态转换：unregistered -> registered（注册成功）、registered -> revoked（替换）、
-- revoked -> registered（替换后注册）。在线连接/心跳是瞬时投影，不落库；current/pending 凭据指针
-- 由 runtime_slots 单行拥有（单一 current authority，DATA-RUNTIME-001）。
CREATE TRIGGER trg_runtime_slots_state_transition BEFORE UPDATE OF state ON runtime_slots
WHEN OLD.state <> NEW.state AND NOT (
  (OLD.state = 'unregistered' AND NEW.state IN ('registered','revoked'))
  OR (OLD.state = 'registered' AND NEW.state = 'revoked')
  OR (OLD.state = 'revoked' AND NEW.state = 'registered')
)
BEGIN SELECT RAISE(ABORT, 'runtime_slot state transition only unregistered->registered/revoked, registered->revoked, revoked->registered'); END;

-- current 指针必须属于同一 slot 且 confirmed_at 非空、retired_at 为空（当前可用长期 token）；
-- pending 指针必须属于同一 slot、retired_at 为空（可已确认或未确认）。registered ⇔ current 非空
-- （表 CHECK），unregistered/revoked ⇔ current/pending 皆空（表 CHECK）——每个语句/提交点
-- registered iff current 非空 iff 指向本 slot 已确认未退休凭据（DATA-RUNTIME-001/002）。
CREATE TRIGGER trg_runtime_slots_current_owner_insert AFTER INSERT ON runtime_slots
WHEN NEW.current_credential_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM runtime_credentials c WHERE c.id = NEW.current_credential_id AND c.slot = NEW.slot
     AND c.confirmed_at IS NOT NULL AND c.retired_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'runtime_slots.current_credential_id must reference a confirmed, unretired credential of the same slot'); END;
CREATE TRIGGER trg_runtime_slots_current_owner_update AFTER UPDATE OF current_credential_id ON runtime_slots
WHEN NEW.current_credential_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM runtime_credentials c WHERE c.id = NEW.current_credential_id AND c.slot = NEW.slot
     AND c.confirmed_at IS NOT NULL AND c.retired_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'runtime_slots.current_credential_id must reference a confirmed, unretired credential of the same slot'); END;
CREATE TRIGGER trg_runtime_slots_pending_owner_insert AFTER INSERT ON runtime_slots
WHEN NEW.pending_credential_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM runtime_credentials c WHERE c.id = NEW.pending_credential_id AND c.slot = NEW.slot AND c.retired_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'runtime_slots.pending_credential_id must reference an unretired credential of the same slot'); END;
CREATE TRIGGER trg_runtime_slots_pending_owner_update AFTER UPDATE OF pending_credential_id ON runtime_slots
WHEN NEW.pending_credential_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM runtime_credentials c WHERE c.id = NEW.pending_credential_id AND c.slot = NEW.slot AND c.retired_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'runtime_slots.pending_credential_id must reference an unretired credential of the same slot'); END;

-- registered slot 的 pending 指针禁止非 NULL -> 另一个非 NULL 直接换绑：必须先 pending->NULL
-- （AFTER 触发器 trg_runtime_slots_abort_retire_pending 自动 retire 原 pending），再 NULL->new
-- （DATA-RUNTIME-002）。合法提升（current = 原 pending 且 pending 清空）不受影响（NEW.pending IS NULL）。
CREATE TRIGGER trg_runtime_slots_pending_no_direct_swap BEFORE UPDATE OF pending_credential_id ON runtime_slots
WHEN OLD.pending_credential_id IS NOT NULL
  AND NEW.pending_credential_id IS NOT NULL
  AND NEW.pending_credential_id IS NOT OLD.pending_credential_id
BEGIN SELECT RAISE(ABORT, 'runtime slot pending can only be cleared before pointing to another credential (no direct swap)'); END;

-- B: registered slot 更换 current 只能是“提升 pending”（DATA-RUNTIME-002）：OLD.pending 非空、
-- NEW.current = OLD.pending、NEW.pending 清空，单条 UPDATE 原子完成。禁止绕 pending 直切、
-- 禁止先清 pending 后另行切换 current。初次注册（unregistered->registered）与替换后重新注册
-- （revoked->registered）不受此限——注册窗口由 trg_runtime_credentials_insert_confirmed_registration_only 限定。
-- current_owner 与 promote 触发器覆盖不同的非法类别且不依赖同类触发器的执行次序：
-- 目标非法（未确认/跨 slot/已退休）最终由 current_owner 拒绝；目标合法但绕 pending 的直切
-- 最终由本触发器拒绝。任一 RAISE(ABORT) 都回滚整条 UPDATE 及此前触发器产生的退休副作用；
-- 只有全部验证通过的合法提升才会保留 promotion_retire_old 的更新。
CREATE TRIGGER trg_runtime_slots_promote_requires_pending AFTER UPDATE OF current_credential_id ON runtime_slots
WHEN OLD.state = 'registered' AND NEW.state = 'registered'
  AND NEW.current_credential_id IS NOT OLD.current_credential_id
  AND EXISTS (
    SELECT 1 FROM runtime_credentials c WHERE c.id = NEW.current_credential_id AND c.slot = NEW.slot
      AND c.confirmed_at IS NOT NULL AND c.retired_at IS NULL
  )
  AND (
    OLD.pending_credential_id IS NULL
    OR NEW.current_credential_id IS NOT OLD.pending_credential_id
    OR NEW.pending_credential_id IS NOT NULL
  )
BEGIN SELECT RAISE(ABORT, 'runtime slot current can only change by atomically promoting the pending credential (current=pending, pending=NULL)'); END;

-- C: retirement 是指针转移的机械副作用（AFTER 触发器），应用不得事后手动 retire 被引用行
-- （trg_runtime_credentials_no_retire_while_referenced 兜底）。提升：旧 current 自动退休；
-- 中止：被清空的 pending 自动退休；替换：该 slot 全部未退休凭据自动退休。时间使用与全库一致
-- 的 UTC 表达（strftime，同 committed_at DEFAULT）。自动 retire 恰好递增 row_version
-- （trg_runtime_credentials_row_version_increment），且不会命中 no_retire_while_referenced——
-- 触发器执行时该行已不再被 current/pending 引用。
CREATE TRIGGER trg_runtime_slots_promotion_retire_old AFTER UPDATE OF current_credential_id ON runtime_slots
WHEN OLD.state = 'registered' AND NEW.state = 'registered'
  AND OLD.current_credential_id IS NOT NULL
  AND NEW.current_credential_id IS NOT OLD.current_credential_id
BEGIN
  UPDATE runtime_credentials SET retired_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'), row_version = row_version + 1
  WHERE id = OLD.current_credential_id AND retired_at IS NULL;
END;
CREATE TRIGGER trg_runtime_slots_abort_retire_pending AFTER UPDATE OF pending_credential_id ON runtime_slots
WHEN OLD.pending_credential_id IS NOT NULL AND NEW.pending_credential_id IS NULL
  AND NEW.current_credential_id IS OLD.current_credential_id
BEGIN
  UPDATE runtime_credentials SET retired_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'), row_version = row_version + 1
  WHERE id = OLD.pending_credential_id AND retired_at IS NULL;
END;
CREATE TRIGGER trg_runtime_slots_replace_retire_all AFTER UPDATE OF state ON runtime_slots
WHEN NEW.state = 'revoked' AND OLD.state <> 'revoked'
BEGIN
  UPDATE runtime_credentials SET retired_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'), row_version = row_version + 1
  WHERE slot = NEW.slot AND retired_at IS NULL;
END;

-- runtime_credentials：来源字段（slot/generation/token_digest/created_at）不可改写；历史不可删除；
-- confirmed_at 与 retired_at 均为一次性生命周期事实（NULL -> 时间戳），不可回退、不可改写
-- （DATA-RUNTIME-002）。被 current 或 pending 指针引用的 credential 禁止设置 retired_at——
-- 吊销只能经指针转移的机械副作用发生（promotion/abort/replace 的 AFTER 触发器）；
-- 每个语句/提交点指针有效。
CREATE TRIGGER trg_runtime_credentials_origin_immutable BEFORE UPDATE OF
  slot, generation, token_digest, created_at ON runtime_credentials
BEGIN SELECT RAISE(ABORT, 'runtime_credential origin is immutable'); END;
CREATE TRIGGER trg_runtime_credentials_no_delete BEFORE DELETE ON runtime_credentials
BEGIN SELECT RAISE(ABORT, 'runtime_credentials history is not deletable'); END;
CREATE TRIGGER trg_runtime_credentials_confirmed_once BEFORE UPDATE OF confirmed_at ON runtime_credentials
WHEN OLD.confirmed_at IS NOT NULL AND NEW.confirmed_at IS NOT OLD.confirmed_at
BEGIN SELECT RAISE(ABORT, 'runtime_credential confirmed_at is a one-time fact and cannot be rewritten'); END;
CREATE TRIGGER trg_runtime_credentials_retired_once BEFORE UPDATE OF retired_at ON runtime_credentials
WHEN OLD.retired_at IS NOT NULL AND NEW.retired_at IS NOT OLD.retired_at
BEGIN SELECT RAISE(ABORT, 'runtime_credential retired_at is a one-time fact and cannot be rewritten'); END;
CREATE TRIGGER trg_runtime_credentials_no_retire_while_referenced BEFORE UPDATE OF retired_at ON runtime_credentials
WHEN NEW.retired_at IS NOT NULL AND OLD.retired_at IS NULL AND EXISTS
  (SELECT 1 FROM runtime_slots s WHERE s.slot = OLD.slot AND (s.current_credential_id = OLD.id OR s.pending_credential_id = OLD.id))
BEGIN SELECT RAISE(ABORT, 'runtime credential cannot be retired while referenced by current or pending pointer'); END;
-- 已退休凭据不可再确认（retire 是终态：中止的 pending 或已吊销的历史行不得复活为可用凭据）；
-- 同一条 UPDATE 同时写 confirmed_at 与 retired_at 同样拒绝。确认与吊销互斥且各一次。
CREATE TRIGGER trg_runtime_credentials_no_confirm_after_retire BEFORE UPDATE OF confirmed_at ON runtime_credentials
WHEN NEW.confirmed_at IS NOT NULL AND OLD.confirmed_at IS NULL AND (OLD.retired_at IS NOT NULL OR NEW.retired_at IS NOT NULL)
BEGIN SELECT RAISE(ABORT, 'runtime credential cannot be confirmed after retirement'); END;
-- 凭据不得“出生即退休”：INSERT 时 retired_at 必须为 NULL（生命周期只允许从 NULL -> 时间戳的
-- UPDATE 推进，DATA-RUNTIME-002）。confirmed_at 可非 NULL（初次注册）也可 NULL（轮换 pending）。
CREATE TRIGGER trg_runtime_credentials_insert_not_retired BEFORE INSERT ON runtime_credentials
WHEN NEW.retired_at IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'runtime credential cannot be created already retired'); END;
-- A: 确认（confirmed_at NULL -> 时间戳）的唯一合法路径是“该 credential 正被同 slot 的
-- pending_credential_id 引用”（轮换确认）。初次注册/替换后注册允许 INSERT 时 confirmed_at 非空，
-- 但仅当 slot 当前为 unregistered/revoked 且 current/pending 皆空（注册窗口）；
-- registered slot 不得 INSERT 已确认孤儿（轮换必须从 pending 确认，不能绕 pending）。
CREATE TRIGGER trg_runtime_credentials_confirm_requires_pending BEFORE UPDATE OF confirmed_at ON runtime_credentials
WHEN NEW.confirmed_at IS NOT NULL AND OLD.confirmed_at IS NULL AND NOT EXISTS (
  SELECT 1 FROM runtime_slots s WHERE s.slot = NEW.slot AND s.pending_credential_id = NEW.id
)
BEGIN SELECT RAISE(ABORT, 'runtime credential cannot be confirmed unless referenced by the slot pending pointer'); END;
CREATE TRIGGER trg_runtime_credentials_insert_confirmed_registration_only BEFORE INSERT ON runtime_credentials
WHEN NEW.confirmed_at IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM runtime_slots s WHERE s.slot = NEW.slot
    AND s.state IN ('unregistered','revoked')
    AND s.current_credential_id IS NULL AND s.pending_credential_id IS NULL
)
BEGIN SELECT RAISE(ABORT, 'confirmed runtime credential can only be inserted for first or replacement registration (slot unregistered/revoked with no pointers)'); END;

-- runtime_artifact_uploads：来源字段不可改写（含 boot_id）；只能以 uploading 创建，状态转换仅
-- uploading->committed/rejected 且终态不可变；committed 必须满足 NULL-safe 正向条件：所引 Attempt
-- 必须 state='Running' 且 runtime_slot/boot_id/connection_epoch 全部非空且与 upload 一致
-- （旧 Attempt 已终态/未派发/替换后 ABA/epoch 不符一律拒绝 commit，只能 rejected）；
-- artifact_id/committed_at 一旦提交不可改；历史不可删除（DATA-ARTIFACT-006）。
CREATE TRIGGER trg_runtime_artifact_uploads_origin_immutable BEFORE UPDATE OF
  upload_id, attempt_id, boot_id, connection_epoch, owner_type, owner_id, kind, retention_kind, sensitive, size_bytes, sha256, created_at ON runtime_artifact_uploads
BEGIN SELECT RAISE(ABORT, 'runtime_artifact_upload origin is immutable'); END;
CREATE TRIGGER trg_runtime_artifact_uploads_insert_state BEFORE INSERT ON runtime_artifact_uploads
WHEN NEW.state <> 'uploading'
BEGIN SELECT RAISE(ABORT, 'runtime_artifact_upload must be created as uploading'); END;
CREATE TRIGGER trg_runtime_artifact_uploads_state_transition BEFORE UPDATE OF state ON runtime_artifact_uploads
WHEN OLD.state <> NEW.state AND NOT (OLD.state = 'uploading' AND NEW.state IN ('committed','rejected'))
BEGIN SELECT RAISE(ABORT, 'runtime_artifact_upload state transition only uploading->committed/rejected'); END;
CREATE TRIGGER trg_runtime_artifact_uploads_commit_attempt BEFORE UPDATE OF state ON runtime_artifact_uploads
WHEN NEW.state = 'committed' AND NEW.artifact_id IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM execution_attempts a
  JOIN artifacts ar ON ar.id = NEW.artifact_id
  JOIN artifact_blobs b ON b.id = ar.blob_id
  WHERE a.id = NEW.attempt_id
    AND a.state = 'Running'
    AND a.runtime_slot IS NOT NULL
    AND a.boot_id IS NOT NULL AND a.boot_id = NEW.boot_id
    AND a.connection_epoch IS NOT NULL AND a.connection_epoch = NEW.connection_epoch
    AND ar.kind = NEW.kind
    AND ar.retention_kind = NEW.retention_kind
    AND ar.sensitive = NEW.sensitive
    AND ar.owner_type = NEW.owner_type
    AND ar.owner_id = NEW.owner_id
    AND b.sha256 = NEW.sha256
    AND b.size_bytes = NEW.size_bytes
)
BEGIN SELECT RAISE(ABORT, 'runtime_artifact_upload commit requires the attempt Running with matching non-null boot_id/connection_epoch and an artifact exactly matching kind/retention_kind/sensitive/owner_type/owner_id/sha256/size_bytes'); END;
CREATE TRIGGER trg_runtime_artifact_uploads_result_immutable BEFORE UPDATE OF artifact_id, committed_at ON runtime_artifact_uploads
WHEN OLD.artifact_id IS NOT NULL AND (NEW.artifact_id IS NOT OLD.artifact_id OR NEW.committed_at IS NOT OLD.committed_at)
BEGIN SELECT RAISE(ABORT, 'runtime_artifact_upload result is immutable once committed'); END;
CREATE TRIGGER trg_runtime_artifact_uploads_no_delete BEFORE DELETE ON runtime_artifact_uploads
BEGIN SELECT RAISE(ABORT, 'runtime_artifact_uploads ledger is not deletable'); END;

-- 替换 fence（Q237「有 active task 时必须先等待完成或明确取消才能替换」）：
-- Execution Attempt 可由 Plinth 或 Lintel 承载（browser_exploration 与巡检 Journey 的子 Attempt
-- 绑定 Lintel，CONTEXT「执行尝试」）；Browser Operation 全部由 Lintel 承载。任意 slot 置 revoked 前
-- 必须没有绑定该 slot 的 active Attempt（Assigned/Running/Cancelling）；此外 lintel 还必须没有
-- active Browser Operation（browser_operations.result IS NULL，直接检查，不通过 Attempt 推断——
-- manual_login、Queued/未派发 Attempt 或已终态 Attempt 上仍活跃的浏览器操作同样拦截）。
-- 应用事务先检查并拒绝（409 active_conflict），本触发器是机械兑底；正常替换流程中不存在 active。
CREATE TRIGGER trg_runtime_slots_no_replace_with_active BEFORE UPDATE OF state ON runtime_slots
WHEN NEW.state = 'revoked' AND (
  EXISTS (SELECT 1 FROM execution_attempts a WHERE a.runtime_slot = NEW.slot AND a.state IN ('Assigned','Running','Cancelling'))
  OR (NEW.slot = 'lintel' AND EXISTS (SELECT 1 FROM browser_operations bo WHERE bo.result IS NULL))
)
BEGIN SELECT RAISE(ABORT, 'runtime slot cannot be replaced while active attempts on the slot or active browser operations (lintel) exist'); END;

-- 派发 fence：Attempt 只能派发到 state='registered' 且 current 指针指向本 slot 已确认未退休凭据的 slot
-- （DATA-ATTEMPT-001/DATA-RUNTIME-001）。与 replace 事务按 SQLite 提交顺序裁决：replace 先提交则
-- 本触发器拒绝后续派发；派发先提交则 trg_runtime_slots_no_replace_with_active 拒绝替换。
-- INSERT（直接带绑定）与 UPDATE（Queued->Assigned 设置绑定）两条路径都覆盖。
CREATE TRIGGER trg_execution_attempts_slot_registered BEFORE INSERT ON execution_attempts
WHEN NEW.runtime_slot IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM runtime_slots s JOIN runtime_credentials c ON c.id = s.current_credential_id
  WHERE s.slot = NEW.runtime_slot AND s.state = 'registered' AND c.slot = s.slot
    AND c.confirmed_at IS NOT NULL AND c.retired_at IS NULL
)
BEGIN SELECT RAISE(ABORT, 'attempt can only be dispatched to a registered slot with a confirmed current credential'); END;
CREATE TRIGGER trg_execution_attempts_slot_registered_update BEFORE UPDATE OF runtime_slot ON execution_attempts
WHEN NEW.runtime_slot IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM runtime_slots s JOIN runtime_credentials c ON c.id = s.current_credential_id
  WHERE s.slot = NEW.runtime_slot AND s.state = 'registered' AND c.slot = s.slot
    AND c.confirmed_at IS NOT NULL AND c.retired_at IS NULL
)
BEGIN SELECT RAISE(ABORT, 'attempt can only be dispatched to a registered slot with a confirmed current credential'); END;

-- 派发绑定（runtime_slot/boot_id/connection_epoch/accepted_at）一旦设置不可改；
-- lease_until 可由心跳续期（可再生，row_version 照常递增）；requested_by_tool_call_id 不可改。
CREATE TRIGGER trg_execution_attempts_runtime_binding_immutable BEFORE UPDATE OF runtime_slot, boot_id, connection_epoch, accepted_at ON execution_attempts
WHEN (OLD.runtime_slot IS NOT NULL AND NEW.runtime_slot IS NOT OLD.runtime_slot)
  OR (OLD.boot_id IS NOT NULL AND NEW.boot_id IS NOT OLD.boot_id)
  OR (OLD.connection_epoch IS NOT NULL AND NEW.connection_epoch IS NOT OLD.connection_epoch)
  OR (OLD.accepted_at IS NOT NULL AND NEW.accepted_at IS NOT OLD.accepted_at)
BEGIN SELECT RAISE(ABORT, 'execution_attempt runtime binding is immutable once set'); END;
-- 跨 Runtime 子执行请求方闭合（DATA-ATTEMPT-007）：requested_by_tool_call_id 非空时必须指向
-- 已派发到 plinth 的父 Attempt 的 tool_call（INSERT 时父 runtime_slot 已设置且不可再改，
-- 因此 INSERT 检查足够，无后期漂移）；创建后完全不可改——NULL->非NULL（晚绑定）、
-- 非NULL->NULL/另一值均拒绝，仅同值/同 NULL 的 no-op 允许（IS 比较）。不建立通用任务 DAG。
CREATE TRIGGER trg_execution_attempts_requestor_plinth BEFORE INSERT ON execution_attempts
WHEN NEW.requested_by_tool_call_id IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM tool_calls tc
  JOIN execution_attempts parent ON parent.id = tc.attempt_id
  WHERE tc.id = NEW.requested_by_tool_call_id
    AND parent.runtime_slot = 'plinth'
)
BEGIN SELECT RAISE(ABORT, 'cross-runtime sub-execution requestor must be a tool call of a plinth-dispatched parent attempt'); END;
CREATE TRIGGER trg_execution_attempts_requestor_immutable BEFORE UPDATE OF requested_by_tool_call_id ON execution_attempts
WHEN NOT (OLD.requested_by_tool_call_id IS NEW.requested_by_tool_call_id)
BEGIN SELECT RAISE(ABORT, 'execution_attempt requestor tool call is immutable once set'); END;

-- browser_operations 与执行尝试的严格绑定（DATA-BROWSER-003）：manual_login 无 attempt；
-- exploration -> browser_exploration（scope investigation，非 run_check 子 Attempt）；
-- journey -> inspection_collection（scope run_check 子 Attempt，check_key 非空）；
-- attempt_id 一旦设置不可改；kind 已由 trg_browser_operations_no_origin_update 冻结。
CREATE TRIGGER trg_browser_operations_attempt_binding_insert BEFORE INSERT ON browser_operations
WHEN NOT (
  (NEW.kind = 'manual_login' AND NEW.attempt_id IS NULL)
  OR (NEW.kind = 'exploration' AND NEW.attempt_id IS NOT NULL AND EXISTS (
        SELECT 1 FROM execution_attempts a
        WHERE a.id = NEW.attempt_id AND a.attempt_type = 'browser_exploration'
          AND a.scope_type = 'investigation' AND a.check_key IS NULL))
  OR (NEW.kind = 'journey' AND NEW.attempt_id IS NOT NULL AND EXISTS (
        SELECT 1 FROM execution_attempts a
        WHERE a.id = NEW.attempt_id AND a.attempt_type = 'inspection_collection'
          AND a.scope_type = 'run_check' AND a.check_key IS NOT NULL))
)
BEGIN SELECT RAISE(ABORT, 'browser_operation kind requires a matching execution_attempt binding'); END;
CREATE TRIGGER trg_browser_operations_attempt_binding_update BEFORE UPDATE OF attempt_id ON browser_operations
WHEN OLD.attempt_id IS NULL AND NEW.attempt_id IS NOT NULL AND NOT (
  (NEW.kind = 'exploration' AND EXISTS (
     SELECT 1 FROM execution_attempts a
     WHERE a.id = NEW.attempt_id AND a.attempt_type = 'browser_exploration'
       AND a.scope_type = 'investigation' AND a.check_key IS NULL))
  OR (NEW.kind = 'journey' AND EXISTS (
     SELECT 1 FROM execution_attempts a
     WHERE a.id = NEW.attempt_id AND a.attempt_type = 'inspection_collection'
       AND a.scope_type = 'run_check' AND a.check_key IS NOT NULL))
)
BEGIN SELECT RAISE(ABORT, 'browser_operation attempt binding must match kind'); END;
CREATE TRIGGER trg_browser_operations_attempt_immutable BEFORE UPDATE OF attempt_id ON browser_operations
WHEN OLD.attempt_id IS NOT NULL AND NEW.attempt_id IS NOT OLD.attempt_id
BEGIN SELECT RAISE(ABORT, 'browser_operation attempt binding is immutable once set'); END;
