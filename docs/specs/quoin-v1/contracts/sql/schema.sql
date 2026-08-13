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
  id            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  username      TEXT NOT NULL UNIQUE,                       -- 稳定登录名，只禁用不删除
  display_name  TEXT NOT NULL,
  role          TEXT NOT NULL CHECK (role IN ('admin','operator')),
  enabled       INTEGER NOT NULL CHECK (enabled IN (0,1)),
  auth_revision INTEGER NOT NULL DEFAULT 1 CHECK (auth_revision > 0),
  password_phc  TEXT NOT NULL CHECK (length(password_phc) > 0), -- Argon2id PHC，格式见 security.md
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
) STRICT;

CREATE TABLE sessions (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  user_id              INTEGER NOT NULL REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  session_token        BLOB NOT NULL UNIQUE CHECK (length(session_token) = 32), -- __Host-quoin-session 值（仅存服务端）
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

CREATE TABLE runtime_slots (
  slot                 TEXT PRIMARY KEY CHECK (slot IN ('plinth','lintel')),
  state                TEXT NOT NULL DEFAULT 'unregistered' CHECK (state IN ('unregistered','registered','revoked')),
  token_digest         BLOB CHECK (token_digest IS NULL OR length(token_digest) = 32), -- 长期 token 只存 digest
  credential_generation INTEGER NOT NULL DEFAULT 0 CHECK (credential_generation >= 0),
  last_seen_at         TEXT,
  created_at           TEXT NOT NULL
) STRICT;

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
  created_at  TEXT NOT NULL,
  disabled_at TEXT,
  CHECK (enabled = 1 OR disabled_at IS NOT NULL)
) STRICT;

CREATE TABLE alert_source_credentials (
  id         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  source_id  INTEGER NOT NULL REFERENCES alert_sources(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  digest     BLOB NOT NULL CHECK (length(digest) = 32), -- 32-byte Bearer 只存 digest
  state      TEXT NOT NULL CHECK (state IN ('Active','Retired')),
  created_at TEXT NOT NULL,
  retired_at TEXT,
  CHECK ((state = 'Retired' AND retired_at IS NOT NULL) OR (state = 'Active' AND retired_at IS NULL))
) STRICT;
CREATE INDEX idx_alert_source_credentials_source ON alert_source_credentials (source_id);

CREATE TABLE alert_deliveries (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  relay_id                   TEXT NOT NULL UNIQUE,   -- Stele relay id，重试幂等键
  source_id                  INTEGER NOT NULL REFERENCES alert_sources(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  credential_id              INTEGER REFERENCES alert_source_credentials(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  credential_snapshot_version INTEGER,               -- Stele 提交的只读快照版本
  protocol                   TEXT NOT NULL CHECK (protocol IN ('alertmanager')),
  body                       BLOB NOT NULL,          -- 精确原始 body 字节（可能非 UTF-8），直接存 SQLite（非 Artifact）
  body_size_bytes            INTEGER NOT NULL CHECK (body_size_bytes >= 0),
  integrity                  TEXT NOT NULL CHECK (integrity IN ('complete','truncated','rejected')),
  status                     TEXT NOT NULL CHECK (status IN ('processed','rejected')),
  group_key                  TEXT,
  received_at                TEXT NOT NULL,          -- Stele 接收时间
  committed_at               TEXT NOT NULL,           -- Quoin 提交时间（提交顺序裁决依据）
  CHECK (length(body) = body_size_bytes)              -- BLOB 长度以字节计（SQLite length() 对 BLOB 返回字节数）
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
  created_at        TEXT NOT NULL,
  CHECK (kind <> 'delivery_truncated' OR delivery_id IS NOT NULL),
  CHECK (kind = 'delivery_truncated' OR delivery_item_id IS NOT NULL)
) STRICT;
CREATE UNIQUE INDEX ux_alert_intake_issue_truncated ON alert_intake_issues (delivery_id) WHERE kind = 'delivery_truncated';
CREATE INDEX idx_alert_intake_issues_source ON alert_intake_issues (source_id, created_at);

-- 有界派生变更日志：id 即单调递增 change_seq（AUTOINCREMENT，同事务分配、永不复用）；
-- 可清理（保留窗口由部署配置），不是告警历史权威源。
CREATE TABLE alert_change_log (
  id            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  occurrence_id INTEGER NOT NULL REFERENCES alert_occurrences(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  change_type   TEXT NOT NULL CHECK (change_type IN ('created','state_changed')),
  row_version   INTEGER NOT NULL CHECK (row_version >= 1),
  committed_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
) STRICT;
CREATE INDEX idx_alert_change_log_occurrence ON alert_change_log (occurrence_id);

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
  created_at    TEXT NOT NULL,
  activated_at  TEXT,
  activated_by  INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  UNIQUE (version)
) STRICT;
CREATE UNIQUE INDEX ux_label_contract_active ON label_contracts (state) WHERE state = 'active';

CREATE TABLE business_systems (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  key                       TEXT NOT NULL UNIQUE,  -- 稳定用户 key，退役不复用
  display_name              TEXT NOT NULL,
  enabled                   INTEGER NOT NULL CHECK (enabled IN (0,1)),
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
  meta_json      TEXT NOT NULL CHECK (json_valid(meta_json)), -- 非秘密元数据
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
  kind              TEXT NOT NULL CHECK (kind IN ('manual_login','exploration','journey')),
  actor_type        TEXT NOT NULL CHECK (actor_type IN ('user','service','system')),
  actor_id          INTEGER NOT NULL,
  started_at        TEXT NOT NULL,
  ended_at          TEXT,
  result            TEXT CHECK (result IS NULL OR result IN ('success','failed','cancelled','interrupted')),
  new_generation_id INTEGER REFERENCES browser_profile_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  trace_artifact_id INTEGER REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  log_json          TEXT CHECK (log_json IS NULL OR json_valid(log_json)),
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
  created_at               TEXT NOT NULL
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

CREATE TABLE artifacts (
  id             INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  sha256         TEXT NOT NULL UNIQUE CHECK (length(sha256) = 64),
  size_bytes     INTEGER NOT NULL CHECK (size_bytes >= 0),
  storage_key    TEXT NOT NULL UNIQUE,              -- 仅由 hash 推导的路径
  kind           TEXT NOT NULL CHECK (kind IN ('attachment','screenshot','trace','tool_result','report_file')),
  sensitive      INTEGER NOT NULL DEFAULT 0 CHECK (sensitive IN (0,1)), -- raw trace 固定 sensitive=1
  retention_kind TEXT NOT NULL CHECK (retention_kind IN ('long_term','generated')),
  owner_type     TEXT NOT NULL,
  owner_id       INTEGER NOT NULL,
  expires_at     TEXT,
  body_expired   INTEGER NOT NULL DEFAULT 0 CHECK (body_expired IN (0,1)),
  created_at     TEXT NOT NULL,
  created_by     INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT
) STRICT;
CREATE INDEX idx_artifacts_owner ON artifacts (owner_type, owner_id);

-- ============================================================================
-- 10. 知识沉淀
-- ============================================================================

CREATE TABLE knowledge_import_batches (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  source_material_id INTEGER NOT NULL UNIQUE REFERENCES source_materials(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  state              TEXT NOT NULL CHECK (state IN ('Processing','AwaitingConfirmation','Failed','Completed','Cancelled')),
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

-- 12.12 Artifact：内容寻址字段不可变，只允许保留字段（expires_at/body_expired）变化
CREATE TRIGGER trg_artifacts_no_content_update BEFORE UPDATE OF
  sha256, size_bytes, storage_key, kind, sensitive, retention_kind, owner_type, owner_id, created_by, created_at ON artifacts
BEGIN SELECT RAISE(ABORT, 'artifact content addressing is immutable'); END;

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
CREATE TRIGGER trg_alert_source_credentials_no_reactivate BEFORE UPDATE OF state ON alert_source_credentials
WHEN OLD.state = 'Retired' AND NEW.state = 'Active'
BEGIN SELECT RAISE(ABORT, 'retired credential cannot be reactivated'); END;
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
-- 浏览器操作：result 一旦产生即终态不可变，且必须带 ended_at（DATA-BROWSER-003）
CREATE TRIGGER trg_browser_operations_result_immutable BEFORE UPDATE OF result ON browser_operations
WHEN OLD.result IS NOT NULL AND NEW.result <> OLD.result
BEGIN SELECT RAISE(ABORT, 'browser_operation result is final once set'); END;
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
