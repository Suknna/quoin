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

PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
PRAGMA foreign_keys = ON;
PRAGMA recursive_triggers = ON;

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
  initialized                INTEGER NOT NULL DEFAULT 0 CHECK (initialized IN (0,1)),
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

CREATE UNIQUE INDEX idx_users_single_admin ON users(role) WHERE role = 'admin';
CREATE TRIGGER trg_users_admin_identity BEFORE UPDATE OF role, enabled ON users
WHEN OLD.role = 'admin' AND (NEW.role <> 'admin' OR NEW.enabled <> 1)
BEGIN SELECT RAISE(ABORT, 'the built-in administrator cannot be demoted or disabled'); END;
CREATE TRIGGER trg_users_admin_no_delete BEFORE DELETE ON users
WHEN OLD.role = 'admin'
BEGIN SELECT RAISE(ABORT, 'the built-in administrator cannot be deleted'); END;

CREATE TABLE user_contacts (
  id INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  user_id INTEGER NOT NULL REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  channel TEXT NOT NULL CHECK (channel IN ('email','sms')),
  enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  target TEXT NOT NULL CHECK (length(target) BETWEEN 3 AND 320),
  version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
  verified_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (user_id, channel)
) STRICT;
CREATE INDEX idx_user_contacts_user ON user_contacts(user_id);

CREATE TABLE auth_flows (
  id INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  flow_type TEXT NOT NULL CHECK (flow_type IN ('admin_initialize','operator_initialize','login','contact_change')),
  user_id INTEGER NOT NULL REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  flow_token_digest BLOB NOT NULL UNIQUE CHECK (length(flow_token_digest) = 32),
  correlation_id TEXT NOT NULL DEFAULT '',
  auth_revision_at_issue INTEGER NOT NULL CHECK (auth_revision_at_issue > 0),
  password_set INTEGER NOT NULL DEFAULT 0 CHECK (password_set IN (0,1)),
  verified_contact_id INTEGER REFERENCES user_contacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  candidate_channel TEXT CHECK (candidate_channel IS NULL OR candidate_channel IN ('email','sms')),
  candidate_target TEXT CHECK (candidate_target IS NULL OR length(candidate_target) BETWEEN 3 AND 320),
  client_label TEXT NOT NULL DEFAULT 'Browser',
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','completed','failed','revoked')),
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  completed_at TEXT,
  failed_attempts INTEGER NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0)
) STRICT;
CREATE INDEX idx_auth_flows_user ON auth_flows(user_id,status);

CREATE TABLE auth_challenges (
  id INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  flow_id INTEGER NOT NULL REFERENCES auth_flows(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  purpose TEXT NOT NULL CHECK (purpose IN ('second_factor','contact_verification')),
  contact_id INTEGER NOT NULL REFERENCES user_contacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  contact_version INTEGER NOT NULL CHECK (contact_version > 0),
  auth_revision_at_issue INTEGER NOT NULL CHECK (auth_revision_at_issue > 0),
  code_digest BLOB NOT NULL CHECK (length(code_digest) = 32),
  delivery_id TEXT NOT NULL UNIQUE,
  delivery_status TEXT NOT NULL DEFAULT 'pending' CHECK (delivery_status IN ('pending','accepted','failed','unknown')),
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  consumed_at TEXT
) STRICT;
CREATE INDEX idx_auth_challenges_flow ON auth_challenges(flow_id);

CREATE TABLE auth_delivery_settings (
  id INTEGER PRIMARY KEY CHECK (id=1),
  source TEXT NOT NULL CHECK (source IN ('deployment','administrator')),
  configuration_json TEXT NOT NULL CHECK (json_valid(configuration_json)),
  secret_nonce BLOB CHECK (secret_nonce IS NULL OR length(secret_nonce)=12),
  secret_ciphertext BLOB,
  root_binding_revision INTEGER NOT NULL CHECK (root_binding_revision > 0),
  row_version INTEGER NOT NULL DEFAULT 1 CHECK (row_version > 0),
  updated_at TEXT NOT NULL,
  CHECK ((secret_nonce IS NULL AND secret_ciphertext IS NULL) OR (secret_nonce IS NOT NULL AND length(secret_ciphertext)>=16))
) STRICT;

CREATE TABLE sessions (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  user_id              INTEGER NOT NULL REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  session_token_digest BLOB NOT NULL UNIQUE CHECK (length(session_token_digest) = 32), -- raw 32-byte bearer 的 SHA-256 digest；raw bearer 只存在于 Cookie 与认证瞬间内存（DATA-AUTH-003）
  auth_revision_at_issue INTEGER NOT NULL CHECK (auth_revision_at_issue > 0), -- 自行改密事务只允许精确前进到当前 User revision；其他签发身份字段不可改写（DATA-AUTH-005）
  client_label         TEXT NOT NULL CHECK (length(client_label) BETWEEN 1 AND 200), -- 登录时由服务端从 User-Agent 机械归一为设备/浏览器摘要；不保存原始 header（UI-AUTH-004）
  created_at           TEXT NOT NULL,
  last_active_at       TEXT NOT NULL,
  idle_expires_at      TEXT NOT NULL,     -- 空闲 12 小时
  absolute_expires_at  TEXT NOT NULL,     -- 绝对 7 天
  revoked_at           TEXT
) STRICT;
CREATE INDEX idx_sessions_user ON sessions (user_id);
CREATE INDEX idx_sessions_expiry ON sessions (absolute_expires_at);

-- 当前部署根密钥绑定。verifier 是固定非秘密明文的 AES-256-GCM 密文；根密钥本身永不落库。
-- rebind 只允许 binding_revision 严格递增并替换 verifier（SEC-KEY-001..007）。
CREATE TABLE root_key_state (
  id                  INTEGER PRIMARY KEY CHECK (id = 1),
  binding_revision    INTEGER NOT NULL CHECK (binding_revision >= 1),
  verifier_nonce      BLOB NOT NULL CHECK (length(verifier_nonce) = 12),
  verifier_ciphertext BLOB NOT NULL CHECK (length(verifier_ciphertext) >= 16),
  bound_at            TEXT NOT NULL
) STRICT;

CREATE TRIGGER trg_sessions_insert_current_auth_revision BEFORE INSERT ON sessions
WHEN NOT EXISTS (
  SELECT 1 FROM users u
  WHERE u.id = NEW.user_id AND u.enabled = 1 AND u.auth_revision = NEW.auth_revision_at_issue
)
BEGIN SELECT RAISE(ABORT, 'session must bind the enabled user current auth_revision'); END;
CREATE TRIGGER trg_sessions_issue_identity_immutable BEFORE UPDATE OF user_id, session_token_digest, client_label, created_at ON sessions
BEGIN SELECT RAISE(ABORT, 'session issue identity is immutable'); END;
CREATE TRIGGER trg_sessions_auth_revision_forward BEFORE UPDATE OF auth_revision_at_issue ON sessions
WHEN OLD.revoked_at IS NOT NULL
  OR NEW.auth_revision_at_issue <> OLD.auth_revision_at_issue + 1
  OR NOT EXISTS (
    SELECT 1 FROM users u
    WHERE u.id = OLD.user_id AND u.enabled = 1 AND u.auth_revision = NEW.auth_revision_at_issue
  )
  OR EXISTS (
    SELECT 1 FROM sessions s
    WHERE s.user_id = OLD.user_id AND s.id <> OLD.id AND s.revoked_at IS NULL
  )
BEGIN SELECT RAISE(ABORT, 'current session auth revision must advance exactly once after every other user session is revoked'); END;
CREATE TRIGGER trg_sessions_revocation_sticky BEFORE UPDATE OF revoked_at ON sessions
WHEN NEW.revoked_at IS NOT OLD.revoked_at AND (OLD.revoked_at IS NOT NULL OR NEW.revoked_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'session revocation is terminal'); END;
CREATE TRIGGER trg_sessions_absolute_expiry_immutable BEFORE UPDATE OF absolute_expires_at ON sessions
WHEN NEW.absolute_expires_at IS NOT OLD.absolute_expires_at
BEGIN SELECT RAISE(ABORT, 'session absolute expiry is immutable'); END;
CREATE TRIGGER trg_sessions_activity_window_forward BEFORE UPDATE OF last_active_at, idle_expires_at ON sessions
WHEN OLD.revoked_at IS NOT NULL OR NEW.last_active_at <= OLD.last_active_at
  OR NEW.idle_expires_at <= OLD.idle_expires_at OR NEW.idle_expires_at > NEW.absolute_expires_at
BEGIN SELECT RAISE(ABORT, 'session activity and idle expiry must advance together within the absolute expiry'); END;
CREATE TRIGGER trg_root_key_state_insert_inactive_history BEFORE INSERT ON root_key_state
WHEN NEW.binding_revision <> 1
BEGIN SELECT RAISE(ABORT, 'initial root key binding revision must be one'); END;
CREATE TRIGGER trg_root_key_state_revision_forward BEFORE UPDATE ON root_key_state
WHEN NEW.id <> OLD.id OR NEW.binding_revision <> OLD.binding_revision + 1
  OR NEW.verifier_nonce IS OLD.verifier_nonce OR NEW.verifier_ciphertext IS OLD.verifier_ciphertext
  OR NEW.bound_at <= OLD.bound_at
BEGIN SELECT RAISE(ABORT, 'root key rebind must replace verifier and advance binding revision exactly once'); END;
CREATE TRIGGER trg_root_key_state_rebind_requires_isolation BEFORE UPDATE ON root_key_state
WHEN EXISTS (SELECT 1 FROM connections c WHERE c.current_credential_generation_id IS NOT NULL AND c.revalidation_required = 0)
  OR NOT EXISTS (SELECT 1 FROM maintenance_state m WHERE m.id = 1 AND m.active = 1 AND m.reason = 'RootKeyRebind')
BEGIN SELECT RAISE(ABORT, 'root key rebind requires RootKeyRebind maintenance and every connection isolated'); END;
CREATE TRIGGER trg_root_key_state_no_delete BEFORE DELETE ON root_key_state
BEGIN SELECT RAISE(ABORT, 'root key state is not deletable'); END;

-- ============================================================================
-- 2. 领域写命令账本与审计
-- ============================================================================

CREATE TABLE client_commands (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  principal_type     TEXT NOT NULL CHECK (principal_type IN ('user','service','system')),
  principal_id       INTEGER NOT NULL,           -- users.id 或 service principal id（service 无 users 行，不设 FK）
  client_command_id  TEXT NOT NULL,
  correlation_id     TEXT,
  command_type       TEXT NOT NULL,
  request_digest     TEXT NOT NULL CHECK (length(request_digest) = 64), -- 非秘密语义字段（含 expected_*；秘密只记存在性）规范化后 SHA-256（DATA-COMMAND-002）
  outcome            TEXT NOT NULL CHECK (outcome IN ('committed','rejected_known')),
  result_object_type TEXT,
  result_object_id   INTEGER,
  result_payload_json TEXT CHECK (result_payload_json IS NULL OR (
    json_valid(result_payload_json) AND json_type(result_payload_json) = 'object'
    AND json_type(result_payload_json, '$.revealHandle') IS NULL
    AND json_type(result_payload_json, '$.registrationTokenHandle') IS NULL
    AND json_type(result_payload_json, '$.bearerToken') IS NULL
    AND json_type(result_payload_json, '$.registrationToken') IS NULL
  )), -- 只持久化非秘密命令结果；reveal capability/raw secret 由内存响应拼装（SEC-REVEAL-005）
  created_at         TEXT NOT NULL,
  UNIQUE (principal_type, principal_id, client_command_id)
) STRICT;
CREATE INDEX idx_client_commands_principal ON client_commands (principal_type, principal_id, created_at);
CREATE TRIGGER trg_client_commands_no_secret_result_recursive BEFORE INSERT ON client_commands
WHEN NEW.result_payload_json IS NOT NULL AND json_valid(NEW.result_payload_json)
  AND EXISTS (
    SELECT 1 FROM json_tree(NEW.result_payload_json)
    WHERE key IN ('revealHandle','registrationTokenHandle','bearerToken','registrationToken',
                  'password','currentPassword','newPassword','kubeconfig','apiKey','rootKey'))
BEGIN SELECT RAISE(ABORT, 'command result cannot persist reveal capabilities or known raw secret fields at any nesting depth'); END;

CREATE TABLE audit_events (
  id                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  actor_type        TEXT NOT NULL CHECK (actor_type IN ('user','service','system')),
  actor_id          INTEGER NOT NULL,
  action            TEXT NOT NULL,
  correlation_id    TEXT,
  request_id        TEXT,
  phase             TEXT NOT NULL DEFAULT 'execute',
  initiator_type    TEXT CHECK (initiator_type IS NULL OR initiator_type IN ('user','service','system')),
  initiator_id      INTEGER,
  client_command_id TEXT,
  outcome           TEXT NOT NULL CHECK (outcome IN ('success','failure','rejected','unknown')),
  domain_ref_type   TEXT,
  domain_ref_id     INTEGER,
  created_at        TEXT NOT NULL
) STRICT;
CREATE INDEX idx_audit_events_correlation ON audit_events(correlation_id,id);
CREATE INDEX idx_audit_events_created ON audit_events (created_at);
CREATE INDEX idx_audit_events_actor ON audit_events (actor_type, actor_id);

CREATE TABLE audit_retention (
  id INTEGER PRIMARY KEY CHECK (id=1),
  retention_months INTEGER NOT NULL DEFAULT 6 CHECK (retention_months>=6),
  cleanup_enabled INTEGER NOT NULL DEFAULT 0 CHECK (cleanup_enabled IN (0,1)),
  row_version INTEGER NOT NULL DEFAULT 1 CHECK (row_version>0),
  last_run_at TEXT,
  last_success_cutoff_at TEXT,
  last_success_deleted_events INTEGER CHECK (last_success_deleted_events IS NULL OR last_success_deleted_events>=0),
  last_failure_at TEXT,
  last_error_code TEXT,
  updated_by_type TEXT CHECK (updated_by_type IS NULL OR updated_by_type IN ('user','service','system')),
  updated_by_id INTEGER,
  updated_at TEXT
) STRICT;
CREATE TABLE audit_cleanup_permits (
  id INTEGER PRIMARY KEY CHECK (id=1),
  active INTEGER NOT NULL DEFAULT 0 CHECK (active IN (0,1)),
  cutoff_at TEXT NOT NULL DEFAULT '',
  upper_event_id INTEGER NOT NULL DEFAULT 0 CHECK (upper_event_id>=0),
  acquired_at TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE TRIGGER trg_audit_cleanup_permit_cutoff BEFORE UPDATE ON audit_cleanup_permits
WHEN NEW.active=1 AND NOT EXISTS (
 SELECT 1 FROM audit_retention r WHERE r.id=1 AND r.cleanup_enabled=1
 AND julianday(NEW.cutoff_at) IS NOT NULL
 AND julianday(NEW.cutoff_at) <= (julianday(date('now','start of month',printf('-%d months',r.retention_months))) + min(CAST(strftime('%d','now') AS INTEGER),CAST(strftime('%d',date('now','start of month',printf('-%d months',r.retention_months),'+1 month','-1 day')) AS INTEGER))-1 + (julianday('now')-julianday(date('now'))))
 AND NEW.upper_event_id <= COALESCE((SELECT MAX(id) FROM audit_events),0)
)
BEGIN SELECT RAISE(ABORT, 'audit cleanup cutoff exceeds the retention policy'); END;
CREATE TRIGGER trg_audit_cleanup_permit_inactive_insert BEFORE INSERT ON audit_cleanup_permits
WHEN NEW.active<>0
BEGIN SELECT RAISE(ABORT, 'audit cleanup permits must start inactive'); END;
CREATE TRIGGER trg_audit_cleanup_permit_no_delete BEFORE DELETE ON audit_cleanup_permits
BEGIN SELECT RAISE(ABORT, 'audit cleanup permit singleton cannot be deleted'); END;
CREATE TRIGGER trg_audit_retention_no_delete BEFORE DELETE ON audit_retention
BEGIN SELECT RAISE(ABORT, 'audit retention singleton cannot be deleted'); END;

CREATE TABLE audit_cleanup_batches (
  id INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id>0),
  cutoff_at TEXT NOT NULL,
  upper_event_id INTEGER NOT NULL CHECK (upper_event_id>=0),
  deleted_events INTEGER NOT NULL CHECK (deleted_events>=0),
  deleted_targets INTEGER NOT NULL CHECK (deleted_targets>=0),
  final INTEGER NOT NULL CHECK (final IN (0,1)),
  created_at TEXT NOT NULL
) STRICT;
CREATE TRIGGER trg_audit_cleanup_batches_no_update BEFORE UPDATE ON audit_cleanup_batches
BEGIN SELECT RAISE(ABORT, 'audit cleanup batches are immutable'); END;
CREATE TRIGGER trg_audit_cleanup_batches_no_delete BEFORE DELETE ON audit_cleanup_batches
BEGIN SELECT RAISE(ABORT, 'audit cleanup batches are immutable'); END;

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
  CHECK ((enabled = 1 AND disabled_at IS NULL) OR (enabled = 0 AND disabled_at IS NOT NULL))
) STRICT;

CREATE TABLE alert_source_credentials (
  id                    INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  source_id             INTEGER NOT NULL REFERENCES alert_sources(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  digest                BLOB NOT NULL CHECK (length(digest) = 32), -- 32-byte Bearer 只存 digest
  state                 TEXT NOT NULL CHECK (state IN ('Active','PendingRetirement','Retired')),
  supersedes_credential_id INTEGER REFERENCES alert_source_credentials(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  first_used_at         TEXT,
  pending_retirement_at TEXT,
  row_version           INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- 生命周期命令并发前提（DATA-ALERT-009）
  created_at            TEXT NOT NULL,
  retired_at            TEXT,
  CHECK (supersedes_credential_id IS NULL OR supersedes_credential_id <> id),
  CHECK ((state = 'Active' AND pending_retirement_at IS NULL AND retired_at IS NULL)
      OR (state = 'PendingRetirement' AND pending_retirement_at IS NOT NULL AND retired_at IS NULL)
      OR (state = 'Retired' AND retired_at IS NOT NULL))
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

-- Platform faults are independent of Alertmanager Delivery and Occurrence.
-- A single open row is the lifecycle authority for one component/reason pair;
-- repeat observations only advance last_seen_at and never create alert noise.
CREATE TABLE platform_faults (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  component            TEXT NOT NULL CHECK (component = 'plinth'),
  -- Closed platform-owned failure vocabulary. Business/model/input errors stay
  -- on their own attempt and must never be promoted into this source.
  reason               TEXT NOT NULL CHECK (reason IN ('runtime_control_stream_disconnected','worker_protocol_error')),
  state                TEXT NOT NULL CHECK (state IN ('Firing','Resolved')),
  row_version          INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  first_seen_at        TEXT NOT NULL,
  last_seen_at         TEXT NOT NULL,
  resolved_at          TEXT,
  -- Execution faults record the newest terminal commit sequence. This durable
  -- order rejects late at-least-once result replays without confusing attempt
  -- creation order with the authoritative terminal commit order.
  last_execution_commit_sequence INTEGER,
  CHECK ((state = 'Firing' AND resolved_at IS NULL) OR (state = 'Resolved' AND resolved_at IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX ux_platform_faults_open_identity
  ON platform_faults(component, reason) WHERE state = 'Firing';
CREATE INDEX idx_platform_faults_state ON platform_faults(state, last_seen_at DESC);

CREATE TABLE alert_occurrence_labels (
  id            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  occurrence_id INTEGER NOT NULL REFERENCES alert_occurrences(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  name          TEXT NOT NULL,
  value         TEXT NOT NULL,
  UNIQUE (occurrence_id, name)
) STRICT;
CREATE INDEX idx_alert_occurrence_labels_name ON alert_occurrence_labels (name);

-- Attribution is immutable delivery-time evidence, not a mutable current-system
-- lookup. Candidate arrays are aligned by index: position i binds the system ID
-- to the exact configuration version which caused it to be a candidate.
CREATE TABLE alert_occurrence_attributions (
  occurrence_id                    INTEGER PRIMARY KEY REFERENCES alert_occurrences(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  status                           TEXT NOT NULL CHECK (status IN ('attributed','unattributed','conflict')),
  candidate_system_ids_json        TEXT NOT NULL CHECK (json_valid(candidate_system_ids_json) AND json_type(candidate_system_ids_json) = 'array'),
  candidate_config_version_ids_json TEXT NOT NULL CHECK (json_valid(candidate_config_version_ids_json) AND json_type(candidate_config_version_ids_json) = 'array'),
  reason_json                      TEXT NOT NULL CHECK (json_valid(reason_json)),
  evaluated_from_delivery_id       INTEGER NOT NULL REFERENCES alert_deliveries(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  evaluated_from_delivery_item_id  INTEGER NOT NULL REFERENCES alert_delivery_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at                       TEXT NOT NULL,
  CHECK (json_array_length(candidate_system_ids_json) = json_array_length(candidate_config_version_ids_json)),
  CHECK ((status = 'attributed' AND json_array_length(candidate_system_ids_json) = 1)
      OR (status = 'unattributed' AND json_array_length(candidate_system_ids_json) = 0)
      OR (status = 'conflict' AND json_array_length(candidate_system_ids_json) > 1))
) STRICT;
CREATE INDEX idx_alert_occurrence_attributions_delivery ON alert_occurrence_attributions (evaluated_from_delivery_id);

-- 业务视图告警归属投影（ADR-0008）：首次接收时一次性冻结的独立证据，与旧
-- business_system 归属历史（上表）并存；旧表停写、旧行保留，本表绝不回写
-- alert_occurrences.business_system_id。候选视图必须在其 alert_source_keys_json
-- 中显式声明交付告警源且非空精确标签条件全部命中；空标签条件不构成兜底匹配。
-- candidates_json 按序冻结每个候选视图的完整快照（viewId/viewKey/displayName/
-- scope），使唯一归属与多候选歧义在视图改名或退役后仍可追溯。
CREATE TABLE alert_occurrence_view_attributions (
  occurrence_id                    INTEGER PRIMARY KEY REFERENCES alert_occurrences(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  status                           TEXT NOT NULL CHECK (status IN ('attributed','ambiguous','unattributed')),
  attributed_view_id               INTEGER REFERENCES business_views(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  candidates_json                  TEXT NOT NULL CHECK (json_valid(candidates_json) AND json_type(candidates_json) = 'array'),
  reason_json                      TEXT NOT NULL CHECK (json_valid(reason_json)),
  evaluated_from_delivery_id       INTEGER NOT NULL REFERENCES alert_deliveries(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  evaluated_from_delivery_item_id  INTEGER NOT NULL REFERENCES alert_delivery_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at                       TEXT NOT NULL,
  CHECK ((status = 'attributed' AND attributed_view_id IS NOT NULL AND json_array_length(candidates_json) = 1)
      OR (status = 'ambiguous' AND attributed_view_id IS NULL AND json_array_length(candidates_json) > 1)
      OR (status = 'unattributed' AND attributed_view_id IS NULL AND json_array_length(candidates_json) = 0))
) STRICT;
CREATE INDEX idx_alert_occurrence_view_attributions_view ON alert_occurrence_view_attributions (attributed_view_id);

-- 归属证据冻结：首收判定后不允许任何 UPDATE/DELETE 改写；INSERT 必须闭合到
-- 同一 Delivery 的真实条目（DATA-ALERT 不可变证据语义）。
CREATE TRIGGER trg_alert_occurrence_view_attributions_immutable BEFORE UPDATE ON alert_occurrence_view_attributions
BEGIN SELECT RAISE(ABORT, 'alert view attribution is immutable delivery-time evidence'); END;
CREATE TRIGGER trg_alert_occurrence_view_attributions_no_delete BEFORE DELETE ON alert_occurrence_view_attributions
BEGIN SELECT RAISE(ABORT, 'alert view attribution is immutable delivery-time evidence'); END;
CREATE TRIGGER trg_alert_occurrence_view_attributions_item_closure BEFORE INSERT ON alert_occurrence_view_attributions
WHEN NOT EXISTS (
  SELECT 1 FROM alert_delivery_items item
  WHERE item.id = NEW.evaluated_from_delivery_item_id
    AND item.delivery_id = NEW.evaluated_from_delivery_id
)
BEGIN SELECT RAISE(ABORT, 'alert view attribution must close to its own delivery item'); END;

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
  delivery_id       INTEGER REFERENCES alert_deliveries(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 首次事件定位
  delivery_item_id  INTEGER REFERENCES alert_delivery_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 首次事件定位
  last_event_id     INTEGER REFERENCES alert_intake_issue_events(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 最近一次 repeat 事件；首次发生为 NULL
  kind              TEXT NOT NULL CHECK (kind IN ('identity_conflict','fingerprint_mismatch','delivery_truncated')),
  issue_key         TEXT NOT NULL CHECK (length(issue_key) = 64 AND issue_key NOT GLOB '*[^0-9a-f]*'), -- DATA-ALERT-011 kind-specific versioned canonical JSON SHA-256 digest
  detail_json       TEXT NOT NULL CHECK (json_valid(detail_json)), -- 首次事件诊断详情
  first_seen_at     TEXT NOT NULL,
  last_seen_at      TEXT NOT NULL,
  occurrence_count  INTEGER NOT NULL DEFAULT 1 CHECK (occurrence_count >= 1),
  acknowledged_at   TEXT,
  acknowledged_by   INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  row_version       INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  created_at        TEXT NOT NULL,
  CHECK (first_seen_at = created_at),
  CHECK (kind <> 'delivery_truncated' OR delivery_id IS NOT NULL),
  CHECK (kind = 'delivery_truncated' OR delivery_item_id IS NOT NULL),
  CHECK ((acknowledged_at IS NULL AND acknowledged_by IS NULL) OR (acknowledged_at IS NOT NULL AND acknowledged_by IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX ux_alert_intake_issue_open_signature
  ON alert_intake_issues (source_id, kind, issue_key)
  WHERE acknowledged_at IS NULL;
CREATE UNIQUE INDEX ux_alert_intake_issue_truncated ON alert_intake_issues (delivery_id) WHERE kind = 'delivery_truncated';
CREATE INDEX idx_alert_intake_issues_source ON alert_intake_issues (source_id, last_seen_at);

CREATE TABLE alert_intake_issue_events (
  id               INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  issue_id         INTEGER NOT NULL REFERENCES alert_intake_issues(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  delivery_id      INTEGER REFERENCES alert_deliveries(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  delivery_item_id INTEGER REFERENCES alert_delivery_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  detail_json      TEXT NOT NULL CHECK (json_valid(detail_json)),
  observed_at      TEXT NOT NULL,
  CHECK (delivery_id IS NOT NULL OR delivery_item_id IS NOT NULL)
) STRICT;
CREATE INDEX idx_alert_intake_issue_events_issue ON alert_intake_issue_events (issue_id, observed_at, id);
CREATE UNIQUE INDEX ux_alert_intake_issue_events_item ON alert_intake_issue_events (delivery_item_id) WHERE delivery_item_id IS NOT NULL;
CREATE UNIQUE INDEX ux_alert_intake_issue_events_delivery ON alert_intake_issue_events (delivery_id) WHERE delivery_item_id IS NULL;

-- 接入问题必须闭合到同一告警源的真实异常 Delivery/Item；kind、首事件定位与后续事件不可漂移。
CREATE TRIGGER trg_alert_intake_issues_source_closure BEFORE INSERT ON alert_intake_issues
WHEN NOT (
  (NEW.kind = 'delivery_truncated' AND NEW.delivery_item_id IS NULL AND EXISTS (
    SELECT 1 FROM alert_deliveries d
    WHERE d.id = NEW.delivery_id AND d.source_id = NEW.source_id
      AND d.integrity = 'truncated' AND d.status = 'processed'
  ))
  OR (NEW.kind IN ('identity_conflict','fingerprint_mismatch') AND EXISTS (
    SELECT 1 FROM alert_delivery_items i JOIN alert_deliveries d ON d.id = i.delivery_id
    WHERE i.id = NEW.delivery_item_id AND d.id = NEW.delivery_id AND d.source_id = NEW.source_id
      AND i.status = NEW.kind
  ))
)
BEGIN SELECT RAISE(ABORT, 'alert intake issue must close to the same source and matching anomalous delivery item'); END;
CREATE TRIGGER trg_alert_intake_issue_events_source_closure BEFORE INSERT ON alert_intake_issue_events
WHEN NOT EXISTS (
  SELECT 1 FROM alert_intake_issues issue
  WHERE issue.id = NEW.issue_id AND issue.acknowledged_at IS NULL AND (
    (issue.kind = 'delivery_truncated' AND NEW.delivery_item_id IS NULL AND EXISTS (
      SELECT 1 FROM alert_deliveries d
      WHERE d.id = NEW.delivery_id AND d.source_id = issue.source_id
        AND d.integrity = 'truncated' AND d.status = 'processed'
    ))
    OR (issue.kind IN ('identity_conflict','fingerprint_mismatch') AND EXISTS (
      SELECT 1 FROM alert_delivery_items i JOIN alert_deliveries d ON d.id = i.delivery_id
      WHERE i.id = NEW.delivery_item_id AND d.source_id = issue.source_id
        AND i.status = issue.kind AND (NEW.delivery_id IS NULL OR NEW.delivery_id = d.id)
    ))
  )
)
BEGIN SELECT RAISE(ABORT, 'alert intake issue event must match its issue source and anomaly kind'); END;
CREATE TRIGGER trg_alert_intake_issues_insert_open BEFORE INSERT ON alert_intake_issues
WHEN NEW.acknowledged_at IS NOT NULL OR NEW.acknowledged_by IS NOT NULL
  OR NEW.occurrence_count <> 1 OR NEW.last_seen_at <> NEW.first_seen_at
  OR NEW.last_event_id IS NOT NULL OR NEW.row_version <> 1
BEGIN SELECT RAISE(ABORT, 'alert intake issue must be created as one unacknowledged first occurrence'); END;

-- 有界派生变更日志：id 即单调递增 change_seq（AUTOINCREMENT，同事务分配、永不复用）；
-- 可清理（保留窗口由部署配置），不是告警历史权威源。最新行（MAX(id)）是回放 high-water，
-- 由触发器强制保留、永不删除（trg_alert_change_log_no_delete_latest，DATA-SSE-009）。
CREATE TABLE alert_change_log (
  id                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  occurrence_id     INTEGER REFERENCES alert_occurrences(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  platform_fault_id INTEGER REFERENCES platform_faults(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  change_type       TEXT NOT NULL CHECK (change_type IN ('created','state_changed')),
  row_version       INTEGER NOT NULL CHECK (row_version >= 1),
  committed_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  CHECK ((occurrence_id IS NOT NULL AND platform_fault_id IS NULL) OR (occurrence_id IS NULL AND platform_fault_id IS NOT NULL))
) STRICT;
CREATE INDEX idx_alert_change_log_occurrence ON alert_change_log (occurrence_id);

-- 有界派生任务变更日志：id 即单调递增 task_change_seq（AUTOINCREMENT，同事务分配、永不复用）；
-- 与权威对象的状态/阶段变化同一事务写入；可清理（保留窗口由部署配置）、可丢弃、可重建，
-- 不是任务历史权威源（DATA-SSE-004/005/006）。object_type/object_id 为多态引用，由应用类型化校验。
CREATE TABLE task_change_log (
  id           INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  object_type  TEXT NOT NULL CHECK (object_type IN
     ('initial_analysis','execution_attempt','inspection_run','inspection_report',
      'tool_call','knowledge_import_batch','knowledge_candidate',
      'config_verification_run','resource_refresh_run')),
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
  attempt_id  INTEGER NOT NULL UNIQUE REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
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
  attempt_id        INTEGER NOT NULL REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  seq               INTEGER NOT NULL CHECK (seq >= 1),
  role              TEXT NOT NULL CHECK (role IN ('user','assistant')),
  status            TEXT NOT NULL CHECK (status IN ('active','withdrawn')),
  content           TEXT NOT NULL,
  client_command_id TEXT,
  parent_message_id INTEGER REFERENCES investigation_messages(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at        TEXT NOT NULL,
  UNIQUE (investigation_id, seq),
  UNIQUE (attempt_id, role)
) STRICT;
CREATE INDEX idx_investigation_messages_parent ON investigation_messages (parent_message_id);
CREATE INDEX idx_investigation_messages_attempt ON investigation_messages (attempt_id);

-- Investigation 可由既有产品对象进入，也可无来源直接进入纯 Chat；来源是在创建事务中可写入多条、
-- 此后不可改写的显式谱系，不是进入对话前的选择向导。
CREATE TABLE investigation_source_links (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  investigation_id    INTEGER NOT NULL REFERENCES investigations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  occurrence_id       INTEGER REFERENCES alert_occurrences(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  initial_analysis_id INTEGER REFERENCES initial_analyses(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  evidence_id         INTEGER REFERENCES evidence(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  inspection_report_id INTEGER REFERENCES inspection_reports(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  linked_by           INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  linked_at           TEXT NOT NULL,
  CHECK ((occurrence_id IS NOT NULL) + (initial_analysis_id IS NOT NULL) +
         (evidence_id IS NOT NULL) + (inspection_report_id IS NOT NULL) = 1)
) STRICT;
CREATE UNIQUE INDEX ux_investigation_source_occurrence ON investigation_source_links (investigation_id, occurrence_id) WHERE occurrence_id IS NOT NULL;
CREATE UNIQUE INDEX ux_investigation_source_analysis ON investigation_source_links (investigation_id, initial_analysis_id) WHERE initial_analysis_id IS NOT NULL;
CREATE UNIQUE INDEX ux_investigation_source_evidence ON investigation_source_links (investigation_id, evidence_id) WHERE evidence_id IS NOT NULL;
CREATE UNIQUE INDEX ux_investigation_source_report ON investigation_source_links (investigation_id, inspection_report_id) WHERE inspection_report_id IS NOT NULL;

-- 上传先建立独立 TextAttachment；消息发送事务再按 ordinal 建立引用。附件对象与消息引用分离，
-- 因而新调查可在首条消息落库前 staging 多个附件，Undo 后也可复用原附件而不重复上传。
CREATE TABLE text_attachments (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  source_material_id INTEGER NOT NULL UNIQUE REFERENCES source_materials(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  artifact_id        INTEGER NOT NULL UNIQUE REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 正文存 Artifact
  original_filename  TEXT NOT NULL,
  size_bytes         INTEGER NOT NULL CHECK (size_bytes >= 0),
  digest             TEXT NOT NULL CHECK (length(digest) = 64),
  uploaded_by        INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  uploaded_at        TEXT NOT NULL
) STRICT;

CREATE TABLE investigation_message_attachments (
  message_id    INTEGER NOT NULL REFERENCES investigation_messages(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  attachment_id INTEGER NOT NULL REFERENCES text_attachments(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  ordinal       INTEGER NOT NULL CHECK (ordinal >= 0),
  PRIMARY KEY (message_id, attachment_id),
  UNIQUE (message_id, ordinal)
) WITHOUT ROWID, STRICT;
CREATE INDEX idx_investigation_message_attachments_attachment ON investigation_message_attachments (attachment_id);

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
  id             INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  version        INTEGER NOT NULL CHECK (version >= 1),
  yaml_body      TEXT NOT NULL,          -- strict YAML 原文（Q12.1 B：Label Contract 只接受严格单文档 YAML，与业务系统配置同一解析机制）
  contract_json  TEXT NOT NULL CHECK (json_valid(contract_json)), -- 解析一次的类型化投影（运行只使用它）
  digest         TEXT NOT NULL CHECK (length(digest) = 64),
  parser_version TEXT NOT NULL,
  schema_version TEXT NOT NULL,
  state          TEXT NOT NULL CHECK (state IN ('draft','active','retired')), -- 派生投影：只由 label_contract_state 指针经触发器维护（DATA-CONFIG-006），禁止手工改写
  row_version    INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- 激活命令对目标草稿行的并发前提（DATA-CONFIG-005）
  created_at     TEXT NOT NULL,
  activated_at   TEXT,  -- 一次性激活事实：NULL -> 时间戳一次，只由激活触发器写入
  UNIQUE (version)
) STRICT;

-- Label Contract 当前指针单行聚合：激活命令的并发前提权威与状态派生来源（DATA-CONFIG-005/006）。
-- current_activation_id 指向产生当前状态的不可变激活命令；current_contract_id 必须与该命令成对变化。
-- 这使直接指针 UPDATE 无法伪装成触发器内部写入，避免依赖时间戳或连接内临时状态。
CREATE TABLE label_contract_state (
  id                    INTEGER PRIMARY KEY CHECK (id = 1),
  current_contract_id   INTEGER REFERENCES label_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  current_activation_id INTEGER REFERENCES label_contract_activations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  row_version           INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  updated_at            TEXT NOT NULL,
  CHECK ((current_contract_id IS NULL) = (current_activation_id IS NULL))
) STRICT;

CREATE TABLE business_systems (
  id                                 INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  key                                TEXT NOT NULL UNIQUE,  -- 稳定用户 key，退役不复用
  display_name                       TEXT NOT NULL,
  enabled                            INTEGER NOT NULL CHECK (enabled IN (0,1)),
  row_version                        INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- 系统行并发前提（DATA-CONFIG-005）
  current_config_version_id          INTEGER REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  timezone                           TEXT,  -- 已发布配置版本的根投影；发布/联合激活时由触发器同步（DATA-CONFIG-001），从未发布时为 NULL
  created_at                         TEXT NOT NULL
) STRICT;

CREATE TABLE business_system_config_versions (
  id                                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  business_system_id                INTEGER NOT NULL REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  version_seq                       INTEGER NOT NULL CHECK (version_seq >= 1),
  state                             TEXT NOT NULL CHECK (state IN ('draft','published','superseded')), -- 派生投影：只由 business_systems.current_config_version_id 经触发器维护（DATA-CONFIG-001），禁止手工改写
  yaml_body                         TEXT NOT NULL,
  parser_version                    TEXT NOT NULL,
  schema_version                    TEXT NOT NULL,
  -- Historical provenance only. New declaration uploads leave this NULL:
  -- Label Contracts no longer govern active Business System lifecycle.
  label_contract_version_id         INTEGER REFERENCES label_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  -- Legacy history predating the declaration cutover deliberately has no
  -- declaration projection. Only appended canonical successors are executable.
  declaration_json                  TEXT CHECK (declaration_json IS NULL OR json_valid(declaration_json)),
  description                       TEXT NOT NULL DEFAULT '',
  discovery_refresh_seconds         INTEGER NOT NULL DEFAULT 300 CHECK (discovery_refresh_seconds BETWEEN 60 AND 86400),
  digest                            TEXT NOT NULL CHECK (length(digest) = 64),
  created_by                        INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at                        TEXT NOT NULL,
  published_at                      TEXT,  -- 一次性发布事实：NULL -> 时间戳一次，只由 current 指针变化触发器写入
  -- 解析一次的类型化根投影（DATA-CONFIG-003）：运行只使用类型结构，不重新解析 YAML
  system_key                        TEXT NOT NULL,  -- 必须等于 business_systems.key（trg_business_config_versions_system_key_match）
  display_name                      TEXT NOT NULL,
  metrics_connection_id             INTEGER NOT NULL REFERENCES connections(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 版本化明确指标接入引用；尝试授权冻结其 revision/credential generation
  enabled                           INTEGER NOT NULL CHECK (enabled IN (0,1)),
  timezone                          TEXT NOT NULL,  -- IANA 时区（根节点统一提供，DATA-CONFIG-004）
  UNIQUE (business_system_id, version_seq)
) STRICT;

-- Optional alert attribution restrictions declared with a config version.
-- Empty source refs/label conditions mean no extra restriction; the Label
-- Contract business label remains the mandatory attribution authority.
CREATE TABLE config_alert_source_refs (
  id                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  config_version_id INTEGER NOT NULL REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  alert_source_id   INTEGER NOT NULL REFERENCES alert_sources(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  UNIQUE (config_version_id, alert_source_id)
) STRICT;

CREATE TABLE config_alert_label_conditions (
  id                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  config_version_id INTEGER NOT NULL REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  label_name        TEXT NOT NULL,
  label_value       TEXT NOT NULL,
  UNIQUE (config_version_id, label_name)
) STRICT;

-- Compiled resource scopes are the execution authority. Legacy discovery rows
-- remain available only to read historic configurations.
-- Durable audit link from immutable predecessor history to its appended
-- canonical successor. It is intentionally append-only alongside the ledger.
CREATE TABLE legacy_config_version_mappings (
  id                       INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  legacy_config_version_id INTEGER NOT NULL UNIQUE REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  canonical_config_version_id INTEGER NOT NULL UNIQUE REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  migration_id             TEXT NOT NULL REFERENCES migration_ledger(migration_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at               TEXT NOT NULL
) STRICT;

CREATE TABLE config_resource_scopes (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  config_version_id    INTEGER NOT NULL REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  resource_key         TEXT NOT NULL,
  display_name         TEXT NOT NULL,
  discovery_metric     TEXT NOT NULL,
  selectors_json       TEXT NOT NULL CHECK (json_valid(selectors_json)),
  identity_labels_json TEXT NOT NULL CHECK (json_valid(identity_labels_json)),
  allowed_metrics_json TEXT NOT NULL CHECK (json_valid(allowed_metrics_json)),
  UNIQUE (config_version_id, resource_key)
) STRICT;

CREATE TABLE config_discoveries (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  config_version_id    INTEGER NOT NULL REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  discovery_key        TEXT NOT NULL,          -- 跨版本稳定 key
  display_name         TEXT NOT NULL,
  selector             TEXT NOT NULL,          -- 单个 instant vector selector（上传时经 Prometheus 官方 AST 校验：禁止 offset/@/聚合/label_replace，DATA-CONFIG-003/CFG-PROMQL-002）
  identity_labels_json TEXT NOT NULL CHECK (json_valid(identity_labels_json)),
  UNIQUE (config_version_id, discovery_key)
) STRICT;

CREATE TABLE config_plans (
  id                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  config_version_id INTEGER NOT NULL REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  plan_key          TEXT NOT NULL,                  -- 跨版本稳定 key
  display_name      TEXT NOT NULL,
  timezone          TEXT NOT NULL DEFAULT 'UTC',
  cron              TEXT,                          -- 标准五字段 cron；NULL = 仅人工运行；时区由配置根节点统一提供（DATA-CONFIG-004）
  UNIQUE (config_version_id, plan_key)
) STRICT;

CREATE TABLE config_checks (
  id                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  plan_id           INTEGER NOT NULL REFERENCES config_plans(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  check_key         TEXT NOT NULL,                  -- 跨版本稳定 key
  display_name      TEXT NOT NULL,
  analysis_question TEXT NOT NULL,
  kind              TEXT NOT NULL CHECK (kind = 'promql'),
  query_mode        TEXT CHECK (query_mode IN ('instant','range')), -- promql：查询模式；range 以真实 evidence_at 为终点并保存实际 start/end/step
  expression        TEXT,                          -- promql：字面量表达式（上传时经 Prometheus 官方 AST 校验）
  range_seconds     INTEGER CHECK (range_seconds IS NULL OR range_seconds > 0),  -- range 查询窗口
  step_seconds      INTEGER CHECK (step_seconds IS NULL OR step_seconds > 0),    -- range 查询步长
  UNIQUE (plan_id, check_key),
  CHECK (
    (kind = 'promql' AND query_mode IS NOT NULL AND expression IS NOT NULL
      AND ((query_mode = 'instant' AND range_seconds IS NULL AND step_seconds IS NULL)
           OR (query_mode = 'range' AND range_seconds IS NOT NULL AND step_seconds IS NOT NULL)))
  )
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

-- ============================================================================
-- 6. 连接、凭据与浏览器身份
-- ============================================================================

CREATE TABLE connections (
  id                                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  name                              TEXT NOT NULL UNIQUE,  -- 稳定用户 key，退役不复用
  type                              TEXT NOT NULL CHECK (type IN ('prometheus','thanos','model_provider')),
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
  id                   INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  connection_id        INTEGER NOT NULL REFERENCES connections(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  generation_seq       INTEGER NOT NULL CHECK (generation_seq >= 1),
  envelope_version     INTEGER NOT NULL CHECK (envelope_version = 1),
  key_binding_revision INTEGER NOT NULL CHECK (key_binding_revision >= 1),
  nonce                BLOB NOT NULL CHECK (length(nonce) = 12),
  ciphertext           BLOB NOT NULL CHECK (length(ciphertext) >= 16), -- AES-256-GCM ciphertext+tag；AAD 见 security.md
  created_by           INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at           TEXT NOT NULL,
  UNIQUE (connection_id, generation_seq),
  UNIQUE (key_binding_revision, nonce)
) STRICT;

-- Connection Probe 是 supervisor 直接执行的 immutable typed result；动作正文及 action-set/version
-- 由 contracts/connection-probes.yaml 独占。header 与 execution_attempts 1:1，typed child 按连接类型封闭。
CREATE TABLE connection_probe_results (
  id                            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  attempt_id                    INTEGER NOT NULL UNIQUE REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  connection_id                 INTEGER NOT NULL REFERENCES connections(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  connection_type               TEXT NOT NULL CHECK (connection_type IN ('model_provider','prometheus','thanos')),
  connection_revision_id        INTEGER NOT NULL REFERENCES connection_revisions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  credential_generation_id      INTEGER NOT NULL REFERENCES credential_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  root_binding_revision         INTEGER NOT NULL CHECK (root_binding_revision >= 1),
  action_set_id                 TEXT NOT NULL CHECK (length(action_set_id) > 0),
  action_set_version            INTEGER NOT NULL CHECK (action_set_version >= 1),
  probe_contract_digest         TEXT NOT NULL CHECK (length(probe_contract_digest) = 64 AND probe_contract_digest NOT GLOB '*[^0-9a-f]*'),
  outcome                       TEXT NOT NULL CHECK (outcome IN ('passed','failed','cancelled','interrupted')),
  result_digest                 TEXT NOT NULL CHECK (length(result_digest) = 64 AND result_digest NOT GLOB '*[^0-9a-f]*'),
  started_at                    TEXT NOT NULL,
  finished_at                   TEXT NOT NULL,
  created_at                    TEXT NOT NULL,
  UNIQUE (connection_revision_id, credential_generation_id, probe_contract_digest, result_digest)
) STRICT;

CREATE TABLE model_provider_connection_probe_results (
  probe_result_id                   INTEGER PRIMARY KEY REFERENCES connection_probe_results(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  chat_model_id                     TEXT NOT NULL,
  embedding_model_id                TEXT,
  context_budget_tokens             INTEGER NOT NULL CHECK (context_budget_tokens >= 1),
  max_output_tokens                 INTEGER NOT NULL CHECK (max_output_tokens >= 1),
  streaming_supported               INTEGER NOT NULL CHECK (streaming_supported IN (0,1)),
  native_tool_calling_supported     INTEGER NOT NULL CHECK (native_tool_calling_supported IN (0,1)),
  multi_tool_call_supported         INTEGER NOT NULL CHECK (multi_tool_call_supported IN (0,1)),
  cancellation_observed             INTEGER NOT NULL CHECK (cancellation_observed IN (0,1)),
  usage_observed                    INTEGER NOT NULL CHECK (usage_observed IN (0,1)),
  request_id_observed               INTEGER NOT NULL CHECK (request_id_observed IN (0,1)),
  embedding_supported               INTEGER NOT NULL CHECK (embedding_supported IN (0,1)),
  embedding_vector_dim              INTEGER CHECK (embedding_vector_dim IS NULL OR embedding_vector_dim >= 1),
  detail_json                       TEXT NOT NULL CHECK (json_valid(detail_json)),
  CHECK ((embedding_supported = 1 AND embedding_model_id IS NOT NULL AND embedding_vector_dim IS NOT NULL)
      OR (embedding_supported = 0 AND embedding_model_id IS NULL AND embedding_vector_dim IS NULL))
) STRICT;

CREATE TABLE thanos_connection_probe_results (
  probe_result_id INTEGER PRIMARY KEY REFERENCES connection_probe_results(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  query           TEXT NOT NULL CHECK (query = 'vector(1)'),
  response_type   TEXT NOT NULL CHECK (response_type = 'vector'),
  sample_count    INTEGER NOT NULL CHECK (sample_count = 1),
  sample_value    TEXT NOT NULL,
  detail_json     TEXT NOT NULL CHECK (json_valid(detail_json))
) STRICT;

-- Model Provider 每次 disabled->enabled 都追加一个显式 qualification 事件；connections 不保存
-- "current/latest qualification" 指针。后续 grant 只可复制与当前 enabled row_version 对应的 immutable probe_result_id。
CREATE TABLE connection_enable_qualifications (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  connection_id       INTEGER NOT NULL REFERENCES connections(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  enabled_row_version INTEGER NOT NULL CHECK (enabled_row_version >= 2),
  probe_result_id     INTEGER NOT NULL REFERENCES connection_probe_results(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_by          INTEGER NOT NULL REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at          TEXT NOT NULL,
  UNIQUE (connection_id, enabled_row_version),
  UNIQUE (connection_id, enabled_row_version, probe_result_id)
) STRICT;

-- Only the model provider is process-wide singular. Metrics connections are
-- independently selected by a business declaration and may all be enabled.
CREATE UNIQUE INDEX ux_connections_one_enabled_model_provider ON connections ((1))
  WHERE type = 'model_provider' AND enabled = 1;

-- ============================================================================
-- 7. 巡检运行与检查结果
-- ============================================================================

-- 独立巡检计划（ADR-0004）：不再内嵌于业务声明。计划直接绑定一个来源接入与
-- 一个插件模板；Run 创建时展开并冻结目标、模板版本、查询窗口与接入授权。
-- plan_key 是跨版本稳定用户 key，退役不复用（稳定身份保留条款）。
CREATE TABLE inspection_plans (
  id               INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  plan_key         TEXT NOT NULL UNIQUE,  -- ^[a-z][a-z0-9-]{0,62}$，退役不复用
  display_name     TEXT NOT NULL,
  enabled          INTEGER NOT NULL CHECK (enabled IN (0,1)),
  connection_id    INTEGER NOT NULL REFERENCES connections(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  plugin_id        TEXT NOT NULL CHECK (length(plugin_id) > 0),
  template_id      TEXT NOT NULL CHECK (length(template_id) > 0),
  template_version TEXT,                  -- NULL = Run 创建时冻结当时 ready 版本
  params_json      TEXT NOT NULL CHECK (json_valid(params_json) AND json_type(params_json) = 'object'),
  -- 分析语义元数据（全部可选）：Run 创建时冻结进 inspection_runs 的对应列，
  -- 计划后续修改不改写任何已存在 Run。
  check_description   TEXT CHECK (check_description IS NULL OR length(check_description) <= 2000),   -- 检查说明：这项检查在量什么
  metric_unit         TEXT CHECK (metric_unit IS NULL OR length(metric_unit) <= 100),                -- 指标单位：结果数值的语义单位
  report_instructions TEXT CHECK (report_instructions IS NULL OR length(report_instructions) <= 4000), -- 初始报告要求：分析的用户级指令基线
  scope_kind       TEXT NOT NULL CHECK (scope_kind IN ('integration','business_view','objects')),
  scope_json       TEXT NOT NULL CHECK (json_valid(scope_json)), -- business_view key 或显式对象集合
  cron             TEXT,                  -- 标准五字段 cron；NULL = 仅人工运行（不产生定时模型费用）
  timezone         TEXT NOT NULL DEFAULT 'UTC',
  row_version      INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  created_by       INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL,
  CHECK ((scope_kind = 'integration' AND json_extract(scope_json, '$.kind') = 'integration'
            AND json_extract(scope_json, '$.businessViewKey') IS NULL AND json_extract(scope_json, '$.objects') IS NULL)
      OR (scope_kind = 'business_view' AND json_valid(scope_json) AND json_extract(scope_json, '$.businessViewKey') IS NOT NULL)
      OR (scope_kind = 'objects' AND json_valid(scope_json) AND json_type(json_extract(scope_json, '$.objects')) = 'array'))
) STRICT;

-- 巡检运行：新 Run 由独立计划产生并冻结模板与接入；business_system_id /
-- config_version_id 仅保留给历史 Run（旧业务声明计划），禁止新写入。
CREATE TABLE inspection_runs (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  business_system_id        INTEGER REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 历史 Run 专用
  plan_key                  TEXT NOT NULL,
  config_version_id         INTEGER REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 历史 Run 专用
  label_contract_version_id INTEGER REFERENCES label_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 历史 Run 专用
  plan_id                   INTEGER REFERENCES inspection_plans(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 新 Run 必填
  connection_id             INTEGER REFERENCES connections(id) ON UPDATE RESTRICT ON DELETE RESTRICT,     -- 新 Run 冻结的来源接入
  plugin_id                 TEXT,                   -- 新 Run 冻结：插件身份
  template_id               TEXT,                   -- 新 Run 冻结：模板身份
  template_version          TEXT,                   -- 新 Run 冻结：模板版本
  frozen_params_json        TEXT CHECK (frozen_params_json IS NULL OR json_valid(frozen_params_json)),
  frozen_scope_json         TEXT CHECK (frozen_scope_json IS NULL OR json_valid(frozen_scope_json)),
  -- 分析语义冻结（新 Run 创建时从计划复制；历史声明 Run 为 NULL）：名称、检查
  -- 说明、单位与初始报告要求随 Run 冻结，计划后续修改不改写已存在 Run，重采证
  -- 原样复制；实际分析要求最终冻结在每个 analysis Attempt 的输入快照。
  frozen_display_name        TEXT,
  frozen_check_description   TEXT,
  frozen_metric_unit         TEXT,
  frozen_report_instructions TEXT,
  trigger_kind              TEXT NOT NULL CHECK (trigger_kind IN ('schedule','manual')),
  scheduled_for             TEXT,                    -- UTC；NULL = 人工触发
  state                     TEXT NOT NULL CHECK (state IN ('Queued','Running','Completed','CompletedWithGaps','Failed','Cancelled','Interrupted','SkippedOverlap')),
  row_version               INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  evidence_at               TEXT,                    -- 真正采证开始时生成
  rerun_of_id               INTEGER REFERENCES inspection_runs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at                TEXT NOT NULL,
  -- 计划 Run 与历史声明 Run 互斥：不是双写，也不存在两可状态。
  CHECK (
    (plan_id IS NOT NULL AND business_system_id IS NULL AND config_version_id IS NULL AND label_contract_version_id IS NULL
      AND connection_id IS NOT NULL AND plugin_id IS NOT NULL AND template_id IS NOT NULL AND template_version IS NOT NULL
      AND frozen_params_json IS NOT NULL AND frozen_scope_json IS NOT NULL)
    OR (plan_id IS NULL AND business_system_id IS NOT NULL AND config_version_id IS NOT NULL
      AND connection_id IS NULL AND plugin_id IS NULL AND template_id IS NULL AND template_version IS NULL
      AND frozen_params_json IS NULL AND frozen_scope_json IS NULL)
  ),
  CHECK (
    (state IN ('Queued','SkippedOverlap') AND evidence_at IS NULL)
    OR (state IN ('Running','Completed','CompletedWithGaps') AND evidence_at IS NOT NULL)
    OR state IN ('Failed','Cancelled','Interrupted')
  ),
  CHECK (
    (trigger_kind = 'schedule' AND scheduled_for IS NOT NULL)
    OR (trigger_kind = 'manual' AND scheduled_for IS NULL)
  )
) STRICT;
-- COALESCE 键把历史 (business_system_id, plan_key) 与新 (plan_key) 两个身份空间
-- 并入同一唯一索引；历史行仍满足原语义。
CREATE UNIQUE INDEX ux_inspection_run_scheduled ON inspection_runs (COALESCE(business_system_id,0), plan_key, scheduled_for) WHERE scheduled_for IS NOT NULL;
CREATE UNIQUE INDEX ux_inspection_run_active ON inspection_runs (COALESCE(business_system_id,0), plan_key)
  WHERE state IN ('Queued','Running');
CREATE INDEX idx_inspection_runs_plan ON inspection_runs (COALESCE(business_system_id,0), plan_key, created_at DESC);
CREATE INDEX idx_inspection_runs_plan_id ON inspection_runs (plan_id, created_at DESC);

-- Run 创建时冻结展开的检查目录：执行中不扩大目标，重新采证必须创建新 Run。
-- 历史 Run（plan_id IS NULL）的检查目录仍由 config_checks/config_plans 承载。
CREATE TABLE inspection_run_checks (
  id            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  run_id        INTEGER NOT NULL REFERENCES inspection_runs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  check_key     TEXT NOT NULL,
  display_name  TEXT NOT NULL,
  plugin_id     TEXT NOT NULL,
  template_id   TEXT NOT NULL,
  template_version TEXT NOT NULL,
  params_json   TEXT NOT NULL CHECK (json_valid(params_json)),
  target_json   TEXT CHECK (target_json IS NULL OR json_valid(target_json)), -- objects 范围的冻结目标（objectType+identityKey）
  created_at    TEXT NOT NULL,
  UNIQUE (run_id, check_key)
) STRICT;

CREATE TABLE inspection_check_results (
  id            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  run_id        INTEGER NOT NULL REFERENCES inspection_runs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  check_key     TEXT NOT NULL,
  status        TEXT NOT NULL CHECK (status IN ('ok','error','gap')),
  evidence_id   INTEGER REFERENCES evidence(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  attempt_id    INTEGER REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 每项都绑定精确 inspection_collection Attempt；PromQL 的 immutable ResultProposal 同样受 Attempt/epoch fence
  result_digest BLOB CHECK (result_digest IS NULL OR length(result_digest) = 32), -- PromQL Result 重放摘要
  gap_reason  TEXT CHECK (gap_reason IS NULL OR gap_reason IN (
                'runtime_unavailable','authentication_required','authentication_probe_unavailable','identity_busy',
                'query_failed','partial_response','no_data','cancelled','interrupted')),
  -- 采集元数据冻结（提交时一次性写入后不可变）：observedAt、真实 warnings 与
  -- （范围模板的）执行窗口事实。成功结果的正文仍在 Evidence；gap/error 没有
  -- Evidence，其观察时间/warnings/窗口元数据只由本列承载，分析清单据此保持
  -- 缺口可见，绝不为缺口伪造执行事实。历史行（升级前）为 NULL。
  meta_json   TEXT CHECK (meta_json IS NULL OR json_valid(meta_json)),
  created_at  TEXT NOT NULL,
  UNIQUE (run_id, check_key),
  CHECK (
    (status = 'ok' AND evidence_id IS NOT NULL AND gap_reason IS NULL)
    OR (status IN ('error','gap') AND gap_reason IS NOT NULL)
  ),
  CHECK (attempt_id IS NOT NULL OR result_digest IS NULL),
  CHECK (result_digest IS NULL OR status IN ('error','gap') OR evidence_id IS NOT NULL)
) STRICT;
CREATE UNIQUE INDEX ux_inspection_check_result_evidence ON inspection_check_results (evidence_id) WHERE evidence_id IS NOT NULL;

-- 每次 inspection_analysis Attempt 实际生效的分析要求冻结（CREATE 后不可改）：
-- report_instructions_override 三态——NULL 行/缺行/列 NULL 表示沿用 Run 冻结的
-- 初始报告要求；'' 表示本次显式无报告要求（清除）；非空文本为仅本次覆盖。检查
-- 说明与单位永远来自 Run 冻结列（本表不复制），旧 Attempt（本表无行）重建字节
-- 保持不变。
CREATE TABLE inspection_analysis_requirements (
  attempt_id                   INTEGER PRIMARY KEY REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  report_instructions_override TEXT CHECK (report_instructions_override IS NULL OR length(report_instructions_override) <= 4000),
  created_at                   TEXT NOT NULL
) STRICT;
CREATE TRIGGER trg_inspection_analysis_requirements_immutable BEFORE UPDATE ON inspection_analysis_requirements
BEGIN SELECT RAISE(ABORT, 'inspection analysis requirements are immutable'); END;
CREATE TRIGGER trg_inspection_analysis_requirements_no_delete BEFORE DELETE ON inspection_analysis_requirements
BEGIN SELECT RAISE(ABORT, 'inspection analysis requirements are not deletable'); END;
-- 闭合约束：要求行只能绑定 inspection_analysis × run 的 Attempt（与
-- trg_inspection_analysis_requires_closed_run 同一引用模式），绝不允许把分析
-- 要求挂到采集/探测等其它 Attempt 身份上。
CREATE TRIGGER trg_inspection_analysis_requirements_scope BEFORE INSERT ON inspection_analysis_requirements
WHEN NOT EXISTS (
  SELECT 1 FROM execution_attempts a
  WHERE a.id = NEW.attempt_id AND a.attempt_type = 'inspection_analysis' AND a.scope_type = 'run'
)
BEGIN SELECT RAISE(ABORT, 'inspection analysis requirements must bind an inspection_analysis run Attempt'); END;

-- ============================================================================
-- 7.5 来源级观测（ADR-0004）：接入即有界观测，身份 = 接入 + 对象类型 + 规范来源身份
-- ============================================================================

-- 来源级观测运行：对单个接入的一次完整有界发现采集。同一接入至多一个活动 Run；
-- 失败/截断/局部结果不得清空资源，也不推断物理删除。
CREATE TABLE observation_runs (
  id            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  connection_id INTEGER NOT NULL REFERENCES connections(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  plugin_id     TEXT NOT NULL CHECK (length(plugin_id) > 0),
  trigger_kind  TEXT NOT NULL CHECK (trigger_kind IN ('manual','schedule','enablement')),
  scheduled_for TEXT,                    -- UTC；仅 schedule 携带，作为确定性去重键
  state         TEXT NOT NULL CHECK (state IN ('Queued','Running','Completed','CompletedWithWarnings','Failed','Cancelled','Interrupted')),
  row_version   INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  evidence_at   TEXT,                    -- 真正开始观测时生成
  result_detail TEXT,
  created_by    INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at    TEXT NOT NULL,
  CHECK ((state IN ('Queued','Running','Completed','CompletedWithWarnings') AND result_detail IS NULL)
      OR (state IN ('Failed','Cancelled','Interrupted') AND result_detail IS NOT NULL)),
  CHECK (
    (trigger_kind = 'manual' AND scheduled_for IS NULL)
    OR (trigger_kind = 'enablement' AND scheduled_for IS NULL)
    OR (trigger_kind = 'schedule' AND scheduled_for IS NOT NULL)
  )
) STRICT;
CREATE UNIQUE INDEX ux_observation_run_active ON observation_runs (connection_id)
  WHERE state IN ('Queued','Running');
CREATE UNIQUE INDEX ux_observation_run_scheduled ON observation_runs (connection_id, scheduled_for)
  WHERE scheduled_for IS NOT NULL;
CREATE INDEX idx_observation_runs_connection ON observation_runs (connection_id, created_at DESC);

-- 每个对象类型一个发现子执行；attempt/evidence 与状态同事务冻结。
CREATE TABLE observation_run_objects (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  observation_run_id INTEGER NOT NULL REFERENCES observation_runs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  object_type        TEXT NOT NULL CHECK (length(object_type) > 0),
  attempt_id         INTEGER UNIQUE REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  evidence_id        INTEGER REFERENCES evidence(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  status             TEXT NOT NULL CHECK (status IN ('ok','error','gap')),
  gap_reason         TEXT CHECK (gap_reason IS NULL OR gap_reason IN (
                       'query_failed','partial_response','no_data','runtime_unavailable','plugin_unavailable','cancelled','interrupted')),
  result_digest      BLOB CHECK (result_digest IS NULL OR length(result_digest) = 32),
  warnings_json      TEXT CHECK (warnings_json IS NULL OR json_valid(warnings_json)),
  created_at         TEXT NOT NULL,
  UNIQUE (observation_run_id, object_type),
  CHECK (
    (status = 'ok' AND evidence_id IS NOT NULL AND gap_reason IS NULL)
    OR (status IN ('error','gap') AND gap_reason IS NOT NULL)
  ),
  CHECK (attempt_id IS NOT NULL OR result_digest IS NULL)
) STRICT;

-- 来源级观测对象：身份 = 接入 + 对象类型 + 规范来源身份；不依赖业务分组，
-- 跨来源同名对象不合并。只有同一冻结范围完整成功才能表达未再观测到。
CREATE TABLE observed_source_objects (
  id              INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  connection_id   INTEGER NOT NULL REFERENCES connections(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  object_type     TEXT NOT NULL,
  identity_key    TEXT NOT NULL,          -- 按 label 名排序的 identity label/value 规范编码（相等性权威）
  identity_digest TEXT CHECK (identity_digest IS NULL OR length(identity_digest) = 64),
  display_name    TEXT,
  labels_json     TEXT NOT NULL CHECK (json_valid(labels_json)),
  observed_at     TEXT,
  current         INTEGER NOT NULL DEFAULT 0 CHECK (current IN (0,1)),
  last_successful_refresh_at TEXT,
  stale           INTEGER NOT NULL DEFAULT 0 CHECK (stale IN (0,1)),
  created_at      TEXT NOT NULL,
  UNIQUE (connection_id, object_type, identity_key)
) STRICT;
CREATE INDEX idx_observed_source_objects_connection ON observed_source_objects (connection_id, object_type);

-- 业务视图（ADR-0004）：对来源接入范围与明确标签条件的可选组织；不拥有资源
-- 身份、凭据或额外权限。Run/计划冻结使用时的视图内容，视图修改不改写历史。
CREATE TABLE business_views (
  id                    INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  view_key              TEXT NOT NULL UNIQUE,  -- ^[a-z][a-z0-9-]{0,62}$，退役不复用
  display_name          TEXT NOT NULL,
  description           TEXT NOT NULL DEFAULT '',
  connection_id         INTEGER REFERENCES connections(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- NULL = 跨来源候选集合
  label_conditions_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(label_conditions_json) AND json_type(label_conditions_json) = 'object'),
  alert_source_keys_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(alert_source_keys_json) AND json_type(alert_source_keys_json) = 'array'), -- 明确 AM 告警源 key 约束；空数组 = 该视图不参与告警归属（与 Prom connection 身份严格区分）
  row_version           INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  created_by            INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at            TEXT NOT NULL,
  updated_at            TEXT NOT NULL
) STRICT;
-- 视图 key 是跨迁移稳定引用（退役不复用）：key 不可改写，行不可删除；
-- 内容演进走 row_version 前提下的整体提交更新。
CREATE TRIGGER trg_business_views_key_immutable BEFORE UPDATE OF view_key ON business_views
BEGIN SELECT RAISE(ABORT, 'business view key is immutable'); END;
CREATE TRIGGER trg_business_views_no_delete BEFORE DELETE ON business_views
BEGIN SELECT RAISE(ABORT, 'business views are never deleted; keys are retired, not reused'); END;

-- Label Contract 激活事件（DATA-CONFIG-002/006）：不可变单行承载 canonical items_json。
-- 单 INSERT 触发 AFTER INSERT 原子校验并切换全部系统指针、更新 label_contract_state、激活/退休契约。
-- 任一 RAISE(ABORT) 回滚该 INSERT 及全部副作用——结构性全有或全无，不存在“只切部分系统”的可提交状态。
CREATE TABLE label_contract_activations (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  contract_id               INTEGER NOT NULL UNIQUE REFERENCES label_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  expected_target_row_version INTEGER NOT NULL CHECK (expected_target_row_version >= 1),
  expected_state_row_version INTEGER NOT NULL CHECK (expected_state_row_version >= 1),
  expected_current_contract_id INTEGER REFERENCES label_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  items_json                TEXT NOT NULL CHECK (json_valid(items_json) AND json_type(items_json) = 'array'),
  applied_at                TEXT,
  created_at                TEXT NOT NULL
) STRICT;

-- ============================================================================
-- 8. 执行尝试、模型/工具调用、证据与报告
-- ============================================================================

CREATE TABLE execution_attempts (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  operation_correlation_id  TEXT,
  initiator_type            TEXT CHECK (initiator_type IS NULL OR initiator_type IN ('user','service','system')),
  initiator_id              INTEGER,
  attempt_type              TEXT NOT NULL CHECK (attempt_type IN
                              ('initial_analysis','investigation','inspection_analysis','knowledge_extraction','embedding',
                               'inspection_collection','connection_probe')),
  scope_type                TEXT NOT NULL CHECK (scope_type IN
                               ('analysis','investigation','run','knowledge_import_batch','embedding_generation','connection','run_check','observation_run')),
  scope_id                  INTEGER NOT NULL,
  check_key                 TEXT,   -- run_check 子 Attempt 非空；其它 scope 为空
  discovery_key             TEXT,   -- observation_run 子 Attempt 必填；其它 scope 为空
  state                     TEXT NOT NULL CHECK (state IN ('Queued','Assigned','Running','Cancelling','Succeeded','Failed','Cancelled','Interrupted')),
  row_version               INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  runtime_slot              TEXT CHECK (runtime_slot = 'plinth'), -- 派发绑定；一旦绑定不可改
  connection_epoch          INTEGER,
  boot_id                   TEXT,
  lease_until               TEXT,
  accepted_at               TEXT,
  started_at                TEXT,
  ended_at                  TEXT,
  quoin_release_version     TEXT NOT NULL,
  runtime_release_version   TEXT,
  agent_version             TEXT,
  termination_reason        TEXT CHECK (termination_reason IS NULL OR termination_reason IN
                              ('timeout','rate_limited','provider_unavailable','invalid_response','context_too_large','tool_error',
                               'artifact_commit_failed','artifact_body_expired','sandbox_unavailable','worker_protocol_error',
                               'cancelled','connection_disabled','business_system_disabled','lease_expired','replaced','revoked')),
  created_at                TEXT NOT NULL,
  CHECK (
    (state = 'Queued' AND runtime_slot IS NULL AND boot_id IS NULL AND connection_epoch IS NULL AND lease_until IS NULL AND accepted_at IS NULL AND runtime_release_version IS NULL)
    OR (state = 'Assigned' AND runtime_slot IS NOT NULL AND boot_id IS NOT NULL AND connection_epoch IS NOT NULL AND lease_until IS NOT NULL AND accepted_at IS NULL AND runtime_release_version IS NOT NULL)
    OR (state = 'Running' AND runtime_slot IS NOT NULL AND boot_id IS NOT NULL AND connection_epoch IS NOT NULL AND lease_until IS NOT NULL AND accepted_at IS NOT NULL AND runtime_release_version IS NOT NULL)
    -- A cancellation fence may win after dispatch binding but before Accept. The
    -- assigned Runtime can already be executing, so preserve its binding and
    -- allow CancelAttempt replay even though accepted_at has not arrived yet.
    OR (state = 'Cancelling' AND runtime_slot IS NOT NULL AND boot_id IS NOT NULL AND connection_epoch IS NOT NULL AND lease_until IS NOT NULL AND runtime_release_version IS NOT NULL)
    OR (state IN ('Succeeded','Failed','Cancelled','Interrupted') AND (
      (runtime_slot IS NULL AND boot_id IS NULL AND connection_epoch IS NULL AND lease_until IS NULL AND accepted_at IS NULL AND runtime_release_version IS NULL)
      OR (runtime_slot IS NOT NULL AND boot_id IS NOT NULL AND connection_epoch IS NOT NULL AND lease_until IS NOT NULL AND runtime_release_version IS NOT NULL)
    ))
  ),
  CHECK (connection_epoch IS NULL OR connection_epoch >= 1),
  CHECK (
    (scope_type = 'run_check' AND check_key IS NOT NULL AND discovery_key IS NULL)
    OR (scope_type = 'observation_run' AND check_key IS NULL AND discovery_key IS NOT NULL)
    OR (scope_type NOT IN ('run_check','observation_run') AND check_key IS NULL AND discovery_key IS NULL)
  ),
  CHECK (
    runtime_slot IS NULL
    OR (attempt_type = 'inspection_collection' AND scope_type IN ('run_check','observation_run') AND runtime_slot = 'plinth')
    OR (attempt_type IN ('initial_analysis','investigation','inspection_analysis','knowledge_extraction','embedding','connection_probe') AND runtime_slot = 'plinth')
  ),
  CHECK (
    (attempt_type = 'initial_analysis' AND scope_type = 'analysis')
    OR (attempt_type = 'investigation' AND scope_type = 'investigation')
    OR (attempt_type = 'inspection_analysis' AND scope_type = 'run')
    OR (attempt_type = 'knowledge_extraction' AND scope_type = 'knowledge_import_batch')
    OR (attempt_type = 'embedding' AND scope_type = 'embedding_generation')
    OR (attempt_type = 'connection_probe' AND scope_type = 'connection')
    OR (attempt_type = 'inspection_collection' AND scope_type IN ('run_check','observation_run'))
  )
) STRICT;
CREATE TRIGGER execution_attempts_correlation_immutable BEFORE UPDATE ON execution_attempts
WHEN OLD.operation_correlation_id IS NOT NULL AND (
 NEW.operation_correlation_id IS NOT OLD.operation_correlation_id
 OR NEW.initiator_type IS NOT OLD.initiator_type OR NEW.initiator_id IS NOT OLD.initiator_id)
BEGIN SELECT RAISE(ABORT, 'attempt operation association is immutable'); END;

CREATE UNIQUE INDEX ux_execution_attempt_active_scope ON execution_attempts (scope_type, scope_id)
  WHERE state IN ('Queued','Assigned','Running','Cancelling') AND check_key IS NULL
    AND scope_type <> 'observation_run';
CREATE UNIQUE INDEX ux_execution_attempt_active_run_check ON execution_attempts (scope_type, scope_id, check_key)
  WHERE scope_type = 'run_check' AND state IN ('Queued','Assigned','Running','Cancelling');
CREATE UNIQUE INDEX ux_execution_attempt_active_observation_object ON execution_attempts (scope_type, scope_id, discovery_key)
  WHERE scope_type = 'observation_run' AND state IN ('Queued','Assigned','Running','Cancelling');
CREATE INDEX idx_execution_attempts_scope ON execution_attempts (scope_type, scope_id);
CREATE INDEX idx_execution_attempts_lease ON execution_attempts (state, lease_until);

-- Attempt 创建时冻结的输入谱系；正文仍由各领域对象/Artifact 拥有。content_digest 覆盖由有序 items、
-- renderer_version 与固定 schema_kind 重建的 canonical JSON，不能只保存 digest 而丢失可解析引用。
CREATE TABLE attempt_input_snapshots (
  id               INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  attempt_id       INTEGER NOT NULL UNIQUE REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  schema_kind      TEXT NOT NULL,
  renderer_version TEXT NOT NULL,
  content_digest   TEXT NOT NULL CHECK (length(content_digest) = 64),
  -- inspection_analysis_v1 专用：冻结 Quoin 预分配的 Report 版本（结构化事实，非正文 digest）。
  inspection_report_version INTEGER CHECK (inspection_report_version IS NULL OR inspection_report_version >= 1),
  -- Agent 执行冻结的插件工具目录快照：完整参数/结果 Schema、工具版本、执行
  -- 位置与摘要，可供历史校验（绝不只存 name/version 的可漂移投影）。历史行
  -- 与非 Agent Attempt 为 NULL；content_digest 按既有语义覆盖目录正文。
  tool_catalog_json TEXT CHECK (tool_catalog_json IS NULL OR json_valid(tool_catalog_json)),
  created_at       TEXT NOT NULL
) STRICT;

CREATE TABLE attempt_input_items (
  id                                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  snapshot_id                       INTEGER NOT NULL REFERENCES attempt_input_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  item_seq                          INTEGER NOT NULL CHECK (item_seq >= 1),
  item_role      TEXT NOT NULL,
  source_digest  TEXT NOT NULL CHECK (length(source_digest) = 64),
  occurrence_id                     INTEGER REFERENCES alert_occurrences(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  initial_analysis_id               INTEGER REFERENCES initial_analyses(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  investigation_message_id          INTEGER REFERENCES investigation_messages(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  evidence_id                       INTEGER REFERENCES evidence(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  artifact_id                       INTEGER REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  knowledge_version_id              INTEGER REFERENCES knowledge_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  inspection_run_id                 INTEGER REFERENCES inspection_runs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  inspection_check_result_id        INTEGER REFERENCES inspection_check_results(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  source_material_id                INTEGER REFERENCES source_materials(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  business_system_config_version_id INTEGER REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  label_contract_version_id         INTEGER REFERENCES label_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  knowledge_import_batch_id         INTEGER REFERENCES knowledge_import_batches(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  embedding_generation_id           INTEGER REFERENCES embedding_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  connection_revision_id             INTEGER REFERENCES connection_revisions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  UNIQUE (snapshot_id, item_seq),
  CHECK (
    (occurrence_id IS NOT NULL) + (initial_analysis_id IS NOT NULL) + (investigation_message_id IS NOT NULL) +
    (evidence_id IS NOT NULL) + (artifact_id IS NOT NULL) + (knowledge_version_id IS NOT NULL) +
    (inspection_run_id IS NOT NULL) + (inspection_check_result_id IS NOT NULL) + (source_material_id IS NOT NULL) +
    (business_system_config_version_id IS NOT NULL) + (label_contract_version_id IS NOT NULL) +
    (knowledge_import_batch_id IS NOT NULL) + (embedding_generation_id IS NOT NULL) +
    (connection_revision_id IS NOT NULL) = 1
  )
) STRICT;

-- 行 id 是当前 Attempt/epoch 下 FetchCredentialGrant 使用的非秘密 locator；revision/generation 与用途
-- 是持久权威。绑定可在 Tool Call 持久化事务中追加，连接轮换不改写旧 binding。
CREATE TABLE attempt_connection_grants (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  attempt_id                INTEGER NOT NULL REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  purpose                   TEXT NOT NULL CHECK (purpose IN ('chat_model','embedding','thanos_query','config_thanos_query','model_probe_chat','model_probe_embedding','prometheus_probe','thanos_probe')),
  business_system_id        INTEGER REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  connection_id             INTEGER NOT NULL REFERENCES connections(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  connection_revision_id    INTEGER NOT NULL REFERENCES connection_revisions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  credential_generation_id  INTEGER NOT NULL REFERENCES credential_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  qualified_probe_result_id INTEGER REFERENCES connection_probe_results(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_by_tool_call_id    INTEGER REFERENCES tool_calls(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at                TEXT NOT NULL,
  CHECK (
      (-- Historic unscoped grants remain readable. New thanos_query inserts are
      -- fenced by trg_attempt_connection_grants_thanos_query_scope below.
       purpose = 'thanos_query' AND created_by_tool_call_id IS NOT NULL AND qualified_probe_result_id IS NULL)
      OR (purpose = 'config_thanos_query' AND business_system_id IS NULL AND created_by_tool_call_id IS NULL AND qualified_probe_result_id IS NULL)
      OR (purpose IN ('chat_model','embedding') AND business_system_id IS NULL AND created_by_tool_call_id IS NULL AND qualified_probe_result_id IS NOT NULL)
      OR (purpose IN ('model_probe_chat','model_probe_embedding','prometheus_probe','thanos_probe') AND business_system_id IS NULL AND created_by_tool_call_id IS NULL AND qualified_probe_result_id IS NULL))
) STRICT;
CREATE UNIQUE INDEX ux_attempt_connection_grant_binding ON attempt_connection_grants
  (attempt_id, purpose, connection_id, connection_revision_id, credential_generation_id, COALESCE(business_system_id, 0));
-- PromQL collection has one deterministic selected metrics locator. A credential
-- rotation must not leave a Queued Attempt with competing historical grants; it
-- must re-freeze a fresh Attempt. The legacy purpose name is wire-stable and
-- covers both Prometheus and Thanos connections.
CREATE UNIQUE INDEX ux_attempt_connection_grant_config_thanos_attempt ON attempt_connection_grants (attempt_id)
  WHERE purpose = 'config_thanos_query';

CREATE TABLE model_calls (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  attempt_id                 INTEGER NOT NULL REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  call_seq                   INTEGER NOT NULL CHECK (call_seq >= 1), -- logical call sequence
  retry_seq                  INTEGER NOT NULL DEFAULT 0 CHECK (retry_seq >= 0), -- physical request within logical call
  operation                  TEXT NOT NULL CHECK (operation IN ('chat','embedding')),
  model_id                   TEXT NOT NULL,
  connection_grant_id        INTEGER NOT NULL REFERENCES attempt_connection_grants(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  provider_request_id        TEXT,
  prompt_renderer_version    TEXT,
  agent_version              TEXT,
  prompt_digest              TEXT CHECK (prompt_digest IS NULL OR length(prompt_digest) = 64),
  tool_schema_version        TEXT,
  tool_schema_digest         TEXT CHECK (tool_schema_digest IS NULL OR length(tool_schema_digest) = 64),
  input_snapshot_digest      TEXT NOT NULL CHECK (length(input_snapshot_digest) = 64),
  rendered_request_digest    TEXT NOT NULL CHECK (length(rendered_request_digest) = 64),
  context_budget_tokens      INTEGER CHECK (context_budget_tokens IS NULL OR context_budget_tokens >= 1),
  max_output_tokens          INTEGER CHECK (max_output_tokens IS NULL OR (max_output_tokens >= 1 AND max_output_tokens < context_budget_tokens)),
  estimated_input_tokens     INTEGER NOT NULL CHECK (estimated_input_tokens >= 0),
  evicted_turn_count         INTEGER NOT NULL DEFAULT 0 CHECK (evicted_turn_count >= 0),
  usage_json                 TEXT CHECK (usage_json IS NULL OR json_valid(usage_json)),
  latency_ms                 INTEGER CHECK (latency_ms IS NULL OR latency_ms >= 0),
  status                     TEXT NOT NULL CHECK (status IN ('running','succeeded','failed','cancelled')),
  termination_reason         TEXT CHECK (termination_reason IS NULL OR termination_reason IN
                               ('timeout','rate_limited','transport_error','provider_unavailable','context_overflow','invalid_response',
                                 'artifact_commit_failed','cancelled')),
  started_at                 TEXT NOT NULL,
  ended_at                   TEXT,
  UNIQUE (attempt_id, call_seq, retry_seq),
  CHECK ((status = 'running' AND ended_at IS NULL AND termination_reason IS NULL)
      OR (status = 'succeeded' AND ended_at IS NOT NULL AND termination_reason IS NULL AND usage_json IS NOT NULL)
      OR (status IN ('failed','cancelled') AND ended_at IS NOT NULL AND termination_reason IS NOT NULL)),
  CHECK ((operation = 'chat' AND prompt_renderer_version IS NOT NULL AND agent_version IS NOT NULL
                         AND prompt_digest IS NOT NULL AND tool_schema_version IS NOT NULL AND tool_schema_digest IS NOT NULL
                         AND context_budget_tokens IS NOT NULL AND max_output_tokens IS NOT NULL)
      OR (operation = 'embedding' AND prompt_renderer_version IS NULL AND agent_version IS NULL
                               AND prompt_digest IS NULL AND tool_schema_version IS NULL AND tool_schema_digest IS NULL
                               AND context_budget_tokens IS NULL AND max_output_tokens IS NULL AND evicted_turn_count = 0))
) STRICT;

-- A terminal intent that cannot safely close its Investigation parent until all
-- Exploration processes have sealed and stopped. model_result retains the exact
-- natural proposal; recovery_loss freezes a no-model Interrupted diagnostic.
CREATE TABLE pending_attempt_terminals (
  attempt_id       INTEGER PRIMARY KEY REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  source           TEXT NOT NULL CHECK (source IN ('model_result','recovery_loss')),
  target_state     TEXT NOT NULL CHECK (target_state IN ('Succeeded','Failed','Interrupted')),
  model_call_id    INTEGER REFERENCES model_calls(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  proposal_json    TEXT,
  proposal_digest  BLOB CHECK (proposal_digest IS NULL OR length(proposal_digest) = 32),
  terminal_reason  TEXT,
  created_at       TEXT NOT NULL,
  CHECK ((source='model_result' AND target_state IN ('Succeeded','Failed')
          AND model_call_id IS NOT NULL AND proposal_json IS NOT NULL AND proposal_digest IS NOT NULL
          AND terminal_reason IS NULL)
      OR (source='recovery_loss' AND target_state='Interrupted'
          AND model_call_id IS NULL AND proposal_json IS NULL AND proposal_digest IS NULL
          AND terminal_reason IS NOT NULL))
) STRICT;
CREATE TRIGGER trg_pending_attempt_terminals_immutable BEFORE UPDATE ON pending_attempt_terminals
BEGIN SELECT RAISE(ABORT, 'pending attempt terminal is immutable'); END;
-- A user cancellation wins over a still-unacknowledged natural result. It
-- intentionally discards the incompatible pending proposal before the parent
-- enters Cancelling; any later replay is adjudicated as late.
CREATE TRIGGER trg_execution_attempts_cancelling_discards_pending BEFORE UPDATE OF state ON execution_attempts
WHEN OLD.state='Running' AND NEW.state='Cancelling'
BEGIN DELETE FROM pending_attempt_terminals WHERE attempt_id=OLD.id; END;

-- 每个物理模型请求的规范化响应审计；流式 delta 只用于实时投影，完成后在此封存组装结果。
CREATE TABLE model_call_outputs (
  model_call_id   INTEGER PRIMARY KEY REFERENCES model_calls(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  complete        INTEGER NOT NULL CHECK (complete IN (0,1)),
  response_json   TEXT NOT NULL CHECK (json_valid(response_json)),
  response_digest TEXT NOT NULL CHECK (length(response_digest) = 64),
  finish_reason   TEXT,
  created_at      TEXT NOT NULL
) STRICT;

CREATE TABLE model_call_input_items (
  id                       INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  model_call_id            INTEGER NOT NULL REFERENCES model_calls(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  item_seq                 INTEGER NOT NULL CHECK (item_seq >= 1),
  item_role                TEXT NOT NULL CHECK (item_role IN ('system','user','assistant','tool')),
  source_digest            TEXT NOT NULL CHECK (length(source_digest) = 64),
  attempt_input_snapshot_id INTEGER REFERENCES attempt_input_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  investigation_message_id INTEGER REFERENCES investigation_messages(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  prior_model_call_id      INTEGER REFERENCES model_calls(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  tool_call_id             INTEGER REFERENCES tool_calls(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  evidence_id              INTEGER REFERENCES evidence(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  artifact_id              INTEGER REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  knowledge_version_id     INTEGER REFERENCES knowledge_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  synthetic_kind           TEXT CHECK (synthetic_kind IS NULL OR synthetic_kind IN ('system_contract','tool_schema')),
  UNIQUE (model_call_id, item_seq),
  CHECK (
    (attempt_input_snapshot_id IS NOT NULL) + (investigation_message_id IS NOT NULL) + (prior_model_call_id IS NOT NULL) +
    (tool_call_id IS NOT NULL) + (evidence_id IS NOT NULL) + (artifact_id IS NOT NULL) +
    (knowledge_version_id IS NOT NULL) + (synthetic_kind IS NOT NULL) = 1
  )
) STRICT;

-- 每行代表一次不可改写的物理 Tool 执行；v1 不在 supervisor 内部自动重试 Tool。
-- 模型再次提出调用时必须形成新的 Model Call 与新的 provider Tool Call ID。
CREATE TABLE tool_calls (
  id                    INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  attempt_id            INTEGER NOT NULL REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  model_call_id         INTEGER NOT NULL REFERENCES model_calls(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  call_seq              INTEGER NOT NULL CHECK (call_seq >= 1),
  tool_index            INTEGER NOT NULL CHECK (tool_index >= 0),
  provider_tool_call_id TEXT NOT NULL,
  tool_name             TEXT NOT NULL,
  tool_version          TEXT NOT NULL,
  arguments_json        TEXT NOT NULL CHECK (json_valid(arguments_json) AND json_type(arguments_json) = 'object'),
  arguments_digest      TEXT NOT NULL CHECK (length(arguments_digest) = 64),
  execution_mode        TEXT NOT NULL CHECK (execution_mode IN ('worker_local','supervisor_typed')),
  failure_mode          TEXT NOT NULL CHECK (failure_mode IN ('return_to_model','fail_attempt')),
  status                TEXT NOT NULL CHECK (status IN ('pending','running','succeeded','failed','cancelled')),
  row_version           INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  result_json           TEXT CHECK (result_json IS NULL OR json_valid(result_json)), -- 有界模型可见预览/结构化结果
  result_artifact_id    INTEGER REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  error_detail          TEXT,
  preflight_error_code  TEXT CHECK (preflight_error_code IS NULL OR preflight_error_code IN ('target_not_found','target_ambiguous','no_mapping')),
  preflight_error_detail TEXT,
  created_at            TEXT NOT NULL,
  started_at            TEXT,
  ended_at              TEXT,
  CHECK ((preflight_error_code IS NULL AND preflight_error_detail IS NULL)
      OR (preflight_error_code IS NOT NULL AND preflight_error_detail IS NOT NULL AND length(preflight_error_detail) BETWEEN 1 AND 1024)),
  UNIQUE (attempt_id, call_seq, tool_index),
  UNIQUE (model_call_id, provider_tool_call_id),
  UNIQUE (model_call_id, tool_index),
  CHECK ((status = 'pending' AND started_at IS NULL AND ended_at IS NULL AND result_json IS NULL AND result_artifact_id IS NULL AND error_detail IS NULL)
      OR (status = 'running' AND started_at IS NOT NULL AND ended_at IS NULL AND result_json IS NULL AND result_artifact_id IS NULL AND error_detail IS NULL)
      OR (status = 'succeeded' AND started_at IS NOT NULL AND ended_at IS NOT NULL AND error_detail IS NULL
                              AND (result_json IS NOT NULL OR result_artifact_id IS NOT NULL))
      OR (status = 'failed' AND started_at IS NOT NULL AND ended_at IS NOT NULL AND error_detail IS NOT NULL
                            AND ((failure_mode = 'return_to_model' AND result_json IS NOT NULL AND result_artifact_id IS NULL)
                              OR (failure_mode = 'fail_attempt' AND result_json IS NULL AND result_artifact_id IS NULL)))
      OR (status = 'cancelled' AND ended_at IS NOT NULL AND error_detail IS NOT NULL
                               AND result_json IS NULL AND result_artifact_id IS NULL))
) STRICT;

-- Normalized execution arguments are distinct from immutable model-proposed
-- arguments. This is the audit record for scope injection after authorization.
CREATE TABLE tool_call_execution_inputs (
  tool_call_id      INTEGER PRIMARY KEY REFERENCES tool_calls(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  arguments_json    TEXT NOT NULL CHECK (json_valid(arguments_json) AND json_type(arguments_json) = 'object'),
  arguments_digest  TEXT NOT NULL CHECK (length(arguments_digest) = 64 AND arguments_digest NOT GLOB '*[^0-9a-f]*'),
  created_at        TEXT NOT NULL
) STRICT;

CREATE TABLE tool_call_connection_grants (
  tool_call_id       INTEGER NOT NULL REFERENCES tool_calls(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  connection_grant_id INTEGER NOT NULL REFERENCES attempt_connection_grants(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  ordinal            INTEGER NOT NULL CHECK (ordinal >= 0),
  PRIMARY KEY (tool_call_id, connection_grant_id),
  UNIQUE (tool_call_id, ordinal)
) WITHOUT ROWID, STRICT;

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
  attempt_id     INTEGER NOT NULL UNIQUE REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  evidence_digest TEXT NOT NULL CHECK (length(evidence_digest) = 64),
  model_id       TEXT NOT NULL,
  prompt_digest  TEXT CHECK (prompt_digest IS NULL OR length(prompt_digest) = 64),
  content        TEXT NOT NULL,
  created_at     TEXT NOT NULL,
  UNIQUE (run_id, version)
) STRICT;

-- 模型输出正文由领域记录拥有；下列有序引用保存输出声明使用的精确 Evidence/Artifact/KnowledgeVersion。
CREATE TABLE initial_analysis_output_evidence (
  output_id   INTEGER NOT NULL REFERENCES initial_analysis_outputs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  evidence_id INTEGER NOT NULL REFERENCES evidence(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  ordinal     INTEGER NOT NULL CHECK (ordinal >= 0),
  PRIMARY KEY (output_id, evidence_id),
  UNIQUE (output_id, ordinal)
) WITHOUT ROWID, STRICT;
CREATE TABLE initial_analysis_output_artifacts (
  output_id   INTEGER NOT NULL REFERENCES initial_analysis_outputs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  artifact_id INTEGER NOT NULL REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  ordinal     INTEGER NOT NULL CHECK (ordinal >= 0),
  PRIMARY KEY (output_id, artifact_id),
  UNIQUE (output_id, ordinal)
) WITHOUT ROWID, STRICT;
CREATE TABLE initial_analysis_output_knowledge_versions (
  output_id            INTEGER NOT NULL REFERENCES initial_analysis_outputs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  knowledge_version_id INTEGER NOT NULL REFERENCES knowledge_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  ordinal              INTEGER NOT NULL CHECK (ordinal >= 0),
  PRIMARY KEY (output_id, knowledge_version_id),
  UNIQUE (output_id, ordinal)
) WITHOUT ROWID, STRICT;
CREATE TABLE investigation_message_evidence (
  message_id  INTEGER NOT NULL REFERENCES investigation_messages(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  evidence_id INTEGER NOT NULL REFERENCES evidence(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  ordinal     INTEGER NOT NULL CHECK (ordinal >= 0),
  PRIMARY KEY (message_id, evidence_id),
  UNIQUE (message_id, ordinal)
) WITHOUT ROWID, STRICT;
CREATE TABLE investigation_message_artifacts (
  message_id  INTEGER NOT NULL REFERENCES investigation_messages(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  artifact_id INTEGER NOT NULL REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  ordinal     INTEGER NOT NULL CHECK (ordinal >= 0),
  PRIMARY KEY (message_id, artifact_id),
  UNIQUE (message_id, ordinal)
) WITHOUT ROWID, STRICT;
CREATE TABLE investigation_message_knowledge_versions (
  message_id           INTEGER NOT NULL REFERENCES investigation_messages(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  knowledge_version_id INTEGER NOT NULL REFERENCES knowledge_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  ordinal              INTEGER NOT NULL CHECK (ordinal >= 0),
  PRIMARY KEY (message_id, knowledge_version_id),
  UNIQUE (message_id, ordinal)
) WITHOUT ROWID, STRICT;
CREATE TABLE inspection_report_evidence (
  report_id   INTEGER NOT NULL REFERENCES inspection_reports(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  evidence_id INTEGER NOT NULL REFERENCES evidence(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  ordinal     INTEGER NOT NULL CHECK (ordinal >= 0),
  PRIMARY KEY (report_id, evidence_id),
  UNIQUE (report_id, ordinal)
) WITHOUT ROWID, STRICT;
CREATE TABLE inspection_report_artifacts (
  report_id   INTEGER NOT NULL REFERENCES inspection_reports(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  artifact_id INTEGER NOT NULL REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  ordinal     INTEGER NOT NULL CHECK (ordinal >= 0),
  PRIMARY KEY (report_id, artifact_id),
  UNIQUE (report_id, ordinal)
) WITHOUT ROWID, STRICT;
CREATE TABLE inspection_report_knowledge_versions (
  report_id            INTEGER NOT NULL REFERENCES inspection_reports(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  knowledge_version_id INTEGER NOT NULL REFERENCES knowledge_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  ordinal              INTEGER NOT NULL CHECK (ordinal >= 0),
  PRIMARY KEY (report_id, knowledge_version_id),
  UNIQUE (report_id, ordinal)
) WITHOUT ROWID, STRICT;

-- Report ResultProposal 的不可变重放账本。唯一入口由 AFTER INSERT trigger
-- 在同一外层事务创建 Report、全部有序引用及 Attempt 成功终态。
CREATE TABLE inspection_report_result_ledgers (
  attempt_id                 INTEGER PRIMARY KEY REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  inspection_run_id          INTEGER NOT NULL REFERENCES inspection_runs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  report_version             INTEGER NOT NULL CHECK (report_version >= 1),
  model_call_id              INTEGER NOT NULL REFERENCES model_calls(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  result_digest              BLOB NOT NULL CHECK (length(result_digest) = 32),
  evidence_digest            TEXT NOT NULL CHECK (length(evidence_digest) = 64 AND evidence_digest NOT GLOB '*[^0-9a-f]*'),
  content                    TEXT NOT NULL CHECK (length(content) BETWEEN 1 AND 1000000),
  prompt_digest              TEXT NOT NULL CHECK (length(prompt_digest) = 64 AND prompt_digest NOT GLOB '*[^0-9a-f]*'),
  evidence_ids_json          TEXT NOT NULL CHECK (json_valid(evidence_ids_json) AND json_type(evidence_ids_json) = 'array'),
  artifact_ids_json          TEXT NOT NULL CHECK (json_valid(artifact_ids_json) AND json_type(artifact_ids_json) = 'array'),
  knowledge_version_ids_json TEXT NOT NULL CHECK (json_valid(knowledge_version_ids_json) AND json_type(knowledge_version_ids_json) = 'array'),
  created_at                 TEXT NOT NULL
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
  kind           TEXT NOT NULL CHECK (kind IN ('attachment','tool_result','report_file','verification_bundle','verification_attachment')),
  media_type     TEXT NOT NULL,
  sensitive      INTEGER NOT NULL DEFAULT 0 CHECK (sensitive IN (0,1)), -- raw trace 固定 sensitive=1
  retention_kind TEXT NOT NULL CHECK (retention_kind IN ('long_term','generated')),
  owner_type     TEXT NOT NULL,
  owner_id       INTEGER NOT NULL,
  expires_at     TEXT,
  body_expired   INTEGER NOT NULL DEFAULT 0 CHECK (body_expired IN (0,1)),
  created_at     TEXT NOT NULL,
  created_by     INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CHECK (kind <> 'trace' OR sensitive = 1),
  CHECK ((retention_kind = 'generated' AND expires_at IS NOT NULL) OR (retention_kind = 'long_term' AND expires_at IS NULL))
) STRICT;
CREATE INDEX idx_artifacts_owner ON artifacts (owner_type, owner_id);
CREATE INDEX idx_artifacts_blob ON artifacts (blob_id);

-- Runtime Artifact 上传 ledger：上传身份与重试幂等权威（DATA-ARTIFACT-006）。upload_id 由
-- Runtime 生成并在整个重试生命周期保持稳定；同 upload_id 同摘要重试返回原 artifact_id，
-- 同 upload_id 不同摘要/owner 冲突拒绝；v1 整单重传，不做 offset 续传。
CREATE TABLE runtime_artifact_uploads (
  id             INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  upload_id      TEXT NOT NULL UNIQUE,
  attempt_id     INTEGER REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  boot_id        TEXT NOT NULL,  -- 与 Attempt 派发绑定的 boot_id；不可改（DATA-ARTIFACT-006）
  connection_epoch INTEGER NOT NULL CHECK (connection_epoch >= 1), -- 旧 epoch 上传只审计、拒绝提交
  owner_type     TEXT NOT NULL,
  owner_id       INTEGER NOT NULL,
  kind           TEXT NOT NULL CHECK (kind IN ('attachment','tool_result','report_file')),
  media_type     TEXT NOT NULL,
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
  CHECK (attempt_id IS NOT NULL OR owner_type <> 'tool_call')
) STRICT;
CREATE INDEX idx_runtime_artifact_uploads_attempt ON runtime_artifact_uploads (attempt_id, created_at);

-- 当前 Attempt 对 Artifact 正文读取的不可变授权；来源可为冻结输入或同 Attempt 已提交 Tool Result。
-- 到期 GC 只把 artifacts.body_expired 置 1，不删除本授权或调用谱系。
CREATE TABLE attempt_artifact_grants (
  attempt_id   INTEGER NOT NULL REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  artifact_id  INTEGER NOT NULL REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  source_kind  TEXT NOT NULL CHECK (source_kind IN ('input_snapshot','tool_result','evidence')),
  source_id    INTEGER NOT NULL,
  granted_at   TEXT NOT NULL,
  PRIMARY KEY (attempt_id, artifact_id)
) WITHOUT ROWID, STRICT;
CREATE INDEX idx_attempt_artifact_grants_source ON attempt_artifact_grants (source_kind, source_id);

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
  source_type             TEXT NOT NULL CHECK (source_type IN ('initial_analysis_output','inspection_report','investigation_message','source_material','knowledge_version')),
  source_id               INTEGER NOT NULL,
  target_knowledge_id     INTEGER REFERENCES reusable_knowledge(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  generation              INTEGER NOT NULL DEFAULT 1 CHECK (generation >= 1),
  state                   TEXT NOT NULL CHECK (state IN ('AwaitingConfirmation','Confirmed','Excluded','Superseded','SourceInvalid')),
  row_version             INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  original_suggestion_json TEXT NOT NULL CHECK (json_valid(original_suggestion_json)), -- 模型原始建议不可变
  draft_title             TEXT,
  draft_body              TEXT,
  draft_scope_json        TEXT CHECK (draft_scope_json IS NULL OR json_valid(draft_scope_json)), -- 适用范围草稿（UI-KNOWLEDGE-003）；确认时写入版本的 scope_json
  draft_revision          INTEGER NOT NULL DEFAULT 0 CHECK (draft_revision >= 0),
  confirmed_knowledge_id  INTEGER REFERENCES reusable_knowledge(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_by              INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at              TEXT NOT NULL,
  CHECK (
    (source_type = 'source_material' AND import_batch_id IS NOT NULL AND target_knowledge_id IS NULL)
    OR (source_type IN ('initial_analysis_output','inspection_report','investigation_message') AND import_batch_id IS NULL AND target_knowledge_id IS NULL)
    OR (source_type = 'knowledge_version' AND import_batch_id IS NULL AND target_knowledge_id IS NOT NULL)
  ),
  CHECK (confirmed_knowledge_id IS NULL OR target_knowledge_id IS NULL OR confirmed_knowledge_id = target_knowledge_id)
) STRICT;
CREATE INDEX idx_knowledge_candidates_batch ON knowledge_candidates (import_batch_id);
CREATE INDEX idx_knowledge_candidates_source ON knowledge_candidates (source_type, source_id);
CREATE UNIQUE INDEX ux_knowledge_candidate_single_source
  ON knowledge_candidates (source_type, source_id)
  WHERE source_type <> 'source_material'
    AND (source_type <> 'knowledge_version' OR state IN ('AwaitingConfirmation','Confirmed'));

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
  source_candidate_id INTEGER NOT NULL UNIQUE REFERENCES knowledge_candidates(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
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
  CHECK (state <> 'current' OR (vector_dim IS NOT NULL AND built_at IS NOT NULL AND validated_at IS NOT NULL))
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
  note        TEXT CHECK (note IS NULL OR length(note) <= 4096),
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
  retention_count      INTEGER NOT NULL DEFAULT 30 CHECK (retention_count >= 1),
  -- 唯一的 durable catch-up anchor；只在 enabled 状态转换时变更（OPS-BACKUP-002）。
  schedule_enabled_at  TEXT,
  row_version          INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- 设置更新并发前提（DATA-BACKUP-008）
  updated_by           INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  updated_at           TEXT NOT NULL,
  CHECK ((enabled = 1 AND schedule_enabled_at IS NOT NULL) OR (enabled = 0 AND schedule_enabled_at IS NULL))
) STRICT;

-- Backup Run 是可恢复的受限状态机：HTTP 202 先插入 queued，执行器再推进 running，
-- 终态 succeeded|failed 不可改写。启动调和把遗留 queued|running 显式推进 failed（OPS-BACKUP-001..004）。
CREATE TABLE artifact_retention_settings (
  id                       INTEGER PRIMARY KEY CHECK (id = 1),
  generated_retention_days INTEGER NOT NULL DEFAULT 90 CHECK (generated_retention_days >= 1),
  row_version              INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  updated_by               INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  updated_at               TEXT NOT NULL
) STRICT;

-- Singleton, independent of Backup Run lifecycle: retention failures preserve a
-- valid newest snapshot but remain durable, visible and retryable.
CREATE TABLE backup_retention_health (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  last_attempt_at TEXT,
  last_failure_at TEXT,
  error_detail    TEXT CHECK (error_detail IS NULL OR (length(error_detail) BETWEEN 1 AND 4096)),
  CHECK ((last_failure_at IS NULL AND error_detail IS NULL) OR (last_failure_at IS NOT NULL AND error_detail IS NOT NULL))
) STRICT;

CREATE TABLE backups (
  correlation_id TEXT,
  initiator_type TEXT CHECK (initiator_type IS NULL OR initiator_type IN ('user','service','system')),
  id              INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  status          TEXT NOT NULL CHECK (status IN ('queued','running','succeeded','failed')),
  stage           TEXT NOT NULL CHECK (stage IN ('queued','preflight','database_snapshot','artifact_copy','manifest_publish','completed')),
  trigger_kind    TEXT NOT NULL CHECK (trigger_kind IN ('manual','scheduled','upgrade')),
  execution_mode  TEXT NOT NULL CHECK (execution_mode IN ('online','offline')),
  scheduled_for   TEXT,  -- UTC 计划边界；仅 scheduled 非空，用于停机 catch-up 幂等
  db_sha256       TEXT CHECK (db_sha256 IS NULL OR length(db_sha256) = 64),
  manifest_sha256 TEXT CHECK (manifest_sha256 IS NULL OR length(manifest_sha256) = 64),
  artifact_count  INTEGER CHECK (artifact_count IS NULL OR artifact_count >= 0),
  -- Sum of every manifest-listed archive-set member (manifest.json, quoin.db,
  -- and copied artifacts), excluding tar framing; zero before successful publication.
  size_bytes      INTEGER NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
  manifest_path   TEXT,
  error_code      TEXT CHECK (error_code IS NULL OR (length(error_code) BETWEEN 1 AND 128)),
  retryable       INTEGER CHECK (retryable IS NULL OR retryable IN (0,1)),
  error_detail    TEXT CHECK (error_detail IS NULL OR (length(error_detail) BETWEEN 1 AND 4096)),
  row_version     INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  started_at      TEXT,
  completed_at    TEXT,
  triggered_by    INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CHECK (
    (trigger_kind = 'manual' AND execution_mode = 'online' AND scheduled_for IS NULL AND triggered_by IS NOT NULL)
    OR (trigger_kind = 'manual' AND execution_mode = 'offline' AND scheduled_for IS NULL AND triggered_by IS NULL)
    OR (trigger_kind = 'scheduled' AND execution_mode = 'online' AND scheduled_for IS NOT NULL AND triggered_by IS NULL)
    OR (trigger_kind = 'upgrade' AND execution_mode = 'online' AND scheduled_for IS NULL AND triggered_by IS NOT NULL)
  ),
  CHECK (
    (status = 'queued' AND stage = 'queued' AND started_at IS NULL AND completed_at IS NULL
      AND db_sha256 IS NULL AND manifest_sha256 IS NULL AND artifact_count IS NULL AND size_bytes = 0 AND manifest_path IS NULL
      AND error_code IS NULL AND retryable IS NULL AND error_detail IS NULL)
    OR
    (status = 'running' AND stage IN ('preflight','database_snapshot','artifact_copy','manifest_publish')
      AND started_at IS NOT NULL AND completed_at IS NULL
      AND db_sha256 IS NULL AND manifest_sha256 IS NULL AND artifact_count IS NULL AND size_bytes = 0 AND manifest_path IS NULL
      AND error_code IS NULL AND retryable IS NULL AND error_detail IS NULL)
    OR
    (status = 'succeeded' AND stage = 'completed' AND started_at IS NOT NULL AND completed_at IS NOT NULL
      AND db_sha256 IS NOT NULL AND manifest_sha256 IS NOT NULL AND artifact_count IS NOT NULL AND size_bytes > 0 AND manifest_path IS NOT NULL
      AND error_code IS NULL AND retryable IS NULL AND error_detail IS NULL)
    OR
    (status = 'failed' AND stage <> 'completed' AND completed_at IS NOT NULL AND db_sha256 IS NULL AND manifest_sha256 IS NULL
      AND artifact_count IS NULL AND size_bytes = 0 AND manifest_path IS NULL AND error_code IS NOT NULL AND retryable IS NOT NULL AND error_detail IS NOT NULL)
  )
) STRICT;
CREATE INDEX idx_backups_created ON backups (created_at);
CREATE UNIQUE INDEX ux_backups_active ON backups ((1)) WHERE status IN ('queued','running');
CREATE UNIQUE INDEX ux_backups_scheduled_for ON backups (scheduled_for) WHERE trigger_kind = 'scheduled';

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

-- 恢复、协调升级与根密钥 rebind 共用的维护聚合。
CREATE TABLE maintenance_state (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  active          INTEGER NOT NULL CHECK (active IN (0,1)),
  reason          TEXT CHECK (reason IS NULL OR reason IN ('Restore','Upgrade','RootKeyRebind')),
  row_version     INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  entered_at      TEXT,
  entered_by_type TEXT CHECK (entered_by_type IS NULL OR entered_by_type IN ('user','system')),
  entered_by_id   INTEGER,
  exited_at       TEXT,
  exited_by_type  TEXT CHECK (exited_by_type IS NULL OR exited_by_type IN ('user','system')),
  exited_by_id    INTEGER,
  CHECK ((active = 1 AND reason IS NOT NULL AND entered_at IS NOT NULL AND entered_by_type IS NOT NULL AND exited_at IS NULL AND exited_by_type IS NULL AND exited_by_id IS NULL)
      OR (active = 0 AND reason IS NULL AND entered_at IS NULL AND entered_by_type IS NULL AND entered_by_id IS NULL))
) STRICT;

CREATE TABLE maintenance_items (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  maintenance_revision INTEGER NOT NULL CHECK (maintenance_revision >= 1),
  kind                 TEXT NOT NULL CHECK (kind IN ('AdminPassword','User','Connection','AlertSource','ActiveAttempt','BackupPreflight','SchemaMigration','ReleaseVersion','Integrity','SearchProjection')),
  object_key           TEXT NOT NULL CHECK (length(object_key) BETWEEN 1 AND 256),
  safe_state           TEXT NOT NULL CHECK (safe_state IN ('Safe','Blocking')),
  detail_code          TEXT NOT NULL CHECK (length(detail_code) BETWEEN 1 AND 128),
  updated_at           TEXT NOT NULL,
  UNIQUE (maintenance_revision, kind, object_key)
) STRICT;
CREATE INDEX idx_maintenance_items_state ON maintenance_items (maintenance_revision, safe_state, kind);

-- ============================================================================
-- 12. 触发器（机器可表达的不变量）
-- ============================================================================

-- 12.1 追加表：禁止 UPDATE / DELETE（持久历史不可改写）
CREATE TRIGGER trg_audit_events_no_update BEFORE UPDATE ON audit_events
BEGIN SELECT RAISE(ABORT, 'audit_events is append-only'); END;
CREATE TRIGGER trg_audit_events_no_delete BEFORE DELETE ON audit_events
WHEN NOT EXISTS (
  SELECT 1 FROM audit_cleanup_permits p JOIN audit_retention r ON r.id=1
  WHERE p.id=1 AND p.active=1 AND r.cleanup_enabled=1
    AND julianday(OLD.created_at)<julianday(p.cutoff_at) AND OLD.id<=p.upper_event_id
)
BEGIN SELECT RAISE(ABORT, 'audit deletion requires an active retention permit'); END;
CREATE TRIGGER trg_audit_event_targets_no_update BEFORE UPDATE ON audit_event_targets
BEGIN SELECT RAISE(ABORT, 'audit_event_targets is append-only'); END;
CREATE TRIGGER trg_audit_event_targets_no_delete BEFORE DELETE ON audit_event_targets
WHEN NOT EXISTS (
 SELECT 1 FROM audit_cleanup_permits p JOIN audit_retention r ON r.id=1
 JOIN audit_events e ON e.id=OLD.audit_event_id
 WHERE p.id=1 AND p.active=1 AND r.cleanup_enabled=1
   AND julianday(e.created_at)<julianday(p.cutoff_at) AND e.id<=p.upper_event_id
)
BEGIN SELECT RAISE(ABORT, 'audit target deletion requires an active retention permit'); END;
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
-- New model tool grants must bind the immutable Business System configuration
-- item carried by this Attempt's input snapshot. A later publish cannot change
-- that approved snapshot route. An attributed Investigation additionally proves
-- its occurrence belongs to the granted business. A source-free direct
-- Investigation is allowed only when it has the same exact config/contract
-- pair, uses that pair's selected metrics connection, and is truly direct
-- (rather than an unrelated source lineage). Existing nullable historic grants
-- remain immutable/readable but cannot be newly inserted or executed.
CREATE TRIGGER trg_attempt_connection_grants_thanos_query_scope BEFORE INSERT ON attempt_connection_grants
WHEN NEW.purpose = 'thanos_query' AND NEW.business_system_id IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM attempt_input_snapshots snapshot
  JOIN attempt_input_items config_item ON config_item.snapshot_id=snapshot.id
  JOIN business_system_config_versions v ON v.id=config_item.business_system_config_version_id
  WHERE snapshot.attempt_id=NEW.attempt_id
    AND config_item.item_role IN ('business_config','config_version')
    AND v.business_system_id=NEW.business_system_id
    AND v.metrics_connection_id=NEW.connection_id
    AND (
      EXISTS (
        SELECT 1 FROM attempt_input_items occurrence_item
        JOIN alert_occurrences occurrence ON occurrence.id=occurrence_item.occurrence_id
        WHERE occurrence_item.snapshot_id=snapshot.id
          AND occurrence_item.item_role='occurrence'
          AND occurrence.business_system_id=NEW.business_system_id
      )
      OR (
        NOT EXISTS (
          SELECT 1 FROM attempt_input_items occurrence_item
          WHERE occurrence_item.snapshot_id=snapshot.id
            AND occurrence_item.occurrence_id IS NOT NULL
        )
        AND EXISTS (
          SELECT 1 FROM execution_attempts attempt
          WHERE attempt.id=NEW.attempt_id
            AND attempt.attempt_type='investigation'
            AND attempt.scope_type='investigation'
            AND NOT EXISTS (
              SELECT 1 FROM investigation_source_links source
              WHERE source.investigation_id=attempt.scope_id
            )
        )
      )
    )
)
BEGIN SELECT RAISE(ABORT, 'metrics query grant requires an attributed or direct investigation frozen business config and selected metrics connection'); END;
-- 无业务归属（business_system_id NULL）的 thanos_query 只有在 Attempt 快照冻结了
-- 对应 metrics_source 来源项、且被授 revision 恰为该来源当前启用修订时才可创建；
-- 不按"NULL 即全来源放行"处理。
CREATE TRIGGER trg_attempt_connection_grants_thanos_query_source BEFORE INSERT ON attempt_connection_grants
WHEN NEW.purpose = 'thanos_query' AND NEW.business_system_id IS NULL AND NOT EXISTS (
  SELECT 1 FROM attempt_input_snapshots snapshot
  JOIN attempt_input_items source_item ON source_item.snapshot_id=snapshot.id
    AND source_item.item_role='metrics_source'
    AND source_item.connection_revision_id=NEW.connection_revision_id
  JOIN connection_revisions r ON r.id=NEW.connection_revision_id
  JOIN connections c ON c.id=r.connection_id AND c.id=NEW.connection_id
    AND c.enabled=1 AND c.current_revision_id=r.id
  WHERE snapshot.attempt_id=NEW.attempt_id
)
BEGIN SELECT RAISE(ABORT, 'standalone metrics query grant requires its exact frozen metrics_source revision, currently enabled'); END;
CREATE TRIGGER trg_attempt_connection_grants_config_thanos_closure BEFORE INSERT ON attempt_connection_grants
WHEN NEW.purpose = 'config_thanos_query' AND NOT EXISTS (
  SELECT 1 FROM execution_attempts a
  WHERE a.id = NEW.attempt_id
    AND a.attempt_type = 'inspection_collection'
    AND a.state = 'Queued'
    AND (
      a.scope_type = 'observation_run'
      OR (a.scope_type = 'run_check' AND EXISTS (
        SELECT 1 FROM inspection_runs r
        JOIN config_plans p ON p.config_version_id = r.config_version_id AND p.plan_key = r.plan_key
        JOIN config_checks c ON c.plan_id = p.id AND c.check_key = a.check_key
        WHERE r.id = a.scope_id AND r.state = 'Running' AND c.kind = 'promql'
      ))
      OR (a.scope_type = 'run_check' AND EXISTS (
        SELECT 1 FROM inspection_runs r
        JOIN inspection_run_checks c ON c.run_id = r.id AND c.check_key = a.check_key
        WHERE r.id = a.scope_id AND r.state = 'Running' AND r.plan_id IS NOT NULL
      ))
    )
)
BEGIN SELECT RAISE(ABORT, 'config metrics query grant requires one Queued Observation or Running Inspection Run collection Attempt'); END;
CREATE TRIGGER trg_evidence_no_update BEFORE UPDATE ON evidence
BEGIN SELECT RAISE(ABORT, 'evidence is append-only'); END;
CREATE TRIGGER trg_evidence_no_delete BEFORE DELETE ON evidence
BEGIN SELECT RAISE(ABORT, 'evidence is append-only'); END;
CREATE TRIGGER trg_evidence_attempt_tool_closure BEFORE INSERT ON evidence
WHEN (NEW.tool_call_id IS NOT NULL AND NEW.attempt_id IS NULL)
   OR (NEW.tool_call_id IS NOT NULL AND NOT EXISTS (
     SELECT 1 FROM execution_attempts a
     JOIN tool_calls t ON t.attempt_id = a.id
     WHERE a.id = NEW.attempt_id AND t.id = NEW.tool_call_id
       AND a.state = 'Running' AND t.status = 'running'
   ))
   OR (NEW.attempt_id IS NOT NULL AND NEW.tool_call_id IS NULL AND NOT EXISTS (
     SELECT 1 FROM execution_attempts a
     WHERE a.id = NEW.attempt_id AND a.state = 'Running'
       AND a.accepted_at IS NOT NULL
       AND (
         a.runtime_slot = 'plinth' AND a.attempt_type = 'inspection_collection'
             AND a.scope_type IN ('run_check','observation_run')
       )
   ))
BEGIN SELECT RAISE(ABORT, 'Evidence must be Quoin-local, close to one same Running Attempt and running Tool Call, or close to one accepted Runtime collection Attempt'); END;
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
CREATE TRIGGER trg_diagnosis_feedback_target_insert AFTER INSERT ON diagnosis_feedback
WHEN NOT (
  (NEW.target_type = 'initial_analysis_output' AND EXISTS (
    SELECT 1 FROM initial_analysis_outputs o WHERE o.id = NEW.target_id
  ))
  OR (NEW.target_type = 'inspection_report' AND EXISTS (
    SELECT 1 FROM inspection_reports r WHERE r.id = NEW.target_id
  ))
  OR (NEW.target_type = 'investigation_message' AND EXISTS (
    SELECT 1 FROM investigation_messages m WHERE m.id = NEW.target_id AND m.role = 'assistant'
  ))
)
BEGIN SELECT RAISE(ABORT, 'diagnosis feedback target must be an immutable diagnosis output or assistant message'); END;
CREATE TRIGGER trg_text_attachments_no_update BEFORE UPDATE ON text_attachments
BEGIN SELECT RAISE(ABORT, 'text_attachments is append-only'); END;
CREATE TRIGGER trg_text_attachments_no_delete BEFORE DELETE ON text_attachments
BEGIN SELECT RAISE(ABORT, 'text_attachments is append-only'); END;
CREATE TRIGGER trg_investigation_message_attachments_user_message AFTER INSERT ON investigation_message_attachments
WHEN NOT EXISTS (
  SELECT 1 FROM investigation_messages m
  WHERE m.id = NEW.message_id AND m.role = 'user' AND m.status = 'active'
)
BEGIN SELECT RAISE(ABORT, 'text attachments may reference only user messages'); END;
CREATE TRIGGER trg_investigation_message_attachments_no_update BEFORE UPDATE ON investigation_message_attachments
BEGIN SELECT RAISE(ABORT, 'investigation_message_attachments is append-only'); END;
CREATE TRIGGER trg_investigation_message_attachments_no_delete BEFORE DELETE ON investigation_message_attachments
BEGIN SELECT RAISE(ABORT, 'investigation_message_attachments history is not deletable'); END;
CREATE TRIGGER trg_source_materials_no_update BEFORE UPDATE ON source_materials
BEGIN SELECT RAISE(ABORT, 'source_materials is append-only'); END;
CREATE TRIGGER trg_source_materials_no_delete BEFORE DELETE ON source_materials
BEGIN SELECT RAISE(ABORT, 'source_materials is append-only'); END;
CREATE TRIGGER trg_investigation_source_links_creation_only BEFORE INSERT ON investigation_source_links
WHEN NOT EXISTS (
  SELECT 1 FROM investigations i WHERE i.id = NEW.investigation_id AND i.created_at = NEW.linked_at
    AND NOT EXISTS (SELECT 1 FROM investigation_messages m WHERE m.investigation_id = i.id)
)
BEGIN SELECT RAISE(ABORT, 'investigation source links may only be frozen before the first Chat message'); END;
CREATE TRIGGER trg_investigation_source_links_no_update BEFORE UPDATE ON investigation_source_links
BEGIN SELECT RAISE(ABORT, 'investigation_source_links is append-only'); END;
CREATE TRIGGER trg_investigation_source_links_no_delete BEFORE DELETE ON investigation_source_links
BEGIN SELECT RAISE(ABORT, 'investigation_source_links is append-only'); END;
CREATE TRIGGER trg_attempt_input_snapshots_no_update BEFORE UPDATE ON attempt_input_snapshots
BEGIN SELECT RAISE(ABORT, 'attempt_input_snapshots is append-only'); END;
CREATE TRIGGER trg_attempt_input_snapshots_no_delete BEFORE DELETE ON attempt_input_snapshots
BEGIN SELECT RAISE(ABORT, 'attempt_input_snapshots is append-only'); END;
CREATE TRIGGER trg_attempt_input_items_no_update BEFORE UPDATE ON attempt_input_items
BEGIN SELECT RAISE(ABORT, 'attempt_input_items is append-only'); END;
CREATE TRIGGER trg_attempt_input_items_no_delete BEFORE DELETE ON attempt_input_items
BEGIN SELECT RAISE(ABORT, 'attempt_input_items is append-only'); END;
CREATE TRIGGER trg_attempt_connection_grants_no_update BEFORE UPDATE ON attempt_connection_grants
BEGIN SELECT RAISE(ABORT, 'attempt_connection_grants is append-only'); END;
CREATE TRIGGER trg_attempt_connection_grants_no_delete BEFORE DELETE ON attempt_connection_grants
BEGIN SELECT RAISE(ABORT, 'attempt_connection_grants is append-only'); END;
CREATE TRIGGER trg_model_call_outputs_no_update BEFORE UPDATE ON model_call_outputs
BEGIN SELECT RAISE(ABORT, 'model_call_outputs is append-only'); END;
CREATE TRIGGER trg_model_call_outputs_no_delete BEFORE DELETE ON model_call_outputs
BEGIN SELECT RAISE(ABORT, 'model_call_outputs is append-only'); END;
CREATE TRIGGER trg_model_call_input_items_no_update BEFORE UPDATE ON model_call_input_items
BEGIN SELECT RAISE(ABORT, 'model_call_input_items is append-only'); END;
CREATE TRIGGER trg_model_call_input_items_no_delete BEFORE DELETE ON model_call_input_items
BEGIN SELECT RAISE(ABORT, 'model_call_input_items is append-only'); END;
CREATE TRIGGER trg_tool_call_connection_grants_no_update BEFORE UPDATE ON tool_call_connection_grants
BEGIN SELECT RAISE(ABORT, 'tool_call_connection_grants is append-only'); END;
CREATE TRIGGER trg_tool_call_connection_grants_no_delete BEFORE DELETE ON tool_call_connection_grants
BEGIN SELECT RAISE(ABORT, 'tool_call_connection_grants is append-only'); END;
CREATE TRIGGER trg_connection_probe_results_no_update BEFORE UPDATE ON connection_probe_results
BEGIN SELECT RAISE(ABORT, 'connection_probe_results is append-only'); END;
CREATE TRIGGER trg_connection_probe_results_no_delete BEFORE DELETE ON connection_probe_results
BEGIN SELECT RAISE(ABORT, 'connection_probe_results is append-only'); END;
CREATE TRIGGER trg_model_provider_connection_probe_results_no_update BEFORE UPDATE ON model_provider_connection_probe_results
BEGIN SELECT RAISE(ABORT, 'typed connection probe results are append-only'); END;
CREATE TRIGGER trg_model_provider_connection_probe_results_no_delete BEFORE DELETE ON model_provider_connection_probe_results
BEGIN SELECT RAISE(ABORT, 'typed connection probe results are append-only'); END;
CREATE TRIGGER trg_thanos_connection_probe_results_no_update BEFORE UPDATE ON thanos_connection_probe_results
BEGIN SELECT RAISE(ABORT, 'typed connection probe results are append-only'); END;
CREATE TRIGGER trg_thanos_connection_probe_results_no_delete BEFORE DELETE ON thanos_connection_probe_results
BEGIN SELECT RAISE(ABORT, 'typed connection probe results are append-only'); END;
CREATE TRIGGER trg_connection_enable_qualifications_no_update BEFORE UPDATE ON connection_enable_qualifications
BEGIN SELECT RAISE(ABORT, 'connection enable qualifications are append-only'); END;
CREATE TRIGGER trg_connection_enable_qualifications_no_delete BEFORE DELETE ON connection_enable_qualifications
BEGIN SELECT RAISE(ABORT, 'connection enable qualifications are append-only'); END;
CREATE TRIGGER trg_connection_revisions_no_update BEFORE UPDATE ON connection_revisions
BEGIN SELECT RAISE(ABORT, 'connection_revisions is append-only'); END;
CREATE TRIGGER trg_connection_revisions_no_delete BEFORE DELETE ON connection_revisions
BEGIN SELECT RAISE(ABORT, 'connection_revisions is append-only'); END;
CREATE TRIGGER trg_credential_generations_no_update BEFORE UPDATE ON credential_generations
BEGIN SELECT RAISE(ABORT, 'credential_generations is append-only'); END;
CREATE TRIGGER trg_credential_generations_no_delete BEFORE DELETE ON credential_generations
BEGIN SELECT RAISE(ABORT, 'credential_generations is append-only'); END;
CREATE TRIGGER trg_credential_generations_current_key_binding BEFORE INSERT ON credential_generations
WHEN NOT EXISTS (SELECT 1 FROM root_key_state k WHERE k.id = 1 AND k.binding_revision = NEW.key_binding_revision)
BEGIN SELECT RAISE(ABORT, 'credential generation must use the current root key binding revision'); END;
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
CREATE TRIGGER trg_initial_analysis_output_evidence_no_update BEFORE UPDATE ON initial_analysis_output_evidence
BEGIN SELECT RAISE(ABORT, 'initial_analysis_output_evidence is append-only'); END;
CREATE TRIGGER trg_initial_analysis_output_evidence_no_delete BEFORE DELETE ON initial_analysis_output_evidence
BEGIN SELECT RAISE(ABORT, 'initial_analysis_output_evidence is append-only'); END;
CREATE TRIGGER trg_initial_analysis_output_artifacts_no_update BEFORE UPDATE ON initial_analysis_output_artifacts
BEGIN SELECT RAISE(ABORT, 'initial_analysis_output_artifacts is append-only'); END;
CREATE TRIGGER trg_initial_analysis_output_artifacts_no_delete BEFORE DELETE ON initial_analysis_output_artifacts
BEGIN SELECT RAISE(ABORT, 'initial_analysis_output_artifacts is append-only'); END;
CREATE TRIGGER trg_initial_analysis_output_knowledge_no_update BEFORE UPDATE ON initial_analysis_output_knowledge_versions
BEGIN SELECT RAISE(ABORT, 'initial_analysis_output_knowledge_versions is append-only'); END;
CREATE TRIGGER trg_initial_analysis_output_knowledge_no_delete BEFORE DELETE ON initial_analysis_output_knowledge_versions
BEGIN SELECT RAISE(ABORT, 'initial_analysis_output_knowledge_versions is append-only'); END;
CREATE TRIGGER trg_investigation_message_evidence_no_update BEFORE UPDATE ON investigation_message_evidence
BEGIN SELECT RAISE(ABORT, 'investigation_message_evidence is append-only'); END;
CREATE TRIGGER trg_investigation_message_evidence_no_delete BEFORE DELETE ON investigation_message_evidence
BEGIN SELECT RAISE(ABORT, 'investigation_message_evidence is append-only'); END;
CREATE TRIGGER trg_investigation_message_artifacts_no_update BEFORE UPDATE ON investigation_message_artifacts
BEGIN SELECT RAISE(ABORT, 'investigation_message_artifacts is append-only'); END;
CREATE TRIGGER trg_investigation_message_artifacts_no_delete BEFORE DELETE ON investigation_message_artifacts
BEGIN SELECT RAISE(ABORT, 'investigation_message_artifacts is append-only'); END;
CREATE TRIGGER trg_investigation_message_knowledge_no_update BEFORE UPDATE ON investigation_message_knowledge_versions
BEGIN SELECT RAISE(ABORT, 'investigation_message_knowledge_versions is append-only'); END;
CREATE TRIGGER trg_investigation_message_knowledge_no_delete BEFORE DELETE ON investigation_message_knowledge_versions
BEGIN SELECT RAISE(ABORT, 'investigation_message_knowledge_versions is append-only'); END;
CREATE TRIGGER trg_inspection_report_evidence_no_update BEFORE UPDATE ON inspection_report_evidence
BEGIN SELECT RAISE(ABORT, 'inspection_report_evidence is append-only'); END;
CREATE TRIGGER trg_inspection_report_evidence_no_delete BEFORE DELETE ON inspection_report_evidence
BEGIN SELECT RAISE(ABORT, 'inspection_report_evidence is append-only'); END;
CREATE TRIGGER trg_inspection_report_artifacts_no_update BEFORE UPDATE ON inspection_report_artifacts
BEGIN SELECT RAISE(ABORT, 'inspection_report_artifacts is append-only'); END;
CREATE TRIGGER trg_inspection_report_artifacts_no_delete BEFORE DELETE ON inspection_report_artifacts
BEGIN SELECT RAISE(ABORT, 'inspection_report_artifacts is append-only'); END;
CREATE TRIGGER trg_inspection_report_knowledge_no_update BEFORE UPDATE ON inspection_report_knowledge_versions
BEGIN SELECT RAISE(ABORT, 'inspection_report_knowledge_versions is append-only'); END;
CREATE TRIGGER trg_inspection_report_knowledge_no_delete BEFORE DELETE ON inspection_report_knowledge_versions
BEGIN SELECT RAISE(ABORT, 'inspection_report_knowledge_versions is append-only'); END;
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
  source_id, delivery_id, delivery_item_id, kind, issue_key, detail_json, first_seen_at, created_at ON alert_intake_issues
BEGIN SELECT RAISE(ABORT, 'alert_intake_issues identity and first event are immutable'); END;
CREATE TRIGGER trg_alert_intake_issues_no_delete BEFORE DELETE ON alert_intake_issues
BEGIN SELECT RAISE(ABORT, 'alert_intake_issues history is not deletable'); END;
CREATE TRIGGER trg_alert_intake_issue_events_no_update BEFORE UPDATE ON alert_intake_issue_events
BEGIN SELECT RAISE(ABORT, 'alert_intake_issue_events is append-only'); END;
CREATE TRIGGER trg_alert_intake_issue_events_no_delete BEFORE DELETE ON alert_intake_issue_events
BEGIN SELECT RAISE(ABORT, 'alert_intake_issue_events history is not deletable'); END;

-- 12.4 调查消息：正文/角色/顺序不可变，只允许 active -> withdrawn
CREATE TRIGGER trg_investigation_messages_no_content_update BEFORE UPDATE OF
  investigation_id, attempt_id, seq, role, content, client_command_id, parent_message_id, created_at ON investigation_messages
BEGIN SELECT RAISE(ABORT, 'investigation_message content is immutable'); END;
CREATE TRIGGER trg_investigation_messages_no_delete BEFORE DELETE ON investigation_messages
BEGIN SELECT RAISE(ABORT, 'investigation_messages history is not deletable'); END;

-- 12.5 知识候选：原始建议与归属不可变；状态/草稿可更新；历史不可删除
CREATE TRIGGER trg_knowledge_candidates_no_origin_update BEFORE UPDATE OF
  import_batch_id, source_type, source_id, target_knowledge_id, generation, original_suggestion_json, created_by, created_at ON knowledge_candidates
BEGIN SELECT RAISE(ABORT, 'knowledge_candidate origin is immutable'); END;
CREATE TRIGGER trg_knowledge_candidates_no_delete BEFORE DELETE ON knowledge_candidates
BEGIN SELECT RAISE(ABORT, 'knowledge_candidates history is not deletable'); END;

-- 12.6 导入批次：状态可更新，其余不可变，历史不可删除
CREATE TRIGGER trg_knowledge_import_batches_no_origin_update BEFORE UPDATE OF
  source_material_id, generation, created_by, created_at ON knowledge_import_batches
BEGIN SELECT RAISE(ABORT, 'knowledge_import_batch origin is immutable'); END;
CREATE TRIGGER trg_knowledge_import_batches_no_delete BEFORE DELETE ON knowledge_import_batches
BEGIN SELECT RAISE(ABORT, 'knowledge_import_batches history is not deletable'); END;

-- 12.7 配置版本：只允许 state/published 字段变化，正文与类型化投影不可变
CREATE TRIGGER trg_business_config_versions_no_content_update BEFORE UPDATE OF
  business_system_id, version_seq, yaml_body, parser_version, schema_version,
  label_contract_version_id, declaration_json, description, discovery_refresh_seconds,
  system_key, display_name, metrics_connection_id, enabled, timezone,
  digest, created_by, created_at ON business_system_config_versions
BEGIN SELECT RAISE(ABORT, 'business_system_config_version content is immutable'); END;

-- 12.8 Label Contract：只允许 state/activation 变化，正文与类型化投影不可变
CREATE TRIGGER trg_label_contracts_no_content_update BEFORE UPDATE OF
  version, yaml_body, contract_json, digest, parser_version, schema_version, created_at ON label_contracts
BEGIN SELECT RAISE(ABORT, 'label_contract content is immutable'); END;

-- 12.9 连接：name/type 不可变。
CREATE TRIGGER trg_connections_no_identity_update BEFORE UPDATE OF name, type, created_at ON connections
BEGIN SELECT RAISE(ABORT, 'connection identity is immutable'); END;

-- 12.11 观测资源：身份字段不可变（identity_key 是相等性权威）
CREATE TRIGGER trg_observed_resources_no_identity_update BEFORE UPDATE OF
  business_system_id, discovery_key, identity_key, identity_digest, created_at ON observed_resources
BEGIN SELECT RAISE(ABORT, 'observed_resource identity is immutable'); END;

-- 12.12 Artifact：物理 blob 身份不可改写；逻辑 Artifact 的来源与到期时刻不可改写，
-- body_expired 只允许由 0 单向收口为 1（DATA-ARTIFACT-003/005）。
CREATE TRIGGER trg_artifact_blobs_no_update BEFORE UPDATE ON artifact_blobs
BEGIN SELECT RAISE(ABORT, 'artifact_blob content addressing is immutable'); END;
CREATE TRIGGER trg_artifact_blobs_no_delete BEFORE DELETE ON artifact_blobs
BEGIN SELECT RAISE(ABORT, 'artifact_blobs metadata is permanent; GC deletes only the physical body'); END;
CREATE TRIGGER trg_artifacts_origin_immutable BEFORE UPDATE OF
  blob_id, kind, media_type, sensitive, retention_kind, owner_type, owner_id, expires_at, created_by, created_at ON artifacts
BEGIN SELECT RAISE(ABORT, 'artifact logical origin and expiry are immutable'); END;
CREATE TRIGGER trg_artifacts_body_expired_sticky BEFORE UPDATE OF body_expired ON artifacts
WHEN OLD.body_expired = 1 AND NEW.body_expired <> 1
BEGIN SELECT RAISE(ABORT, 'expired artifact body cannot be revived'); END;
CREATE TRIGGER trg_artifacts_owner_closure BEFORE INSERT ON artifacts
WHEN NOT (
  (NEW.kind = 'report_file' AND NEW.owner_type = 'investigation_message' AND EXISTS (SELECT 1 FROM investigation_messages m WHERE m.id = NEW.owner_id))
  OR (NEW.kind = 'report_file' AND NEW.owner_type = 'evidence' AND EXISTS (SELECT 1 FROM evidence e WHERE e.id = NEW.owner_id))
  OR (NEW.kind = 'tool_result' AND NEW.owner_type = 'tool_call' AND EXISTS (SELECT 1 FROM tool_calls t WHERE t.id = NEW.owner_id))
  OR (NEW.kind = 'report_file' AND NEW.owner_type = 'inspection_report' AND EXISTS (SELECT 1 FROM inspection_reports r WHERE r.id = NEW.owner_id))
  OR (NEW.kind = 'report_file' AND NEW.owner_type = 'backup' AND EXISTS (SELECT 1 FROM backups b WHERE b.id = NEW.owner_id))
  OR (NEW.kind = 'attachment' AND NEW.owner_type = 'source_material' AND EXISTS (SELECT 1 FROM source_materials s WHERE s.id = NEW.owner_id))
)
BEGIN SELECT RAISE(ABORT, 'artifact kind/owner_type/owner_id must reference an existing compatible authority row'); END;

-- 12.14 模型/工具调用：每行是一条物理请求/执行事实；归属与签名不可变，只允许状态/result 收口。
CREATE TRIGGER trg_model_calls_no_origin_update BEFORE UPDATE OF
  attempt_id, call_seq, retry_seq, operation, model_id, connection_grant_id,
  prompt_renderer_version, agent_version, prompt_digest, tool_schema_version, tool_schema_digest,
  input_snapshot_digest, rendered_request_digest, context_budget_tokens, max_output_tokens, estimated_input_tokens,
  evicted_turn_count, started_at ON model_calls
BEGIN SELECT RAISE(ABORT, 'model_call origin is immutable'); END;
CREATE TRIGGER trg_tool_calls_no_origin_update BEFORE UPDATE OF
  attempt_id, model_call_id, call_seq, tool_index, provider_tool_call_id, tool_name,
  tool_version, arguments_json, arguments_digest, execution_mode, failure_mode, created_at ON tool_calls
BEGIN SELECT RAISE(ABORT, 'tool_call origin is immutable'); END;
-- Preflight routing is an accepted Tool Call fact, not mutable attempt state:
-- only one pending NULL -> paired known outcome transition is possible.
CREATE TRIGGER trg_tool_calls_preflight_immutable BEFORE UPDATE OF
  preflight_error_code, preflight_error_detail ON tool_calls
WHEN NOT (OLD.status = 'pending' AND NEW.status = 'pending'
  AND OLD.preflight_error_code IS NULL AND OLD.preflight_error_detail IS NULL
  AND NEW.preflight_error_code IS NOT NULL AND NEW.preflight_error_detail IS NOT NULL)
BEGIN SELECT RAISE(ABORT, 'tool_call preflight outcome is immutable'); END;

-- 12.15 用户：auth_revision 必须严格递增（账号变更裁决依据）
CREATE TRIGGER trg_users_auth_revision_monotonic BEFORE UPDATE OF auth_revision ON users
WHEN NEW.auth_revision <= OLD.auth_revision
BEGIN SELECT RAISE(ABORT, 'auth_revision must increase'); END;
CREATE TRIGGER trg_users_security_change_revision BEFORE UPDATE ON users
WHEN (NEW.enabled IS NOT OLD.enabled
   OR NEW.role IS NOT OLD.role
   OR NEW.password_phc IS NOT OLD.password_phc
   OR NEW.password_change_required IS NOT OLD.password_change_required
   OR NEW.password_change_required_at IS NOT OLD.password_change_required_at)
 AND NEW.auth_revision <> OLD.auth_revision + 1
BEGIN SELECT RAISE(ABORT, 'user security changes must increment auth_revision exactly once'); END;
CREATE TRIGGER trg_users_auth_revision_requires_security_change BEFORE UPDATE OF auth_revision ON users
WHEN NEW.enabled IS OLD.enabled
 AND NEW.role IS OLD.role
 AND NEW.password_phc IS OLD.password_phc
 AND NEW.password_change_required IS OLD.password_change_required
 AND NEW.password_change_required_at IS OLD.password_change_required_at
BEGIN SELECT RAISE(ABORT, 'auth_revision can only advance with a user security change'); END;

-- 12.16 告警源凭据：最多两个可认证 generation；轮换新凭据首次成功使用后，旧凭据机械进入
-- PendingRetirement，只有 Admin 显式命令才退休；任一已退休凭据不可复活（SEC-SERVICE-004）。
CREATE TRIGGER trg_alert_source_credentials_max2_insert BEFORE INSERT ON alert_source_credentials
WHEN NEW.state IN ('Active','PendingRetirement') AND (SELECT COUNT(*) FROM alert_source_credentials
     WHERE source_id = NEW.source_id AND state IN ('Active','PendingRetirement')) >= 2
BEGIN SELECT RAISE(ABORT, 'at most two accepted credentials per source'); END;
CREATE UNIQUE INDEX ux_alert_source_one_pending_retirement
  ON alert_source_credentials (source_id) WHERE state = 'PendingRetirement';
CREATE UNIQUE INDEX ux_alert_source_one_replacement
  ON alert_source_credentials (supersedes_credential_id) WHERE supersedes_credential_id IS NOT NULL;
CREATE TRIGGER trg_alert_source_credentials_rotation_shape BEFORE INSERT ON alert_source_credentials
WHEN (NEW.first_used_at IS NOT NULL OR NEW.pending_retirement_at IS NOT NULL OR NEW.retired_at IS NOT NULL)
  OR (NEW.supersedes_credential_id IS NULL AND EXISTS (
      SELECT 1 FROM alert_source_credentials c WHERE c.source_id = NEW.source_id AND c.state IN ('Active','PendingRetirement')))
  OR (NEW.supersedes_credential_id IS NOT NULL AND NOT EXISTS (
      SELECT 1 FROM alert_source_credentials c WHERE c.id = NEW.supersedes_credential_id
        AND c.source_id = NEW.source_id AND c.state = 'Active'))
BEGIN SELECT RAISE(ABORT, 'alert credential must start Active and either be the first accepted generation or supersede the current Active generation'); END;
CREATE TRIGGER trg_alert_source_credentials_state_transition BEFORE UPDATE OF state ON alert_source_credentials
WHEN NEW.state <> OLD.state AND NOT (
  (OLD.state = 'Active' AND NEW.state IN ('PendingRetirement','Retired'))
  OR (OLD.state = 'PendingRetirement' AND NEW.state = 'Retired'))
BEGIN SELECT RAISE(ABORT, 'invalid alert credential state transition'); END;
CREATE TRIGGER trg_alert_source_credentials_no_retire_superseded_before_first_use BEFORE UPDATE OF state ON alert_source_credentials
WHEN OLD.state = 'Active' AND NEW.state = 'Retired' AND EXISTS (
  SELECT 1 FROM alert_source_credentials replacement
  WHERE replacement.supersedes_credential_id = OLD.id
    AND replacement.state = 'Active'
    AND replacement.first_used_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'superseded alert credential cannot retire before replacement first use'); END;
CREATE TRIGGER trg_alert_source_credentials_first_used_once BEFORE UPDATE OF first_used_at ON alert_source_credentials
WHEN OLD.first_used_at IS NOT NULL OR NEW.first_used_at IS NULL OR OLD.state <> 'Active'
BEGIN SELECT RAISE(ABORT, 'alert credential first_used_at may only advance once while Active'); END;
CREATE TRIGGER trg_alert_source_credentials_mark_old_pending AFTER UPDATE OF first_used_at ON alert_source_credentials
WHEN OLD.first_used_at IS NULL AND NEW.first_used_at IS NOT NULL AND NEW.supersedes_credential_id IS NOT NULL
BEGIN
  UPDATE alert_source_credentials
  SET state = 'PendingRetirement', pending_retirement_at = NEW.first_used_at, row_version = row_version + 1
  WHERE id = NEW.supersedes_credential_id AND state = 'Active';
END;
CREATE TRIGGER trg_alert_source_credentials_lifecycle_shape BEFORE UPDATE ON alert_source_credentials
WHEN (NEW.state = 'Active' AND (NEW.pending_retirement_at IS NOT NULL OR NEW.retired_at IS NOT NULL))
  OR (NEW.state = 'PendingRetirement' AND (NEW.pending_retirement_at IS NULL OR NEW.retired_at IS NOT NULL))
  OR (NEW.state = 'Retired' AND NEW.retired_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'alert credential lifecycle timestamps must match state'); END;
CREATE TRIGGER trg_alert_source_credentials_row_version_increment BEFORE UPDATE ON alert_source_credentials
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'credential row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_alert_deliveries_credential_current BEFORE INSERT ON alert_deliveries
WHEN NOT EXISTS (
  SELECT 1 FROM alert_sources s JOIN alert_source_credentials c ON c.source_id = s.id
  WHERE s.id = NEW.source_id AND s.enabled = 1 AND c.id = NEW.credential_id
    AND c.state IN ('Active','PendingRetirement'))
BEGIN SELECT RAISE(ABORT, 'delivery credential must be accepted, enabled, and owned by the source at commit'); END;
CREATE TRIGGER trg_alert_deliveries_record_first_use AFTER INSERT ON alert_deliveries
WHEN EXISTS (SELECT 1 FROM alert_source_credentials c WHERE c.id = NEW.credential_id AND c.first_used_at IS NULL)
BEGIN
  UPDATE alert_source_credentials SET first_used_at = NEW.received_at, row_version = row_version + 1
  WHERE id = NEW.credential_id AND first_used_at IS NULL;
END;
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

-- Platform faults use the same bounded alert change transport without claiming
-- an upstream occurrence; platform_fault_id preserves the independent identity.
CREATE TRIGGER trg_platform_fault_change_insert AFTER INSERT ON platform_faults
BEGIN
  INSERT INTO alert_change_log (platform_fault_id, change_type, row_version)
  VALUES (NEW.id, 'created', NEW.row_version);
END;
CREATE TRIGGER trg_platform_fault_change_state AFTER UPDATE OF state ON platform_faults
WHEN NEW.state <> OLD.state
BEGIN
  INSERT INTO alert_change_log (platform_fault_id, change_type, row_version)
  VALUES (NEW.id, 'state_changed', NEW.row_version);
END;

-- 12.19 row_version 由应用在每次 UPDATE 中显式递增（SQLite 触发器不支持 SET NEW；
-- 并发前提 expected_row_version 的比较与递增在同一 UPDATE 的 WHERE 与 SET 中完成）。

-- 12.20 知识检索退出是粘性的：永不自动复活（恢复复用必须创建并确认新版本）
CREATE TRIGGER trg_knowledge_retrieval_exit_sticky BEFORE UPDATE ON knowledge_version_retrieval_state
WHEN OLD.exited = 1
BEGIN SELECT RAISE(ABORT, 'retrieval exit is terminal and immutable; recovery requires a new confirmed version'); END;
-- exited_at / updated_at 由应用在退出 UPDATE 中一次写入，退出后整行不可改写。

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
CREATE TRIGGER trg_knowledge_search_docs_no_update BEFORE UPDATE ON knowledge_search_docs
BEGIN SELECT RAISE(ABORT, 'knowledge_search_docs is an immutable eligibility projection; delete and insert a new version projection'); END;

-- 12.22 稳定身份 key / 登录名：不可改写
CREATE TRIGGER trg_users_username_immutable BEFORE UPDATE OF username ON users
BEGIN SELECT RAISE(ABORT, 'username is stable and cannot be rewritten'); END;
CREATE TRIGGER trg_business_systems_identity_immutable BEFORE UPDATE OF key, created_at ON business_systems
BEGIN SELECT RAISE(ABORT, 'business_system stable key is immutable'); END;
-- 首次 YAML 上传创建的聚合必须从 Disabled/未发布状态开始；YAML 的 enabled/timezone
-- 只存在于第一份不可变草稿，显式发布后才投影到 business_systems（DATA-CONFIG-001）。
CREATE TRIGGER trg_business_systems_insert_unconfigured BEFORE INSERT ON business_systems
WHEN NEW.enabled <> 0 OR NEW.current_config_version_id IS NOT NULL
  OR NEW.timezone IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'business_system must be created Disabled with no current config or published root projection'); END;
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
  plan_id, connection_id, plugin_id, template_id, template_version,
  frozen_params_json, frozen_scope_json,
  frozen_display_name, frozen_check_description, frozen_metric_unit, frozen_report_instructions,
  trigger_kind, scheduled_for, rerun_of_id, created_at ON inspection_runs
BEGIN SELECT RAISE(ABORT, 'inspection_run binding is immutable'); END;
CREATE TRIGGER trg_execution_attempts_origin_immutable BEFORE UPDATE OF
  attempt_type, scope_type, scope_id, plan_key, check_key,
  quoin_release_version, created_at ON execution_attempts
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
CREATE TRIGGER trg_maintenance_state_row_version_increment BEFORE UPDATE ON maintenance_state
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'maintenance_state row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_knowledge_version_retrieval_state_row_version_increment BEFORE UPDATE ON knowledge_version_retrieval_state
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'knowledge_version_retrieval_state row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_reusable_knowledge_row_version_increment BEFORE UPDATE ON reusable_knowledge
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'reusable_knowledge row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_backup_settings_row_version_increment BEFORE UPDATE ON backup_settings
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'backup_settings row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_backup_settings_schedule_enabled_at_transition BEFORE UPDATE ON backup_settings
WHEN (NEW.enabled = OLD.enabled AND NEW.schedule_enabled_at IS NOT OLD.schedule_enabled_at)
  OR (OLD.enabled = 1 AND NEW.enabled = 0 AND NEW.schedule_enabled_at IS NOT NULL)
  OR (OLD.enabled = 0 AND NEW.enabled = 1 AND NEW.schedule_enabled_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'backup_settings schedule_enabled_at must follow enabled transitions'); END;
CREATE TRIGGER trg_artifact_retention_settings_row_version_increment BEFORE UPDATE ON artifact_retention_settings
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'artifact_retention_settings row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_backups_row_version_increment BEFORE UPDATE ON backups
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'backups row_version must increase exactly by 1'); END;

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
CREATE TRIGGER trg_alert_intake_issues_repeat_update BEFORE UPDATE OF last_seen_at, occurrence_count, last_event_id ON alert_intake_issues
WHEN OLD.acknowledged_at IS NOT NULL
  OR NEW.acknowledged_at IS NOT OLD.acknowledged_at
  OR NEW.acknowledged_by IS NOT OLD.acknowledged_by
  OR NEW.occurrence_count <> OLD.occurrence_count + 1
  OR NEW.last_event_id IS NULL OR NEW.last_event_id = OLD.last_event_id
  OR NOT EXISTS (
    SELECT 1 FROM alert_intake_issue_events e
    WHERE e.id = NEW.last_event_id AND e.issue_id = OLD.id AND e.observed_at = NEW.last_seen_at
      AND e.id > COALESCE(OLD.last_event_id, 0)
  )
  OR NEW.occurrence_count <> (SELECT COUNT(*) FROM alert_intake_issue_events e WHERE e.issue_id = OLD.id)
BEGIN SELECT RAISE(ABORT, 'intake issue repeat must append one new event and advance the open aggregate once'); END;
CREATE TRIGGER trg_alert_intake_issues_ack_does_not_change_repeat BEFORE UPDATE OF acknowledged_at, acknowledged_by ON alert_intake_issues
WHEN NEW.last_seen_at <> OLD.last_seen_at OR NEW.occurrence_count <> OLD.occurrence_count OR NEW.last_event_id IS NOT OLD.last_event_id
BEGIN SELECT RAISE(ABORT, 'intake issue acknowledgement cannot rewrite repeat history'); END;

-- 指针变更只能由激活触发器内部修改（trg_label_contract_state_activate_atomic）。
-- 放行条件不是时间戳：目标 activation 必须是本次尚未标记 applied 的不可变 INSERT，且 contract 精确匹配。
-- SQLite statement 原子性保证 activation INSERT 的 AFTER 触发器完成前外部语句不可见该未应用行。
CREATE TRIGGER trg_label_contract_state_row_version_increment BEFORE UPDATE ON label_contract_state
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'label_contract_state row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_label_contract_state_no_delete BEFORE DELETE ON label_contract_state
BEGIN SELECT RAISE(ABORT, 'label_contract_state is a single-row table'); END;
CREATE TRIGGER trg_label_contract_state_no_insert_pointer BEFORE INSERT ON label_contract_state
WHEN NEW.current_contract_id IS NOT NULL OR NEW.current_activation_id IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'label_contract_state pointer can only be set by the atomic activation INSERT'); END;
CREATE TRIGGER trg_label_contract_state_no_direct_pointer_update BEFORE UPDATE OF current_contract_id, current_activation_id ON label_contract_state
WHEN NEW.current_contract_id IS OLD.current_contract_id
  OR NEW.current_activation_id IS OLD.current_activation_id
  OR NOT EXISTS (
    SELECT 1 FROM label_contract_activations a
    WHERE a.id = NEW.current_activation_id
      AND a.contract_id = NEW.current_contract_id
      AND a.applied_at IS NULL
  )
BEGIN SELECT RAISE(ABORT, 'label_contract_state pointer pair can only be changed by the matching atomic activation INSERT'); END;
CREATE TRIGGER trg_label_contract_state_no_unset BEFORE UPDATE OF current_contract_id, current_activation_id ON label_contract_state
WHEN NEW.current_contract_id IS NULL OR NEW.current_activation_id IS NULL
BEGIN SELECT RAISE(ABORT, 'label_contract_state pointer cannot be unset (no deactivation)'); END;

-- 12.23f
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
CREATE TRIGGER trg_legacy_config_version_mappings_no_update BEFORE UPDATE ON legacy_config_version_mappings
BEGIN SELECT RAISE(ABORT, 'legacy_config_version_mappings is append-only'); END;
CREATE TRIGGER trg_legacy_config_version_mappings_no_delete BEFORE DELETE ON legacy_config_version_mappings
BEGIN SELECT RAISE(ABORT, 'legacy_config_version_mappings is append-only'); END;
CREATE TRIGGER trg_observed_resources_no_delete BEFORE DELETE ON observed_resources
BEGIN SELECT RAISE(ABORT, 'observed_resources history is not deletable'); END;
CREATE TRIGGER trg_connections_no_delete BEFORE DELETE ON connections
BEGIN SELECT RAISE(ABORT, 'connections are tombstone-only'); END;
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
CREATE TRIGGER trg_tool_call_execution_inputs_no_update BEFORE UPDATE ON tool_call_execution_inputs
BEGIN SELECT RAISE(ABORT, 'tool_call execution input is immutable'); END;
CREATE TRIGGER trg_tool_call_execution_inputs_no_delete BEFORE DELETE ON tool_call_execution_inputs
BEGIN SELECT RAISE(ABORT, 'tool_call execution input is retained audit history'); END;
CREATE TRIGGER trg_reusable_knowledge_no_delete BEFORE DELETE ON reusable_knowledge
BEGIN SELECT RAISE(ABORT, 'reusable_knowledge history is not deletable'); END;
CREATE TRIGGER trg_knowledge_retrieval_state_no_delete BEFORE DELETE ON knowledge_version_retrieval_state
BEGIN SELECT RAISE(ABORT, 'retrieval exit state is sticky and not deletable'); END;
CREATE TRIGGER trg_embedding_generations_no_delete BEFORE DELETE ON embedding_generations
BEGIN SELECT RAISE(ABORT, 'embedding_generations history is not deletable'); END;
CREATE TRIGGER trg_backups_no_delete BEFORE DELETE ON backups
BEGIN SELECT RAISE(ABORT, 'backups history is not deletable'); END;

CREATE TRIGGER trg_schema_state_no_delete BEFORE DELETE ON schema_state
BEGIN SELECT RAISE(ABORT, 'schema_state is a single-row table'); END;
CREATE TRIGGER trg_backup_settings_no_delete BEFORE DELETE ON backup_settings
BEGIN SELECT RAISE(ABORT, 'backup_settings is a single-row table'); END;
CREATE TRIGGER trg_artifact_retention_settings_no_delete BEFORE DELETE ON artifact_retention_settings
BEGIN SELECT RAISE(ABORT, 'artifact_retention_settings is a single-row table'); END;

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
-- 指针变更前置守卫：只能移动到同系统的未发布版本（published_at IS NULL 即从未发布）；
-- 禁止直接 INSERT/UPDATE 携带已发布版本（re-publish 旧版本）或跨系统版本（DATA-CONFIG-001）。
CREATE TRIGGER trg_business_systems_config_owner_update BEFORE UPDATE OF current_config_version_id ON business_systems
WHEN NEW.current_config_version_id IS NOT NULL AND (OLD.current_config_version_id IS NULL OR NEW.current_config_version_id IS NOT OLD.current_config_version_id)
  AND NOT EXISTS (SELECT 1 FROM business_system_config_versions v
                  WHERE v.id = NEW.current_config_version_id AND v.business_system_id = NEW.id AND v.published_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'current config pointer can only move to an unpublished version of the same business system'); END;
-- 禁止 current 指针从非空变为 NULL（不允许取消发布；DATA-CONFIG-001）。
-- 普通发布只能选择以当前 Label Contract 为目标的草稿；切向候选 Label Contract 的配置版本
-- 只能由同一条未应用 activation INSERT 的原子联合激活触发器完成（DATA-CONFIG-001/002）。
-- Label Contract provenance is archival-only; publishing a draft never depends on an active contract.
CREATE TRIGGER trg_business_systems_no_unset_config_pointer BEFORE UPDATE OF current_config_version_id ON business_systems
WHEN OLD.current_config_version_id IS NOT NULL AND NEW.current_config_version_id IS NULL
BEGIN SELECT RAISE(ABORT, 'business_systems current_config_version_id cannot be unset (no deactivation)'); END;
-- 根投影守卫：business_systems 的 display_name/enabled/timezone 必须等于 current
-- 指针所指版本的类型化根投影（不允许绕过 YAML 发布直接改写，DATA-CONFIG-001）。
CREATE TRIGGER trg_business_systems_projection_matches_version BEFORE UPDATE OF display_name, enabled, timezone ON business_systems
WHEN NEW.current_config_version_id IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM business_system_config_versions v
  WHERE v.id = NEW.current_config_version_id
    AND v.display_name = NEW.display_name AND v.enabled = NEW.enabled
    AND v.timezone = NEW.timezone
)
BEGIN SELECT RAISE(ABORT, 'business_system root projection must equal its current config version root projection'); END;
CREATE TRIGGER trg_business_systems_pointer_projection_on_pointer_change BEFORE UPDATE OF current_config_version_id ON business_systems
WHEN NEW.current_config_version_id IS NOT NULL AND (OLD.current_config_version_id IS NULL OR NEW.current_config_version_id IS NOT OLD.current_config_version_id) AND NOT EXISTS (
  SELECT 1 FROM business_system_config_versions v
  WHERE v.id = NEW.current_config_version_id
    AND v.display_name = NEW.display_name AND v.enabled = NEW.enabled
    AND v.timezone = NEW.timezone
)
BEGIN SELECT RAISE(ABORT, 'current pointer change must carry the target version root projection in the same UPDATE'); END;
-- 指针变更后继：把新 current 版本派生为 published（写入一次性 published_at 事实）、旧 current 派生为 superseded。
CREATE TRIGGER trg_business_systems_publish_derived AFTER UPDATE OF current_config_version_id ON business_systems
WHEN NEW.current_config_version_id IS NOT NULL AND (OLD.current_config_version_id IS NULL OR NEW.current_config_version_id IS NOT OLD.current_config_version_id)
BEGIN
  UPDATE business_system_config_versions SET state = 'published', published_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
  WHERE id = NEW.current_config_version_id AND state = 'draft';
  UPDATE business_system_config_versions SET state = 'superseded'
  WHERE id = OLD.current_config_version_id AND OLD.current_config_version_id IS NOT NULL AND state = 'published';
END;
CREATE TRIGGER trg_reusable_knowledge_current_owner_insert AFTER INSERT ON reusable_knowledge
WHEN NEW.current_version_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM knowledge_versions v WHERE v.id = NEW.current_version_id AND v.knowledge_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'current_version_id must belong to the same knowledge'); END;
CREATE TRIGGER trg_reusable_knowledge_current_owner_update AFTER UPDATE OF current_version_id ON reusable_knowledge
WHEN NEW.current_version_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM knowledge_versions v WHERE v.id = NEW.current_version_id AND v.knowledge_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'current_version_id must belong to the same knowledge'); END;
CREATE TRIGGER trg_knowledge_versions_next_sequence BEFORE INSERT ON knowledge_versions
WHEN NEW.version_seq <> COALESCE((SELECT MAX(v.version_seq) FROM knowledge_versions v WHERE v.knowledge_id = NEW.knowledge_id), 0) + 1
BEGIN SELECT RAISE(ABORT, 'knowledge version sequence must append exactly once'); END;
CREATE TRIGGER trg_reusable_knowledge_current_forward_only BEFORE UPDATE OF current_version_id ON reusable_knowledge
WHEN NEW.current_version_id IS NULL
  OR NOT EXISTS (
    SELECT 1 FROM knowledge_versions next
    LEFT JOIN knowledge_versions previous ON previous.id = OLD.current_version_id
    WHERE next.id = NEW.current_version_id AND next.knowledge_id = OLD.id
      AND next.version_seq = COALESCE(previous.version_seq, 0) + 1
  )
BEGIN SELECT RAISE(ABORT, 'knowledge current version must advance to the next immutable version'); END;
CREATE TRIGGER trg_knowledge_candidates_source_insert AFTER INSERT ON knowledge_candidates
WHEN NOT (
  (NEW.source_type = 'initial_analysis_output' AND EXISTS (
    SELECT 1 FROM initial_analysis_outputs o WHERE o.id = NEW.source_id
  ))
  OR (NEW.source_type = 'inspection_report' AND EXISTS (
    SELECT 1 FROM inspection_reports r WHERE r.id = NEW.source_id
  ))
  OR (NEW.source_type = 'investigation_message' AND EXISTS (
    SELECT 1 FROM investigation_messages m WHERE m.id = NEW.source_id AND m.role = 'assistant' AND m.status = 'active'
  ))
  OR (NEW.source_type = 'source_material' AND EXISTS (
    SELECT 1 FROM knowledge_import_batches b
    JOIN source_materials s ON s.id = b.source_material_id
    WHERE b.id = NEW.import_batch_id AND s.id = NEW.source_id
  ))
  OR (NEW.source_type = 'knowledge_version' AND EXISTS (
    SELECT 1 FROM knowledge_versions v
    WHERE v.id = NEW.source_id AND v.knowledge_id = NEW.target_knowledge_id
  ))
)
BEGIN SELECT RAISE(ABORT, 'knowledge candidate must reference a valid immutable source'); END;
CREATE TRIGGER trg_knowledge_candidates_source_not_rejected AFTER INSERT ON knowledge_candidates
WHEN NEW.source_type IN ('initial_analysis_output','inspection_report','investigation_message')
  AND EXISTS (
    SELECT 1 FROM diagnosis_feedback f
    WHERE f.target_type = NEW.source_type AND f.target_id = NEW.source_id AND f.value = 'rejected'
  )
BEGIN SELECT RAISE(ABORT, 'rejected diagnosis source cannot create a knowledge candidate'); END;
CREATE TRIGGER trg_attempt_input_items_no_withdrawn_message AFTER INSERT ON attempt_input_items
WHEN NEW.investigation_message_id IS NOT NULL
  AND EXISTS (
    SELECT 1 FROM investigation_messages m
    WHERE m.id = NEW.investigation_message_id AND m.status = 'withdrawn'
  )
BEGIN SELECT RAISE(ABORT, 'withdrawn investigation message cannot enter a new attempt input snapshot'); END;
CREATE TRIGGER trg_knowledge_versions_candidate_closure_insert AFTER INSERT ON knowledge_versions
WHEN NOT EXISTS (
  SELECT 1 FROM knowledge_candidates c
  WHERE c.id = NEW.source_candidate_id
    AND c.state = 'Confirmed'
    AND c.confirmed_knowledge_id = NEW.knowledge_id
)
BEGIN SELECT RAISE(ABORT, 'knowledge version must be produced by its confirmed candidate'); END;
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
  source_id, digest, supersedes_credential_id, created_at ON alert_source_credentials
BEGIN SELECT RAISE(ABORT, 'alert_source_credential origin is immutable'); END;
-- Attempt 可追加多条不可变 attempt_connection_grants；execution_attempts 不再携带单连接伪权威。
-- Backup Run 仅允许 queued -> running|failed、running -> running|succeeded|failed；阶段只前进，终态不可变。
-- 行身份、触发来源与 created_at 不可改写；每次合法状态推进恰好递增 row_version（OPS-BACKUP-001..004）。
CREATE TRIGGER trg_backups_identity_immutable BEFORE UPDATE OF id, trigger_kind, execution_mode, scheduled_for, created_at, triggered_by ON backups
BEGIN SELECT RAISE(ABORT, 'backup run identity is immutable'); END;
CREATE TRIGGER trg_backups_transition BEFORE UPDATE ON backups
WHEN NOT (
  (OLD.status = 'queued' AND NEW.status IN ('running','failed'))
  OR (OLD.status = 'running' AND NEW.status IN ('running','succeeded','failed'))
)
BEGIN SELECT RAISE(ABORT, 'invalid backup run transition'); END;
CREATE TRIGGER trg_backups_transition_stage_consistent BEFORE UPDATE ON backups
WHEN
  (OLD.status = 'queued' AND NEW.status = 'running' AND NEW.stage <> 'preflight')
  OR (OLD.status = 'queued' AND NEW.status = 'failed' AND NEW.stage <> 'queued')
  OR (OLD.status = 'running' AND NEW.status = 'failed' AND NEW.stage <> OLD.stage)
  OR (OLD.status = 'running' AND NEW.status = 'succeeded' AND OLD.stage <> 'manifest_publish')
BEGIN SELECT RAISE(ABORT, 'backup run transition stage is inconsistent'); END;
CREATE TRIGGER trg_backups_stage_forward BEFORE UPDATE ON backups
WHEN OLD.status = 'running' AND NEW.status = 'running' AND (
  CASE NEW.stage
    WHEN 'preflight' THEN 1
    WHEN 'database_snapshot' THEN 2
    WHEN 'artifact_copy' THEN 3
    WHEN 'manifest_publish' THEN 4
    ELSE 0
  END < CASE OLD.stage
    WHEN 'preflight' THEN 1
    WHEN 'database_snapshot' THEN 2
    WHEN 'artifact_copy' THEN 3
    WHEN 'manifest_publish' THEN 4
    ELSE 0
  END
  OR CASE NEW.stage
    WHEN 'preflight' THEN 1
    WHEN 'database_snapshot' THEN 2
    WHEN 'artifact_copy' THEN 3
    WHEN 'manifest_publish' THEN 4
    ELSE 0
  END > CASE OLD.stage
    WHEN 'preflight' THEN 1
    WHEN 'database_snapshot' THEN 2
    WHEN 'artifact_copy' THEN 3
    WHEN 'manifest_publish' THEN 4
    ELSE 0
  END + 1
)
BEGIN SELECT RAISE(ABORT, 'backup run stage must remain or advance by one'); END;
CREATE TRIGGER trg_backups_updated_at_changes BEFORE UPDATE ON backups
WHEN NEW.updated_at = OLD.updated_at
BEGIN SELECT RAISE(ABORT, 'backup run update must change updated_at'); END;
CREATE TRIGGER trg_maintenance_state_insert_inactive BEFORE INSERT ON maintenance_state
WHEN NEW.active <> 0 OR NEW.reason IS NOT NULL OR NEW.entered_at IS NOT NULL OR NEW.entered_by_type IS NOT NULL OR NEW.entered_by_id IS NOT NULL
  OR NEW.exited_at IS NOT NULL OR NEW.exited_by_type IS NOT NULL OR NEW.exited_by_id IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'maintenance_state must be seeded inactive before entering maintenance'); END;
CREATE TRIGGER trg_maintenance_state_transition BEFORE UPDATE OF active ON maintenance_state
WHEN NEW.active <> OLD.active AND NOT (
  (OLD.active = 0 AND NEW.active = 1 AND NEW.reason IS NOT NULL AND NEW.entered_at IS NOT NULL
    AND NEW.entered_by_type IS NOT NULL AND NEW.exited_at IS NULL AND NEW.exited_by_type IS NULL AND NEW.exited_by_id IS NULL)
  OR (OLD.active = 1 AND NEW.active = 0 AND NEW.reason IS NULL AND NEW.entered_at IS NULL
    AND NEW.entered_by_type IS NULL AND NEW.entered_by_id IS NULL AND NEW.exited_at IS NOT NULL AND NEW.exited_by_type IS NOT NULL))
BEGIN SELECT RAISE(ABORT, 'maintenance state transition must explicitly enter or exit'); END;
CREATE TRIGGER trg_maintenance_state_active_identity_immutable BEFORE UPDATE OF reason, entered_at, entered_by_type, entered_by_id ON maintenance_state
WHEN OLD.active = 1 AND NEW.active = 1 AND (
  NEW.reason IS NOT OLD.reason OR NEW.entered_at IS NOT OLD.entered_at
  OR NEW.entered_by_type IS NOT OLD.entered_by_type OR NEW.entered_by_id IS NOT OLD.entered_by_id)
BEGIN SELECT RAISE(ABORT, 'active maintenance identity is immutable'); END;
CREATE TRIGGER trg_maintenance_state_active_revision_frozen BEFORE UPDATE OF row_version ON maintenance_state
WHEN OLD.active = 1 AND NEW.active = 1
BEGIN SELECT RAISE(ABORT, 'active maintenance revision is frozen until exit'); END;
CREATE TRIGGER trg_maintenance_state_exit_requires_safe_items BEFORE UPDATE OF active ON maintenance_state
WHEN OLD.active = 1 AND NEW.active = 0 AND (
  NOT EXISTS (SELECT 1 FROM maintenance_items i WHERE i.maintenance_revision = OLD.row_version)
  OR EXISTS (SELECT 1 FROM maintenance_items i WHERE i.maintenance_revision = OLD.row_version AND i.safe_state = 'Blocking'))
BEGIN SELECT RAISE(ABORT, 'maintenance exit requires a non-empty checklist with every item safe'); END;
CREATE TRIGGER trg_maintenance_state_no_delete BEFORE DELETE ON maintenance_state
BEGIN SELECT RAISE(ABORT, 'maintenance_state is not deletable'); END;
CREATE TRIGGER trg_maintenance_items_insert_current BEFORE INSERT ON maintenance_items
WHEN NOT EXISTS (SELECT 1 FROM maintenance_state m WHERE m.id = 1 AND m.active = 1 AND m.row_version = NEW.maintenance_revision)
BEGIN SELECT RAISE(ABORT, 'maintenance item must belong to the active maintenance revision'); END;
CREATE TRIGGER trg_maintenance_items_identity_immutable BEFORE UPDATE OF maintenance_revision, kind, object_key ON maintenance_items
BEGIN SELECT RAISE(ABORT, 'maintenance item identity is immutable'); END;
CREATE TRIGGER trg_maintenance_items_update_current BEFORE UPDATE ON maintenance_items
WHEN NOT EXISTS (SELECT 1 FROM maintenance_state m WHERE m.id = 1 AND m.active = 1 AND m.row_version = OLD.maintenance_revision)
BEGIN SELECT RAISE(ABORT, 'only items of the active maintenance revision may change'); END;
CREATE TRIGGER trg_maintenance_items_no_delete BEFORE DELETE ON maintenance_items
BEGIN SELECT RAISE(ABORT, 'maintenance checklist history is not deletable'); END;
-- Embedding generation 来源字段不可变；vector_dim 一旦设置或该 generation 已有 embeddings 即不可变更
CREATE TRIGGER trg_embedding_generations_origin_immutable BEFORE UPDATE OF
  model_name, model_version, generation, created_at ON embedding_generations
BEGIN SELECT RAISE(ABORT, 'embedding_generation origin is immutable'); END;
CREATE TRIGGER trg_embedding_generations_vector_dim_immutable BEFORE UPDATE OF vector_dim ON embedding_generations
WHEN (OLD.vector_dim IS NOT NULL AND NEW.vector_dim <> OLD.vector_dim)
  OR (EXISTS (SELECT 1 FROM embeddings e WHERE e.embedding_generation_id = OLD.id) AND NEW.vector_dim IS NOT OLD.vector_dim)
BEGIN SELECT RAISE(ABORT, 'embedding_generation vector_dim is immutable once set or embeddings exist'); END;
CREATE TRIGGER trg_embedding_generations_insert_building BEFORE INSERT ON embedding_generations
WHEN NEW.state <> 'building'
BEGIN SELECT RAISE(ABORT, 'embedding_generation must be created building'); END;
CREATE TRIGGER trg_embedding_generations_state_transition BEFORE UPDATE OF state ON embedding_generations
WHEN NEW.state <> OLD.state AND NOT (
  (OLD.state = 'building' AND NEW.state IN ('current','retired'))
  OR (OLD.state = 'current' AND NEW.state = 'retired')
)
BEGIN SELECT RAISE(ABORT, 'invalid embedding_generation state transition'); END;

-- 12.28 生命周期终态不可变：终态只可到达、不可离开（非终态间转换由应用按状态机推进）
CREATE TRIGGER trg_initial_analyses_insert_queued BEFORE INSERT ON initial_analyses
WHEN NEW.state <> 'Queued'
BEGIN SELECT RAISE(ABORT, 'initial_analysis must be created Queued'); END;
CREATE TRIGGER trg_initial_analyses_terminal_immutable BEFORE UPDATE OF state ON initial_analyses
WHEN OLD.state IN ('Succeeded','Failed','Cancelled','Interrupted') AND NEW.state <> OLD.state
BEGIN SELECT RAISE(ABORT, 'initial_analysis terminal state is immutable'); END;
CREATE TRIGGER trg_initial_analyses_state_transition BEFORE UPDATE OF state ON initial_analyses
WHEN NEW.state <> OLD.state AND NOT (
  (OLD.state = 'Queued' AND NEW.state IN ('Running','Failed','Cancelled','Interrupted'))
  OR (OLD.state = 'Running' AND NEW.state IN ('Succeeded','Failed','Cancelled','Interrupted'))
)
BEGIN SELECT RAISE(ABORT, 'invalid initial_analysis state transition'); END;
CREATE TRIGGER trg_inspection_runs_insert_state BEFORE INSERT ON inspection_runs
WHEN NEW.state NOT IN ('Queued','SkippedOverlap')
  OR (NEW.state = 'SkippedOverlap' AND (NEW.trigger_kind <> 'schedule' OR NEW.scheduled_for IS NULL))
BEGIN SELECT RAISE(ABORT, 'inspection_run must be created Queued or as a scheduled SkippedOverlap record'); END;
CREATE TRIGGER trg_inspection_runs_terminal_immutable BEFORE UPDATE OF state ON inspection_runs
WHEN OLD.state IN ('Completed','CompletedWithGaps','Failed','Cancelled','Interrupted','SkippedOverlap') AND NEW.state <> OLD.state
BEGIN SELECT RAISE(ABORT, 'inspection_run terminal state is immutable'); END;
CREATE TRIGGER trg_inspection_runs_state_transition BEFORE UPDATE OF state ON inspection_runs
WHEN NEW.state <> OLD.state AND NOT (
  (OLD.state = 'Queued' AND NEW.state IN ('Running','Failed','Cancelled','Interrupted'))
  OR (OLD.state = 'Running' AND NEW.state IN ('Completed','CompletedWithGaps','Failed','Cancelled','Interrupted'))
)
BEGIN SELECT RAISE(ABORT, 'invalid inspection_run state transition'); END;
CREATE TRIGGER trg_inspection_runs_evidence_at_once BEFORE UPDATE OF evidence_at ON inspection_runs
WHEN NOT (OLD.evidence_at IS NULL AND NEW.evidence_at IS NOT NULL AND OLD.state = 'Queued' AND NEW.state = 'Running')
BEGIN SELECT RAISE(ABORT, 'inspection_run evidence_at is generated exactly once when collection starts'); END;
-- 终态转换必须与子 Attempt fence 同一事务提交；Cancelled 允许已 fence 到 Cancelling 的运行子 Attempt。
CREATE TRIGGER trg_inspection_runs_terminal_children_fenced BEFORE UPDATE OF state ON inspection_runs
WHEN NEW.state <> OLD.state AND NEW.state IN ('Completed','CompletedWithGaps','Failed','Cancelled','Interrupted') AND EXISTS (
  SELECT 1 FROM execution_attempts a
  WHERE a.scope_type = 'run_check' AND a.scope_id = NEW.id
    AND (a.state IN ('Queued','Assigned','Running') OR (NEW.state <> 'Cancelled' AND a.state = 'Cancelling'))
)
BEGIN SELECT RAISE(ABORT, 'inspection_run cannot become terminal before child attempts are fenced'); END;
-- 冻结成功结果集前必须覆盖计划全部 check；Completed 全为完整 ok，CompletedWithGaps 至少一项明确缺口。
CREATE TRIGGER trg_inspection_runs_result_set_complete BEFORE UPDATE OF state ON inspection_runs
WHEN NEW.state IN ('Completed','CompletedWithGaps') AND NEW.state <> OLD.state AND (
  EXISTS (
    SELECT 1 FROM config_plans p JOIN config_checks c ON c.plan_id = p.id
    WHERE p.config_version_id = NEW.config_version_id AND p.plan_key = NEW.plan_key
      AND NOT EXISTS (SELECT 1 FROM inspection_check_results r WHERE r.run_id = NEW.id AND r.check_key = c.check_key)
  )
  OR EXISTS (
    SELECT 1 FROM inspection_run_checks c
    WHERE c.run_id = NEW.id
      AND NOT EXISTS (SELECT 1 FROM inspection_check_results r WHERE r.run_id = NEW.id AND r.check_key = c.check_key)
  )
  OR (NEW.state = 'Completed' AND EXISTS (SELECT 1 FROM inspection_check_results r WHERE r.run_id = NEW.id AND r.status <> 'ok'))
  OR (NEW.state = 'CompletedWithGaps' AND NOT EXISTS (SELECT 1 FROM inspection_check_results r WHERE r.run_id = NEW.id AND r.status IN ('error','gap')))
)
BEGIN SELECT RAISE(ABORT, 'completed inspection_run must freeze one valid result per configured check'); END;
CREATE TRIGGER trg_execution_attempts_terminal_immutable BEFORE UPDATE OF state ON execution_attempts
WHEN OLD.state IN ('Succeeded','Failed','Cancelled','Interrupted') AND NEW.state <> OLD.state
BEGIN SELECT RAISE(ABORT, 'execution_attempt terminal state is immutable'); END;
CREATE TRIGGER trg_execution_attempts_state_transition BEFORE UPDATE OF state ON execution_attempts
WHEN NEW.state <> OLD.state AND NOT (
  (OLD.state = 'Queued' AND NEW.state IN ('Assigned','Failed','Cancelled'))
  OR (OLD.state = 'Queued' AND NEW.state = 'Succeeded' AND OLD.runtime_slot IS NULL AND (
    OLD.attempt_type = 'inspection_collection' AND
      EXISTS (SELECT 1 FROM inspection_check_results r WHERE r.attempt_id = OLD.id AND r.result_digest IS NOT NULL)
  ))
  OR (OLD.state = 'Assigned' AND NEW.state IN ('Running','Failed','Cancelling','Interrupted'))
  OR (OLD.state = 'Running' AND NEW.state IN ('Succeeded','Failed','Cancelling','Interrupted'))
  OR (OLD.state = 'Cancelling' AND NEW.state = 'Cancelled')
)
BEGIN SELECT RAISE(ABORT, 'invalid execution_attempt state transition'); END;
CREATE TRIGGER trg_model_call_output_closure BEFORE INSERT ON model_call_outputs
WHEN NOT EXISTS (SELECT 1 FROM model_calls m WHERE m.id = NEW.model_call_id AND m.status = 'running')
BEGIN SELECT RAISE(ABORT, 'model call output must be sealed while its physical request is running'); END;
CREATE TRIGGER trg_model_call_output_terminal_coupling BEFORE INSERT ON model_call_outputs
WHEN NEW.complete = 1 AND NOT EXISTS (
  SELECT 1 FROM model_calls m
  WHERE m.id = NEW.model_call_id
    AND m.status = 'running'
    AND m.ended_at IS NULL
    AND m.termination_reason IS NULL
)
BEGIN SELECT RAISE(ABORT, 'complete model output requires its Model Call to be nonterminal until the enclosing transaction succeeds it'); END;
CREATE TRIGGER trg_model_calls_status_transition BEFORE UPDATE OF status ON model_calls
WHEN NEW.status <> OLD.status AND NOT (OLD.status = 'running' AND NEW.status IN ('succeeded','failed','cancelled'))
BEGIN SELECT RAISE(ABORT, 'model_call status transition must be running -> terminal'); END;
CREATE TRIGGER trg_model_call_output_shape BEFORE INSERT ON model_call_outputs
WHEN EXISTS (
  SELECT 1 FROM model_calls m
  WHERE m.id = NEW.model_call_id AND m.operation = 'chat'
    AND (json_type(NEW.response_json) IS NOT 'object'
      OR json_type(NEW.response_json, '$.tool_calls') IS NOT 'array')
)
BEGIN SELECT RAISE(ABORT, 'chat model response_json must be an object with a tool_calls array, including an empty array'); END;
CREATE TRIGGER trg_model_call_success_output BEFORE UPDATE OF status ON model_calls
WHEN NEW.status = 'succeeded' AND NOT EXISTS (
  SELECT 1 FROM model_call_outputs o WHERE o.model_call_id = NEW.id AND o.complete = 1)
BEGIN SELECT RAISE(ABORT, 'succeeded model call requires one complete sealed response'); END;
CREATE TRIGGER trg_model_call_success_input BEFORE UPDATE OF status ON model_calls
WHEN NEW.status = 'succeeded' AND (
  NOT EXISTS (SELECT 1 FROM model_call_input_items i WHERE i.model_call_id = NEW.id)
  OR (NEW.operation = 'chat' AND (
      NOT EXISTS (SELECT 1 FROM model_call_input_items i WHERE i.model_call_id = NEW.id AND i.synthetic_kind = 'system_contract')
      OR NOT EXISTS (SELECT 1 FROM model_call_input_items i WHERE i.model_call_id = NEW.id AND i.synthetic_kind = 'tool_schema')))
)
BEGIN SELECT RAISE(ABORT, 'succeeded model call requires persisted input lineage and chat contract items'); END;
CREATE TRIGGER trg_model_call_non_success_output BEFORE UPDATE OF status ON model_calls
WHEN NEW.status IN ('failed','cancelled') AND EXISTS (
  SELECT 1 FROM model_call_outputs o WHERE o.model_call_id = NEW.id AND o.complete = 1)
BEGIN SELECT RAISE(ABORT, 'failed or cancelled model call cannot expose a complete response'); END;
CREATE TRIGGER trg_model_calls_terminal_immutable BEFORE UPDATE OF status ON model_calls
WHEN OLD.status IN ('succeeded','failed','cancelled') AND NEW.status <> OLD.status
BEGIN SELECT RAISE(ABORT, 'model_call terminal status is immutable'); END;
CREATE TRIGGER trg_model_calls_terminal_result_immutable BEFORE UPDATE OF
  provider_request_id, usage_json, latency_ms, termination_reason, ended_at ON model_calls
WHEN OLD.status IN ('succeeded','failed','cancelled') AND (
  NEW.provider_request_id IS NOT OLD.provider_request_id OR NEW.usage_json IS NOT OLD.usage_json
  OR NEW.latency_ms IS NOT OLD.latency_ms OR NEW.termination_reason IS NOT OLD.termination_reason
  OR NEW.ended_at IS NOT OLD.ended_at)
BEGIN SELECT RAISE(ABORT, 'model_call terminal result is immutable'); END;
CREATE TRIGGER trg_model_calls_provider_request_id_once BEFORE UPDATE OF provider_request_id ON model_calls
WHEN OLD.provider_request_id IS NOT NULL OR NEW.provider_request_id IS NULL
BEGIN SELECT RAISE(ABORT, 'provider_request_id may only be recorded once'); END;
CREATE TRIGGER trg_model_calls_insert_running BEFORE INSERT ON model_calls
WHEN NEW.status <> 'running'
BEGIN SELECT RAISE(ABORT, 'model_call must be created running before provider I/O'); END;
CREATE TRIGGER trg_model_call_retry_closure BEFORE INSERT ON model_calls
WHEN (NEW.retry_seq = 0 AND EXISTS (
       SELECT 1 FROM model_calls m WHERE m.attempt_id = NEW.attempt_id AND m.call_seq = NEW.call_seq))
   OR (NEW.retry_seq > 0 AND NOT EXISTS (
       SELECT 1 FROM model_calls prior
       WHERE prior.attempt_id = NEW.attempt_id AND prior.call_seq = NEW.call_seq
         AND prior.retry_seq = NEW.retry_seq - 1 AND prior.status = 'failed'
         AND NOT EXISTS (SELECT 1 FROM model_call_outputs o WHERE o.model_call_id = prior.id)
         AND prior.operation = NEW.operation AND prior.model_id = NEW.model_id
         AND prior.connection_grant_id = NEW.connection_grant_id
         AND ((prior.termination_reason = 'context_overflow'
               AND NEW.input_snapshot_digest = prior.input_snapshot_digest
               AND NEW.evicted_turn_count > prior.evicted_turn_count
               AND NEW.rendered_request_digest <> prior.rendered_request_digest)
OR (prior.termination_reason IN ('transport_error','timeout','rate_limited')
                AND NEW.input_snapshot_digest = prior.input_snapshot_digest
               AND NEW.evicted_turn_count = prior.evicted_turn_count
               AND NEW.rendered_request_digest = prior.rendered_request_digest))))
BEGIN SELECT RAISE(ABORT, 'model call retry must follow the immediately prior immutable failed physical request'); END;
CREATE TRIGGER trg_model_call_sequence_closure BEFORE INSERT ON model_calls
WHEN NEW.retry_seq = 0 AND EXISTS (
  SELECT 1 FROM execution_attempts a
  WHERE a.id = NEW.attempt_id AND a.attempt_type <> 'connection_probe'
) AND (
  NEW.call_seq < 1
  OR (NEW.call_seq = 1 AND EXISTS (
    SELECT 1 FROM model_calls m WHERE m.attempt_id = NEW.attempt_id
  ))
  OR (NEW.call_seq > 1 AND NOT EXISTS (
    SELECT 1 FROM model_calls prior
    JOIN model_call_outputs output ON output.model_call_id = prior.id AND output.complete = 1
    WHERE prior.attempt_id = NEW.attempt_id
      AND prior.call_seq = NEW.call_seq - 1
      AND prior.status = 'succeeded'
      AND (
        NEW.operation = 'embedding'
        OR (
          json_type(output.response_json, '$.tool_calls') = 'array'
          AND json_array_length(output.response_json, '$.tool_calls') =
              (SELECT count(*) FROM tool_calls t WHERE t.model_call_id = prior.id)
          AND NOT EXISTS (
            SELECT 1 FROM tool_calls t
            WHERE t.model_call_id = prior.id
              AND (t.status NOT IN ('succeeded','failed','cancelled')
                OR (t.status = 'failed' AND t.failure_mode = 'fail_attempt'))
          )
        )
      )
  ))
)
BEGIN SELECT RAISE(ABORT, 'model call sequence must start at one and continue only after every proposed Tool Call is materialized, terminal, and continuable'); END;
CREATE TRIGGER trg_tool_calls_insert_pending BEFORE INSERT ON tool_calls
WHEN NEW.status <> 'pending'
BEGIN SELECT RAISE(ABORT, 'tool_call must be created pending before any execution'); END;
CREATE TRIGGER trg_tool_calls_status_transition BEFORE UPDATE OF status ON tool_calls
WHEN NEW.status <> OLD.status AND NOT (
  (OLD.status = 'pending' AND NEW.status IN ('running','cancelled'))
  OR (OLD.status = 'running' AND NEW.status IN ('succeeded','failed','cancelled')))
BEGIN SELECT RAISE(ABORT, 'tool_call status transition must follow pending -> running -> terminal'); END;
CREATE TRIGGER trg_tool_call_begin_sequence BEFORE UPDATE OF status ON tool_calls
WHEN OLD.status = 'pending' AND NEW.status = 'running' AND (
  NOT EXISTS (SELECT 1 FROM execution_attempts a WHERE a.id = NEW.attempt_id AND a.state = 'Running')
  OR EXISTS (SELECT 1 FROM tool_calls prior
             WHERE prior.model_call_id = NEW.model_call_id AND prior.tool_index < NEW.tool_index
               AND (prior.status NOT IN ('succeeded','failed','cancelled')
                    OR (prior.status = 'failed' AND prior.failure_mode = 'fail_attempt')))
)
BEGIN SELECT RAISE(ABORT, 'Tool Call may start only while its Attempt is Running and every previous Tool Call is terminal and continuable'); END;
CREATE TRIGGER trg_tool_calls_terminal_immutable BEFORE UPDATE OF status ON tool_calls
WHEN OLD.status IN ('succeeded','failed','cancelled') AND NEW.status <> OLD.status
BEGIN SELECT RAISE(ABORT, 'tool_call terminal status is immutable'); END;
CREATE TRIGGER trg_tool_calls_terminal_result_immutable BEFORE UPDATE OF
  result_json, result_artifact_id, error_detail, started_at, ended_at ON tool_calls
WHEN OLD.status IN ('succeeded','failed','cancelled') AND (
  NEW.result_json IS NOT OLD.result_json OR NEW.result_artifact_id IS NOT OLD.result_artifact_id
  OR NEW.error_detail IS NOT OLD.error_detail OR NEW.started_at IS NOT OLD.started_at OR NEW.ended_at IS NOT OLD.ended_at)
BEGIN SELECT RAISE(ABORT, 'tool_call terminal result is immutable'); END;
CREATE TRIGGER trg_tool_call_result_artifact_closure BEFORE UPDATE OF status, result_artifact_id ON tool_calls
WHEN NEW.result_artifact_id IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM artifacts ar WHERE ar.id = NEW.result_artifact_id
    AND ar.kind = 'tool_result' AND ar.retention_kind = 'generated'
    AND ar.owner_type = 'tool_call' AND ar.owner_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'tool_call result Artifact must be its generated tool_result Artifact'); END;
CREATE TRIGGER trg_knowledge_import_batches_insert_processing BEFORE INSERT ON knowledge_import_batches
WHEN NEW.state <> 'Processing'
BEGIN SELECT RAISE(ABORT, 'knowledge_import_batch must be created Processing'); END;
CREATE TRIGGER trg_knowledge_import_batches_terminal_immutable BEFORE UPDATE OF state ON knowledge_import_batches
WHEN OLD.state IN ('Failed','Completed','Cancelled') AND NEW.state <> OLD.state
BEGIN SELECT RAISE(ABORT, 'knowledge_import_batch terminal state is immutable'); END;
CREATE TRIGGER trg_knowledge_import_batches_state_transition BEFORE UPDATE OF state ON knowledge_import_batches
WHEN NEW.state <> OLD.state AND NOT (
  (OLD.state = 'Processing' AND NEW.state IN ('AwaitingConfirmation','Failed','Cancelled'))
  OR (OLD.state = 'AwaitingConfirmation' AND NEW.state IN ('Processing','Completed','Cancelled'))
)
BEGIN SELECT RAISE(ABORT, 'invalid knowledge_import_batch state transition'); END;
CREATE TRIGGER trg_knowledge_candidates_terminal_immutable BEFORE UPDATE OF state ON knowledge_candidates
WHEN OLD.state IN ('Confirmed','Excluded','Superseded','SourceInvalid') AND NEW.state <> OLD.state
BEGIN SELECT RAISE(ABORT, 'knowledge_candidate terminal state is immutable'); END;
CREATE TRIGGER trg_label_contracts_state_derived BEFORE UPDATE OF state ON label_contracts
WHEN NEW.state <> OLD.state AND NOT (
  (OLD.state = 'draft' AND NEW.state = 'active'
    AND NEW.activated_at IS NOT NULL
    AND EXISTS (SELECT 1 FROM label_contract_state WHERE current_contract_id = NEW.id))
  OR (OLD.state = 'active' AND NEW.state = 'retired'
    AND NOT EXISTS (SELECT 1 FROM label_contract_state WHERE current_contract_id = NEW.id))
)
BEGIN SELECT RAISE(ABORT, 'label_contract state is derived from the label_contract_state pointer and cannot be forged'); END;
CREATE TRIGGER trg_label_contracts_insert_state_draft BEFORE INSERT ON label_contracts
WHEN NEW.state <> 'draft' OR NEW.activated_at IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'label_contract must be created as an unactivated draft'); END;
CREATE TRIGGER trg_label_contracts_activated_at_once BEFORE UPDATE OF activated_at ON label_contracts
WHEN (OLD.activated_at IS NOT NULL AND NEW.activated_at IS NOT OLD.activated_at)
  OR (OLD.activated_at IS NULL AND NEW.activated_at IS NOT NULL
      AND NOT (OLD.state = 'draft' AND NEW.state = 'active'))
BEGIN SELECT RAISE(ABORT, 'label_contract activated_at is a derived one-time fact written only while entering active'); END;
CREATE TRIGGER trg_business_config_versions_state_derived BEFORE UPDATE OF state ON business_system_config_versions
WHEN NEW.state <> OLD.state AND NOT (
  (OLD.state = 'draft' AND NEW.state = 'published'
    AND NEW.published_at IS NOT NULL
    AND EXISTS (SELECT 1 FROM business_systems WHERE current_config_version_id = NEW.id))
  OR (OLD.state = 'published' AND NEW.state = 'superseded'
    AND NOT EXISTS (SELECT 1 FROM business_systems WHERE current_config_version_id = NEW.id))
)
BEGIN SELECT RAISE(ABORT, 'config version state is derived from the business_systems current pointer and cannot be forged'); END;
CREATE TRIGGER trg_business_config_versions_insert_state_draft BEFORE INSERT ON business_system_config_versions
WHEN NEW.state <> 'draft' OR NEW.published_at IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'config version must be created as an unpublished draft'); END;
CREATE TRIGGER trg_business_config_versions_published_at_once BEFORE UPDATE OF published_at ON business_system_config_versions
WHEN (OLD.published_at IS NOT NULL AND NEW.published_at IS NOT OLD.published_at)
  OR (OLD.published_at IS NULL AND NEW.published_at IS NOT NULL
      AND NOT (OLD.state = 'draft' AND NEW.state = 'published'))
BEGIN SELECT RAISE(ABORT, 'config version published_at is a derived one-time fact written only while entering published'); END;
CREATE TRIGGER trg_business_config_versions_terminal_superseded BEFORE UPDATE OF state ON business_system_config_versions
WHEN OLD.state = 'superseded' AND NEW.state <> 'superseded'
BEGIN SELECT RAISE(ABORT, 'superseded config version is terminal'); END;
-- 配置版本 system_key 必须等于所属业务系统的稳定 key（DATA-CONFIG-003）。
CREATE TRIGGER trg_business_config_versions_system_key_match BEFORE INSERT ON business_system_config_versions
WHEN NEW.system_key <> (SELECT key FROM business_systems WHERE id = NEW.business_system_id)
BEGIN SELECT RAISE(ABORT, 'config version system_key must equal the business system stable key'); END;
CREATE TRIGGER trg_business_config_versions_system_key_match_update BEFORE UPDATE OF system_key ON business_system_config_versions
WHEN NEW.system_key <> (SELECT key FROM business_systems WHERE id = NEW.business_system_id)
BEGIN SELECT RAISE(ABORT, 'config version system_key must equal the business system stable key'); END;
CREATE TRIGGER trg_label_contracts_retired_terminal BEFORE UPDATE OF state ON label_contracts
WHEN OLD.state = 'retired' AND NEW.state <> 'retired'
BEGIN SELECT RAISE(ABORT, 'retired label_contract is terminal'); END;

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

-- runtime_artifact_uploads：来源字段不可改写（含 boot_id）；只能以 uploading 创建，状态转换仅
-- uploading->committed/rejected 且终态不可变；committed 必须满足 NULL-safe 正向条件：所引 Attempt
-- 普通上传必须绑定 state='Running' Attempt 且 runtime_slot/boot_id/connection_epoch 精确一致。
-- 旧 Attempt、不同 boot 或不一致 epoch 一律拒绝 commit，只能 rejected；
-- artifact_id/committed_at 一旦提交不可改；历史不可删除（DATA-ARTIFACT-006）。
CREATE TRIGGER trg_runtime_artifact_uploads_origin_immutable BEFORE UPDATE OF
  upload_id, attempt_id, boot_id, connection_epoch, owner_type, owner_id, kind, media_type, retention_kind, sensitive, size_bytes, sha256, created_at ON runtime_artifact_uploads
BEGIN SELECT RAISE(ABORT, 'runtime_artifact_upload origin is immutable'); END;
CREATE TRIGGER trg_runtime_artifact_uploads_insert_state BEFORE INSERT ON runtime_artifact_uploads
WHEN NEW.state <> 'uploading'
BEGIN SELECT RAISE(ABORT, 'runtime_artifact_upload must be created as uploading'); END;
CREATE TRIGGER trg_runtime_artifact_uploads_state_transition BEFORE UPDATE OF state ON runtime_artifact_uploads
WHEN OLD.state <> NEW.state AND NOT (OLD.state = 'uploading' AND NEW.state IN ('committed','rejected'))
BEGIN SELECT RAISE(ABORT, 'runtime_artifact_upload state transition only uploading->committed/rejected'); END;
CREATE TRIGGER trg_runtime_artifact_uploads_commit_attempt BEFORE UPDATE OF state ON runtime_artifact_uploads
WHEN NEW.state = 'committed' AND NEW.artifact_id IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM artifacts ar JOIN artifact_blobs b ON b.id = ar.blob_id
  WHERE ar.id = NEW.artifact_id
    AND ar.kind = NEW.kind
    AND ar.media_type = NEW.media_type
    AND ar.retention_kind = NEW.retention_kind
    AND ar.sensitive = NEW.sensitive
    AND ar.owner_type = NEW.owner_type
    AND ar.owner_id = NEW.owner_id
    AND b.sha256 = NEW.sha256
    AND b.size_bytes = NEW.size_bytes
    AND EXISTS (
      SELECT 1 FROM execution_attempts a WHERE a.id = NEW.attempt_id
        AND a.state = 'Running' AND a.runtime_slot IS NOT NULL
        AND a.boot_id = NEW.boot_id
        AND a.connection_epoch = NEW.connection_epoch)
)
BEGIN SELECT RAISE(ABORT, 'runtime_artifact_upload commit requires a matching Running Attempt and an exactly matching artifact'); END;
CREATE TRIGGER trg_runtime_artifact_uploads_tool_result_owner BEFORE INSERT ON runtime_artifact_uploads
WHEN NEW.kind = 'tool_result' AND (
  NEW.owner_type <> 'tool_call' OR NEW.retention_kind <> 'generated' OR NOT EXISTS (
    SELECT 1 FROM tool_calls t WHERE t.id = NEW.owner_id AND t.attempt_id = NEW.attempt_id))
BEGIN SELECT RAISE(ABORT, 'tool_result upload must be generated and owned by a Tool Call of the same Attempt'); END;
CREATE TRIGGER trg_runtime_artifact_uploads_result_immutable BEFORE UPDATE OF artifact_id, committed_at ON runtime_artifact_uploads
WHEN OLD.artifact_id IS NOT NULL AND (NEW.artifact_id IS NOT OLD.artifact_id OR NEW.committed_at IS NOT OLD.committed_at)
BEGIN SELECT RAISE(ABORT, 'runtime_artifact_upload result is immutable once committed'); END;
CREATE TRIGGER trg_runtime_artifact_uploads_no_delete BEFORE DELETE ON runtime_artifact_uploads
BEGIN SELECT RAISE(ABORT, 'runtime_artifact_uploads ledger is not deletable'); END;

CREATE TRIGGER trg_attempt_artifact_grants_closure BEFORE INSERT ON attempt_artifact_grants
WHEN NOT EXISTS (SELECT 1 FROM execution_attempts a JOIN artifacts ar ON ar.id = NEW.artifact_id
                 WHERE a.id = NEW.attempt_id AND ar.body_expired = 0
                   AND ((NEW.source_kind = 'input_snapshot' AND a.state = 'Queued')
                     OR (NEW.source_kind IN ('tool_result','evidence') AND a.state = 'Running')))
  OR (NEW.source_kind = 'input_snapshot' AND NOT EXISTS (
      SELECT 1 FROM attempt_input_snapshots s JOIN attempt_input_items i ON i.snapshot_id = s.id
      WHERE s.id = NEW.source_id AND s.attempt_id = NEW.attempt_id AND i.artifact_id = NEW.artifact_id))
  OR (NEW.source_kind = 'tool_result' AND NOT EXISTS (
      SELECT 1 FROM tool_calls t JOIN artifacts ar ON ar.id = NEW.artifact_id
      WHERE t.id = NEW.source_id AND t.attempt_id = NEW.attempt_id AND t.status = 'succeeded'
        AND ar.owner_type = 'tool_call' AND ar.owner_id = t.id))
  OR (NEW.source_kind = 'evidence' AND NOT EXISTS (
      SELECT 1 FROM evidence e WHERE e.id = NEW.source_id AND e.artifact_id = NEW.artifact_id
        AND (e.attempt_id = NEW.attempt_id OR EXISTS (
          SELECT 1 FROM attempt_input_snapshots s JOIN attempt_input_items i ON i.snapshot_id = s.id
          WHERE s.attempt_id = NEW.attempt_id AND i.evidence_id = e.id))))
BEGIN SELECT RAISE(ABORT, 'Attempt Artifact grant must bind a Running Attempt to an unexpired input, Tool Result, or Evidence Artifact'); END;
CREATE TRIGGER trg_attempt_artifact_grants_no_update BEFORE UPDATE ON attempt_artifact_grants
BEGIN SELECT RAISE(ABORT, 'Attempt Artifact grants are append-only'); END;
CREATE TRIGGER trg_attempt_artifact_grants_no_delete BEFORE DELETE ON attempt_artifact_grants
BEGIN SELECT RAISE(ABORT, 'Attempt Artifact grants are retained as immutable access lineage'); END;

CREATE TRIGGER trg_execution_attempts_run_check_slot_kind BEFORE UPDATE OF runtime_slot ON execution_attempts
WHEN NEW.attempt_type = 'inspection_collection' AND NEW.scope_type = 'run_check' AND NOT EXISTS (
  SELECT 1 FROM inspection_runs r
  JOIN config_plans p ON p.config_version_id = r.config_version_id AND p.plan_key = r.plan_key
  JOIN config_checks c ON c.plan_id = p.id AND c.check_key = NEW.check_key
  WHERE r.id = NEW.scope_id AND r.state = 'Running'
    AND c.kind = 'promql' AND NEW.runtime_slot = 'plinth'
  UNION ALL
  -- 独立计划 Run：插件采集固定由 Plinth supervisor 执行。
  SELECT 1 FROM inspection_runs r
  JOIN inspection_run_checks c ON c.run_id = r.id AND c.check_key = NEW.check_key
  WHERE r.id = NEW.scope_id AND r.state = 'Running' AND r.plan_id IS NOT NULL
    AND NEW.runtime_slot = 'plinth'
)
BEGIN SELECT RAISE(ABORT, 'inspection run check attempt slot must match its check kind'); END;

-- 派发绑定（runtime_slot/boot_id/connection_epoch/accepted_at）一旦设置不可改；
-- lease_until 可由心跳续期（可再生，row_version 照常递增）。
CREATE TRIGGER trg_execution_attempts_runtime_binding_immutable BEFORE UPDATE OF runtime_slot, boot_id, connection_epoch, accepted_at ON execution_attempts
WHEN (OLD.runtime_slot IS NOT NULL AND NEW.runtime_slot IS NOT OLD.runtime_slot)
  OR (OLD.boot_id IS NOT NULL AND NEW.boot_id IS NOT OLD.boot_id)
  OR (OLD.connection_epoch IS NOT NULL AND NEW.connection_epoch IS NOT OLD.connection_epoch)
  OR (OLD.accepted_at IS NOT NULL AND NEW.accepted_at IS NOT OLD.accepted_at)
BEGIN SELECT RAISE(ABORT, 'execution_attempt runtime binding is immutable once set'); END;

-- 12.38 Label Contract 原子激活（DATA-CONFIG-002/006）：单个顶层 INSERT 触发 AFTER INSERT，
-- 在同一 statement 中重验全部前提并原子切换全部系统指针、更新 label_contract_state。
-- 任一 RAISE(ABORT) 回滚该 INSERT 及全部副作用。
CREATE TRIGGER trg_label_contract_activations_insert_unapplied BEFORE INSERT ON label_contract_activations
WHEN NEW.applied_at IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'label_contract_activation must be inserted unapplied'); END;
CREATE TRIGGER trg_label_contract_activations_no_content_update BEFORE UPDATE OF contract_id, expected_target_row_version, expected_state_row_version, expected_current_contract_id, items_json, created_at ON label_contract_activations
BEGIN SELECT RAISE(ABORT, 'label_contract_activation command content is immutable'); END;
CREATE TRIGGER trg_label_contract_activations_applied_once BEFORE UPDATE OF applied_at ON label_contract_activations
WHEN OLD.applied_at IS NOT NULL OR NEW.applied_at IS NULL
BEGIN SELECT RAISE(ABORT, 'label_contract_activation applied_at is a one-time fact'); END;
CREATE TRIGGER trg_label_contract_activations_no_delete BEFORE DELETE ON label_contract_activations
BEGIN SELECT RAISE(ABORT, 'label_contract_activation history is not deletable'); END;
CREATE TRIGGER trg_label_contract_state_activate_atomic AFTER INSERT ON label_contract_activations
BEGIN
  -- 1) 命令 JSON 必须是封闭的 item 数组，系统只能出现一次。
  SELECT RAISE(ABORT, 'activation items must be closed objects with typed fields and unique business_system_id')
  WHERE EXISTS (
    SELECT 1 FROM json_each(NEW.items_json) je
    WHERE json_type(je.value) IS NOT 'object'
       OR json_type(je.value, '$.business_system_id') IS NOT 'integer'
       OR json_type(je.value, '$.config_version_id') IS NOT 'integer'
       OR json_type(je.value, '$.verification_run_id') IS NOT 'integer'
       OR json_type(je.value, '$.expected_business_system_row_version') IS NOT 'integer'
       OR COALESCE(json_type(je.value, '$.expected_current_config_version_id'), 'missing') NOT IN ('integer','null')
       OR EXISTS (
         SELECT 1 FROM json_each(je.value) member
         WHERE member.key NOT IN ('business_system_id','config_version_id','verification_run_id','expected_current_config_version_id','expected_business_system_row_version')
       )
  )
  OR (SELECT COUNT(*) FROM json_each(NEW.items_json)) <>
     (SELECT COUNT(DISTINCT CAST(je.value ->> '$.business_system_id' AS INTEGER)) FROM json_each(NEW.items_json) je);
  -- 2) 目标契约必须从未激活
  SELECT RAISE(ABORT, 'activation target must be an unactivated draft')
  WHERE EXISTS (SELECT 1 FROM label_contracts lc WHERE lc.id = NEW.contract_id AND lc.activated_at IS NOT NULL);
  -- 3) 目标契约 row_version 前提匹配
  SELECT RAISE(ABORT, 'activation target row_version mismatch')
  WHERE NOT EXISTS (SELECT 1 FROM label_contracts lc WHERE lc.id = NEW.contract_id AND lc.row_version = NEW.expected_target_row_version);
  -- 4) label_contract_state 前提匹配
  SELECT RAISE(ABORT, 'activation state pointer/row_version mismatch')
  WHERE NOT EXISTS (SELECT 1 FROM label_contract_state s
    WHERE s.id = 1 AND s.row_version = NEW.expected_state_row_version
      AND s.current_contract_id IS NEW.expected_current_contract_id);
  -- 5) 覆盖：启用系统必须全部出现在 items_json，不得含禁用系统
  SELECT RAISE(ABORT, 'activation items must cover every enabled system exactly once and no disabled system')
  WHERE EXISTS (SELECT 1 FROM business_systems bs WHERE bs.enabled = 1 AND NOT EXISTS
    (SELECT 1 FROM json_each(NEW.items_json) je
     WHERE CAST(je.value ->> '$.business_system_id' AS INTEGER) = bs.id))
  OR EXISTS (SELECT 1 FROM json_each(NEW.items_json) je
     JOIN business_systems bs ON bs.id = CAST(je.value ->> '$.business_system_id' AS INTEGER)
     WHERE bs.enabled = 0);
  -- 6) 逐项闭合：config 属于该系统、未发布、以被激活契约为目标；并发前提匹配。
  --    使用 json_each 遍历 items_json 中的每个 item 进行重验。
  --    （Config Verification Run Passed 前置已随验证引擎退役。）
  SELECT RAISE(ABORT, 'activation item validation failed')
  WHERE EXISTS (
    SELECT 1 FROM json_each(NEW.items_json) je
    WHERE NOT EXISTS (
      SELECT 1
      FROM business_systems bs
      JOIN business_system_config_versions v ON v.id = CAST(je.value ->> '$.config_version_id' AS INTEGER)
      WHERE bs.id = CAST(je.value ->> '$.business_system_id' AS INTEGER)
        AND v.business_system_id = bs.id
        AND v.published_at IS NULL
        AND v.label_contract_version_id = NEW.contract_id
        AND (CAST(je.value ->> '$.expected_current_config_version_id' AS INTEGER) IS bs.current_config_version_id
             OR (je.value ->> '$.expected_current_config_version_id' IS NULL AND bs.current_config_version_id IS NULL))
        AND CAST(je.value ->> '$.expected_business_system_row_version' AS INTEGER) = bs.row_version
    )
  );
  -- 7) 原子切换全部系统 current 指针（级联触发 business_systems 指针/投影/row_version/发布派生守卫）
  UPDATE business_systems SET
    current_config_version_id = CAST(je.value ->> '$.config_version_id' AS INTEGER),
    row_version = row_version + 1,
    display_name = (SELECT display_name FROM business_system_config_versions v WHERE v.id = CAST(je.value ->> '$.config_version_id' AS INTEGER)),
    enabled = (SELECT enabled FROM business_system_config_versions v WHERE v.id = CAST(je.value ->> '$.config_version_id' AS INTEGER)),
    timezone = (SELECT timezone FROM business_system_config_versions v WHERE v.id = CAST(je.value ->> '$.config_version_id' AS INTEGER))
  FROM json_each(NEW.items_json) je
  WHERE business_systems.id = CAST(je.value ->> '$.business_system_id' AS INTEGER);
  -- 8) 更新 label_contract_state 指针对；匹配的未应用 activation_id 是唯一内部写入令牌。
  UPDATE label_contract_state SET
    current_contract_id = NEW.contract_id,
    current_activation_id = NEW.id,
    row_version = row_version + 1,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
  WHERE id = 1;
  -- 9) 契约激活事实（一次性）与旧契约退休（派生）
  UPDATE label_contracts SET state = 'active', activated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'), row_version = row_version + 1
  WHERE id = NEW.contract_id AND state = 'draft';
  UPDATE label_contracts SET state = 'retired', row_version = row_version + 1
  WHERE id = NEW.expected_current_contract_id AND NEW.expected_current_contract_id IS NOT NULL AND state = 'active';
  -- 10) 所有副作用完成后才封存 activation；此后该行不能再作为指针变更令牌。
  UPDATE label_contract_activations SET applied_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
  WHERE id = NEW.id;
END;
-- 12.39 巡检运行闭合：手工 Run 消费启用系统的当前已发布配置（历史声明计划），或一个
-- 启用接入上的启用独立计划（ADR-0004）。重新采证是唯一例外：它必须从一个已终止的
-- 同源 Run 逐字段复制不可变绑定，不能被当前指针重写。
CREATE TRIGGER trg_inspection_runs_closure BEFORE INSERT ON inspection_runs
WHEN NOT EXISTS (
  SELECT 1 FROM business_systems b
  JOIN business_system_config_versions v ON v.id = b.current_config_version_id AND v.business_system_id = b.id
  JOIN config_plans p ON p.config_version_id = v.id AND p.plan_key = NEW.plan_key
  WHERE NEW.rerun_of_id IS NULL
    AND b.id = NEW.business_system_id AND b.enabled = 1
    AND v.id = NEW.config_version_id AND v.state = 'published' AND v.published_at IS NOT NULL
  UNION ALL
  -- 独立计划 Run：绑定启用接入上的启用计划并冻结其模板与授权来源。
  SELECT 1 FROM inspection_plans p
  JOIN connections c ON c.id = p.connection_id
  WHERE NEW.rerun_of_id IS NULL
    AND p.id = NEW.plan_id AND p.plan_key = NEW.plan_key AND p.enabled = 1
    AND c.id = NEW.connection_id AND c.enabled = 1
    AND NEW.business_system_id IS NULL AND NEW.config_version_id IS NULL AND NEW.label_contract_version_id IS NULL
  UNION ALL
  SELECT 1 FROM inspection_runs source
  JOIN business_systems b ON b.id = source.business_system_id
  JOIN config_plans p ON p.config_version_id = source.config_version_id AND p.plan_key = source.plan_key
  WHERE NEW.rerun_of_id IS NOT NULL AND NEW.trigger_kind = 'manual'
    AND NEW.scheduled_for IS NULL AND source.id = NEW.rerun_of_id
    AND b.enabled = 1
    AND source.state IN ('Completed','CompletedWithGaps','Failed','Cancelled','Interrupted')
    AND source.business_system_id = NEW.business_system_id
    AND source.config_version_id = NEW.config_version_id
    AND source.plan_key = NEW.plan_key
  UNION ALL
  SELECT 1 FROM inspection_runs source
  JOIN inspection_plans p ON p.id = source.plan_id
  WHERE NEW.rerun_of_id IS NOT NULL AND NEW.trigger_kind = 'manual'
    AND NEW.scheduled_for IS NULL AND source.id = NEW.rerun_of_id
    AND source.plan_id IS NOT NULL AND p.enabled = 1
    AND source.state IN ('Completed','CompletedWithGaps','Failed','Cancelled','Interrupted')
    AND source.plan_id = NEW.plan_id
    AND source.plan_key = NEW.plan_key
)
BEGIN SELECT RAISE(ABORT, 'inspection_run must bind an enabled source: the enabled business system current published config, or an enabled plan on an enabled connection, or exactly copy a terminal source Run'); END;

-- 12.40 execution_attempts 统一从 Queued 创建；输入快照/grant 依赖 Attempt ID，禁止绕过派发事务直接出生为 active/terminal。
CREATE TRIGGER trg_execution_attempts_insert_queued BEFORE INSERT ON execution_attempts
WHEN NEW.state <> 'Queued'
BEGIN SELECT RAISE(ABORT, 'execution_attempt must be created Queued before input freeze and dispatch'); END;

-- execution_attempts 作用域闭合（DATA-ATTEMPT-002）：每种固定工作模式只引用其权威 scope；
-- 发布前 PromQL 与资源刷新都是 supervisor-only inspection_collection Attempt，
-- 因而必须绑定运行中的 Config Verification / Resource Refresh 和该配置中的精确声明。
CREATE TRIGGER trg_execution_attempts_scope_exists BEFORE INSERT ON execution_attempts
WHEN (NEW.scope_type = 'analysis' AND NOT EXISTS (
        SELECT 1 FROM initial_analyses a WHERE a.id = NEW.scope_id AND a.state IN ('Queued','Running')))
   OR (NEW.scope_type = 'investigation' AND NOT EXISTS (
        SELECT 1 FROM investigations i WHERE i.id = NEW.scope_id))
   OR (NEW.scope_type = 'run' AND NOT EXISTS (
        SELECT 1 FROM inspection_runs r WHERE r.id = NEW.scope_id
          AND r.state IN ('Running','Completed','CompletedWithGaps')))
   OR (NEW.scope_type = 'knowledge_import_batch' AND NOT EXISTS (
        SELECT 1 FROM knowledge_import_batches b WHERE b.id = NEW.scope_id AND b.state = 'Processing'))
   OR (NEW.scope_type = 'embedding_generation' AND NOT EXISTS (
        SELECT 1 FROM embedding_generations g WHERE g.id = NEW.scope_id))
    OR (NEW.scope_type = 'connection' AND NOT EXISTS (
         SELECT 1 FROM connections c WHERE c.id = NEW.scope_id))
   OR (NEW.scope_type = 'observation_run' AND NOT EXISTS (
        SELECT 1 FROM observation_runs o JOIN observation_run_objects x ON x.observation_run_id = o.id
        WHERE o.id = NEW.scope_id AND o.state IN ('Queued','Running')
          AND x.object_type = NEW.discovery_key AND x.attempt_id IS NULL))
   OR (NEW.scope_type = 'run_check' AND NOT EXISTS (
        SELECT 1 FROM inspection_runs r JOIN config_plans p ON p.config_version_id = r.config_version_id AND p.plan_key = r.plan_key
        JOIN config_checks c ON c.plan_id = p.id
        WHERE r.id = NEW.scope_id AND r.state = 'Running'
          AND c.check_key = NEW.check_key AND c.kind = 'promql' AND NEW.check_key IS NOT NULL
        UNION ALL
        SELECT 1 FROM inspection_runs r JOIN inspection_run_checks c ON c.run_id = r.id
        WHERE r.id = NEW.scope_id AND r.state = 'Running' AND r.plan_id IS NOT NULL
          AND c.check_key = NEW.check_key AND NEW.check_key IS NOT NULL))
BEGIN SELECT RAISE(ABORT, 'execution_attempt scope must reference the active object required by its fixed work mode'); END;

-- 输入快照只在 Queued 阶段创建，schema_kind 按固定 AttemptType 映射到版本化 wire schema；items 之后同事务追加。
CREATE TRIGGER trg_attempt_input_snapshot_closure BEFORE INSERT ON attempt_input_snapshots
WHEN NOT EXISTS (
  SELECT 1 FROM execution_attempts a
  WHERE a.id = NEW.attempt_id AND a.state = 'Queued'
    AND NEW.schema_kind = CASE a.attempt_type
      WHEN 'initial_analysis' THEN 'initial_analysis_v1'
      WHEN 'investigation' THEN 'investigation_v1'
      WHEN 'inspection_analysis' THEN 'inspection_analysis_v1'
      WHEN 'knowledge_extraction' THEN 'knowledge_extraction_v1'
      WHEN 'embedding' THEN 'embedding_v1'
      WHEN 'inspection_collection' THEN CASE a.scope_type
        WHEN 'observation_run' THEN 'source_observation_execution_v1'
        WHEN 'run_check' THEN CASE WHEN EXISTS (
          SELECT 1 FROM inspection_runs r
          JOIN config_plans p ON p.config_version_id = r.config_version_id AND p.plan_key = r.plan_key
          JOIN config_checks c ON c.plan_id = p.id AND c.check_key = a.check_key
          WHERE r.id = a.scope_id AND c.kind = 'promql')
          THEN 'inspection_promql_execution_v1'
          WHEN EXISTS (
          SELECT 1 FROM inspection_runs r
          WHERE r.id = a.scope_id AND r.plan_id IS NOT NULL)
          THEN 'inspection_plugin_execution_v1'
          ELSE 'inspection_collection_v1' END
        ELSE 'inspection_collection_v1'
      END
      WHEN 'connection_probe' THEN 'connection_probe_v1'
    END)
  OR (NEW.schema_kind = 'inspection_analysis_v1') <> (NEW.inspection_report_version IS NOT NULL)
BEGIN SELECT RAISE(ABORT, 'attempt input snapshot schema_kind must match the versioned schema of the same Queued Attempt type'); END;
CREATE TRIGGER trg_attempt_input_item_closure BEFORE INSERT ON attempt_input_items
WHEN NOT EXISTS (
  SELECT 1 FROM attempt_input_snapshots s JOIN execution_attempts a ON a.id = s.attempt_id
  WHERE s.id = NEW.snapshot_id AND a.state = 'Queued'
    AND (NEW.connection_revision_id IS NULL OR (
      (a.attempt_type = 'connection_probe' AND EXISTS (
        SELECT 1 FROM connection_revisions r WHERE r.id = NEW.connection_revision_id AND r.connection_id = a.scope_id))
      -- 来源级观测与独立计划 run_check：冻结其运行绑定的接入修订作为来源
      -- 谱系；修订必须精确归属于该 Attempt scope 的连接，不给宽泛例外。
      OR (a.attempt_type = 'inspection_collection' AND a.scope_type = 'observation_run' AND EXISTS (
        SELECT 1 FROM connection_revisions r
        JOIN observation_runs o ON o.id = a.scope_id AND o.connection_id = r.connection_id
        WHERE r.id = NEW.connection_revision_id))
      OR (a.attempt_type = 'inspection_collection' AND a.scope_type = 'run_check' AND EXISTS (
        SELECT 1 FROM connection_revisions r
        JOIN inspection_runs ir ON ir.id = a.scope_id AND ir.plan_id IS NOT NULL AND ir.connection_id = r.connection_id
        WHERE r.id = NEW.connection_revision_id))
      -- Agent 冻结的来源绑定：Attempt 创建时读取当前启用的来源修订精确冻结，
      -- item_role 决定平台类型；不依赖任何已存在的 grant（tool grant 发生在
      -- 之后的 ModelToolCall，二者无顺序循环）。后续 grant 必须匹配该冻结项。
      OR (NEW.item_role = 'metrics_source' AND EXISTS (
        SELECT 1 FROM connection_revisions r
        JOIN connections c ON c.id = r.connection_id
          AND c.type IN ('prometheus','thanos') AND c.enabled = 1 AND c.current_revision_id = r.id
        WHERE r.id = NEW.connection_revision_id))
    )))
BEGIN SELECT RAISE(ABORT, 'attempt input items may only be frozen for the same Queued Attempt and valid fixed-mode source'); END;
-- 派发前必须已经存在可重建的输入谱系与固定工作模式版本；Plinth 模型工作还必须绑定真实探测通过的模型 grant。
CREATE TRIGGER trg_execution_attempts_dispatch_ready BEFORE UPDATE OF state ON execution_attempts
WHEN OLD.state = 'Queued' AND NEW.state = 'Assigned' AND (
  NOT EXISTS (SELECT 1 FROM attempt_input_snapshots s JOIN attempt_input_items i ON i.snapshot_id = s.id WHERE s.attempt_id = NEW.id)
  OR EXISTS (SELECT 1 FROM attempt_input_snapshots s JOIN attempt_input_items i ON i.snapshot_id = s.id
             WHERE s.attempt_id = NEW.id AND i.artifact_id IS NOT NULL
               AND NOT EXISTS (SELECT 1 FROM attempt_artifact_grants g
                               WHERE g.attempt_id = NEW.id AND g.artifact_id = i.artifact_id AND g.source_kind = 'input_snapshot' AND g.source_id = s.id))
  OR NEW.quoin_release_version = ''
  OR (NEW.attempt_type IN ('initial_analysis','investigation','inspection_analysis','knowledge_extraction')
      AND (NEW.runtime_slot <> 'plinth' OR NEW.agent_version IS NULL OR NOT EXISTS (
        SELECT 1 FROM attempt_connection_grants g WHERE g.attempt_id = NEW.id AND g.purpose = 'chat_model')))
  OR (NEW.attempt_type = 'embedding' AND (NEW.runtime_slot <> 'plinth' OR NEW.agent_version IS NOT NULL OR NOT EXISTS (
      SELECT 1 FROM attempt_connection_grants g WHERE g.attempt_id = NEW.id AND g.purpose = 'embedding')))
  OR (NEW.attempt_type = 'connection_probe' AND (
      NEW.runtime_slot <> 'plinth' OR NEW.agent_version IS NOT NULL
      OR NOT EXISTS (
        SELECT 1 FROM connections c WHERE c.id = NEW.scope_id AND (
          (c.type = 'model_provider'
            AND EXISTS (SELECT 1 FROM attempt_connection_grants g WHERE g.attempt_id = NEW.id AND g.connection_id = c.id AND g.purpose = 'model_probe_chat')
            AND EXISTS (SELECT 1 FROM attempt_connection_grants g WHERE g.attempt_id = NEW.id AND g.connection_id = c.id AND g.purpose = 'model_probe_embedding'))
          OR (c.type = 'prometheus'
            AND EXISTS (SELECT 1 FROM attempt_connection_grants g WHERE g.attempt_id = NEW.id AND g.connection_id = c.id AND g.purpose = 'prometheus_probe'))
          OR (c.type = 'thanos'
            AND EXISTS (SELECT 1 FROM attempt_connection_grants g WHERE g.attempt_id = NEW.id AND g.connection_id = c.id AND g.purpose = 'thanos_probe'))
        )
      )))
  OR (NEW.attempt_type = 'inspection_collection' AND NEW.scope_type = 'run_check'
      AND EXISTS (
        SELECT 1 FROM inspection_runs r
        JOIN config_plans p ON p.config_version_id = r.config_version_id AND p.plan_key = r.plan_key
        JOIN config_checks c ON c.plan_id = p.id AND c.check_key = NEW.check_key
        WHERE r.id = NEW.scope_id AND c.kind = 'promql'
      )
      AND NOT EXISTS (
        SELECT 1
        FROM attempt_connection_grants g
        JOIN connections connection ON connection.id = g.connection_id
        JOIN connection_revisions revision ON revision.id = g.connection_revision_id
          AND revision.connection_id = connection.id
        JOIN credential_generations credential ON credential.id = g.credential_generation_id
          AND credential.connection_id = connection.id
        JOIN root_key_state root_key ON root_key.id = 1
        WHERE g.attempt_id = NEW.id AND g.purpose = 'config_thanos_query'
          AND connection.type IN ('prometheus','thanos') AND connection.enabled = 1 AND connection.revalidation_required = 0
          AND connection.current_revision_id = revision.id
          AND connection.current_credential_generation_id = credential.id
          AND credential.key_binding_revision = root_key.binding_revision
      ))
)
BEGIN SELECT RAISE(ABORT, 'attempt cannot dispatch without frozen input, release binding, and required model grant'); END;

-- Investigation 的 user/assistant 消息都绑定本轮唯一 Attempt；用户消息先创建，成功 proposal 才追加 assistant 消息。
CREATE TRIGGER trg_investigation_messages_attempt_closure BEFORE INSERT ON investigation_messages
WHEN NOT EXISTS (
  SELECT 1 FROM execution_attempts a
  WHERE a.id = NEW.attempt_id AND a.attempt_type = 'investigation'
    AND a.scope_type = 'investigation' AND a.scope_id = NEW.investigation_id
)
BEGIN SELECT RAISE(ABORT, 'investigation message must bind an investigation Attempt of the same investigation'); END;
CREATE TRIGGER trg_investigation_user_message_attempt BEFORE INSERT ON investigation_messages
WHEN NEW.role = 'user' AND NOT EXISTS (
  SELECT 1 FROM execution_attempts a WHERE a.id = NEW.attempt_id AND a.state = 'Queued')
BEGIN SELECT RAISE(ABORT, 'user message must be committed with its newly Queued Attempt'); END;
CREATE TRIGGER trg_investigation_assistant_message_success BEFORE INSERT ON investigation_messages
WHEN NEW.role = 'assistant' AND NOT EXISTS (
  SELECT 1 FROM execution_attempts a WHERE a.id = NEW.attempt_id AND a.state = 'Running')
BEGIN SELECT RAISE(ABORT, 'assistant message is committed only while its Attempt is Running in the same result transaction'); END;

CREATE TRIGGER trg_initial_analysis_output_attempt BEFORE INSERT ON initial_analysis_outputs
WHEN NOT EXISTS (
  SELECT 1 FROM execution_attempts a JOIN initial_analyses ia ON ia.id = a.scope_id
  WHERE a.id = NEW.attempt_id AND a.attempt_type = 'initial_analysis' AND a.scope_type = 'analysis'
    AND ia.id = NEW.analysis_id AND a.state = 'Running'
)
BEGIN SELECT RAISE(ABORT, 'initial analysis output must bind its Running analysis Attempt'); END;
CREATE TRIGGER trg_inspection_report_attempt BEFORE INSERT ON inspection_reports
WHEN NOT EXISTS (
  SELECT 1 FROM execution_attempts a WHERE a.id = NEW.attempt_id
    AND a.attempt_type = 'inspection_analysis' AND a.scope_type = 'run' AND a.scope_id = NEW.run_id
    AND a.state = 'Running'
)
BEGIN SELECT RAISE(ABORT, 'inspection report must bind its Running inspection_analysis Attempt'); END;

-- Connection Probe header/typed child 闭合到同一 connection/revision/generation/action set。
CREATE TRIGGER trg_connection_probe_results_closure BEFORE INSERT ON connection_probe_results
WHEN NOT EXISTS (
  SELECT 1 FROM connections c
  JOIN connection_revisions r ON r.id = NEW.connection_revision_id AND r.connection_id = c.id
  JOIN credential_generations g ON g.id = NEW.credential_generation_id AND g.connection_id = c.id
  JOIN execution_attempts a ON a.id = NEW.attempt_id
  WHERE c.id = NEW.connection_id AND c.type = NEW.connection_type
    AND g.key_binding_revision = NEW.root_binding_revision
    AND a.attempt_type = 'connection_probe' AND a.scope_type = 'connection' AND a.scope_id = c.id
    AND a.state = 'Running'
    AND EXISTS (
      SELECT 1 FROM attempt_connection_grants ag
      WHERE ag.attempt_id = a.id AND ag.connection_id = c.id
        AND ag.connection_revision_id = r.id AND ag.credential_generation_id = g.id
        AND ag.qualified_probe_result_id IS NULL
        AND ag.purpose = CASE c.type
          WHEN 'model_provider' THEN 'model_probe_chat'
          WHEN 'prometheus' THEN 'prometheus_probe'
          WHEN 'thanos' THEN 'thanos_probe'
        END)
)
BEGIN SELECT RAISE(ABORT, 'connection probe result must close over its Running supervisor probe Attempt and exact connection binding'); END;
CREATE TRIGGER trg_connection_probe_attempt_terminal_closure BEFORE UPDATE OF state ON execution_attempts
WHEN OLD.attempt_type = 'connection_probe' AND NEW.state IN ('Succeeded','Failed','Cancelled','Interrupted')
  AND NOT EXISTS (
    SELECT 1 FROM connection_probe_results p
    WHERE p.attempt_id = OLD.id
      AND p.outcome = CASE NEW.state
        WHEN 'Succeeded' THEN 'passed'
        WHEN 'Failed' THEN 'failed'
        WHEN 'Cancelled' THEN 'cancelled'
        WHEN 'Interrupted' THEN 'interrupted'
      END
      AND ((p.connection_type = 'model_provider' AND EXISTS (
              SELECT 1 FROM model_provider_connection_probe_results x WHERE x.probe_result_id = p.id))
        OR (p.connection_type IN ('prometheus','thanos') AND EXISTS (
              SELECT 1 FROM thanos_connection_probe_results x WHERE x.probe_result_id = p.id)))
  )
BEGIN SELECT RAISE(ABORT, 'connection probe Attempt terminal state requires one matching immutable typed result'); END;
CREATE TRIGGER trg_model_provider_connection_probe_results_closure BEFORE INSERT ON model_provider_connection_probe_results
WHEN NOT EXISTS (
  SELECT 1 FROM connection_probe_results p
  JOIN connection_revisions r ON r.id = p.connection_revision_id
  WHERE p.id = NEW.probe_result_id AND p.connection_type = 'model_provider'
    AND json_extract(r.config_json, '$.chatModelId') = NEW.chat_model_id
    AND (json_type(r.config_json, '$.embeddingModelId') IS NULL OR json_extract(r.config_json, '$.embeddingModelId') = NEW.embedding_model_id)
    AND (json_type(r.config_json, '$.contextBudgetTokens') IS NULL OR json_extract(r.config_json, '$.contextBudgetTokens') = NEW.context_budget_tokens)
    AND (json_type(r.config_json, '$.maxOutputTokens') IS NULL OR json_extract(r.config_json, '$.maxOutputTokens') = NEW.max_output_tokens)
    AND EXISTS (SELECT 1 FROM model_calls m WHERE m.attempt_id = p.attempt_id AND m.operation = 'chat' AND m.status = 'succeeded')
    AND (NEW.embedding_supported = 0 OR EXISTS (SELECT 1 FROM model_calls m WHERE m.attempt_id = p.attempt_id AND m.operation = 'embedding' AND m.status = 'succeeded'))
    AND EXISTS (SELECT 1 FROM model_calls m WHERE m.attempt_id = p.attempt_id AND m.operation = 'chat' AND m.status = 'cancelled' AND m.termination_reason = 'cancelled')
)
BEGIN SELECT RAISE(ABORT, 'model-provider probe child must match its header, provider config and real calls'); END;
CREATE TRIGGER trg_thanos_connection_probe_results_closure BEFORE INSERT ON thanos_connection_probe_results
WHEN NOT EXISTS (SELECT 1 FROM connection_probe_results p WHERE p.id = NEW.probe_result_id AND p.connection_type IN ('prometheus','thanos'))
BEGIN SELECT RAISE(ABORT, 'metrics probe child must match a Prometheus-compatible probe header'); END;
CREATE TRIGGER trg_connections_model_provider_insert_disabled BEFORE INSERT ON connections
WHEN NEW.type = 'model_provider' AND NEW.enabled = 1
BEGIN SELECT RAISE(ABORT, 'model_provider must be created disabled until its revision and credential pass the real capability probe'); END;
CREATE TRIGGER trg_connections_current_key_binding BEFORE UPDATE OF enabled, revalidation_required, current_credential_generation_id ON connections
WHEN NEW.enabled = 1 AND NEW.revalidation_required = 0 AND (
  NEW.current_credential_generation_id IS NULL OR NOT EXISTS (
    SELECT 1 FROM credential_generations g JOIN root_key_state k ON k.id = 1
    WHERE g.id = NEW.current_credential_generation_id AND g.connection_id = NEW.id
      AND g.key_binding_revision = k.binding_revision))
BEGIN SELECT RAISE(ABORT, 'enabled validated connection requires a current credential under the current root key binding'); END;
CREATE TRIGGER trg_connection_enable_qualification_closure BEFORE INSERT ON connection_enable_qualifications
WHEN NOT EXISTS (
  SELECT 1 FROM connections c
  JOIN connection_probe_results p ON p.id = NEW.probe_result_id AND p.connection_id = c.id
  JOIN credential_generations g ON g.id = p.credential_generation_id
  JOIN root_key_state k ON k.id = 1 AND k.binding_revision = g.key_binding_revision
  WHERE c.id = NEW.connection_id AND c.type IN ('model_provider','prometheus','thanos') AND c.enabled = 0
    AND NEW.enabled_row_version = c.row_version + 1
    AND p.connection_revision_id = c.current_revision_id
    AND p.credential_generation_id = c.current_credential_generation_id
    AND p.root_binding_revision = k.binding_revision AND p.outcome = 'passed'
    AND (
      (c.type = 'model_provider' AND EXISTS (
        SELECT 1 FROM model_provider_connection_probe_results m
        WHERE m.probe_result_id = p.id AND m.streaming_supported = 1
          AND m.native_tool_calling_supported = 1 AND m.cancellation_observed = 1
          AND m.usage_observed = 1
          -- Embeddings are optional for a chat-only current revision. When
          -- configured, their successful real-call result remains mandatory.
          AND (json_type((SELECT config_json FROM connection_revisions WHERE id = c.current_revision_id), '$.embeddingModelId') IS NULL OR m.embedding_supported = 1)))
      OR (c.type IN ('prometheus','thanos') AND EXISTS (
        SELECT 1 FROM thanos_connection_probe_results metrics
        WHERE metrics.probe_result_id = p.id))
    ))
BEGIN SELECT RAISE(ABORT, 'enable qualification must select a passed probe for the exact current model-provider binding'); END;
CREATE TRIGGER trg_connections_enable_requires_probe BEFORE UPDATE OF enabled, current_revision_id, current_credential_generation_id ON connections
WHEN NEW.type IN ('model_provider','prometheus','thanos') AND NEW.enabled = 1 AND (
  OLD.enabled <> 0 OR NEW.current_revision_id IS NULL OR NEW.current_credential_generation_id IS NULL OR NOT EXISTS (
    SELECT 1 FROM connection_enable_qualifications q
    WHERE q.connection_id = NEW.id AND q.enabled_row_version = NEW.row_version))
BEGIN SELECT RAISE(ABORT, 'metrics and model-provider enable must atomically append an explicit immutable qualification event'); END;
CREATE TRIGGER trg_attempt_connection_grant_closure BEFORE INSERT ON attempt_connection_grants
WHEN NOT EXISTS (
  SELECT 1 FROM execution_attempts a
  JOIN connections c ON c.id = NEW.connection_id
  JOIN connection_revisions r ON r.id = NEW.connection_revision_id AND r.connection_id = c.id
  JOIN credential_generations g ON g.id = NEW.credential_generation_id AND g.connection_id = c.id
  JOIN root_key_state k ON k.id = 1 AND g.key_binding_revision = k.binding_revision
  WHERE a.id = NEW.attempt_id AND a.state IN ('Queued','Assigned','Running')
    AND c.current_revision_id = NEW.connection_revision_id
    AND c.current_credential_generation_id = NEW.credential_generation_id
    AND (
      (a.attempt_type = 'connection_probe' AND a.scope_type = 'connection' AND a.scope_id = c.id
        AND NEW.qualified_probe_result_id IS NULL
        AND ((c.type = 'model_provider' AND NEW.purpose IN ('model_probe_chat','model_probe_embedding'))
          OR (c.type = 'prometheus' AND NEW.purpose = 'prometheus_probe')
          OR (c.type = 'thanos' AND NEW.purpose = 'thanos_probe')))
      OR (c.enabled = 1 AND c.revalidation_required = 0 AND (
        (NEW.purpose IN ('chat_model','embedding') AND c.type = 'model_provider'
          AND EXISTS (
            SELECT 1 FROM connection_enable_qualifications q
            JOIN connection_probe_results p ON p.id = q.probe_result_id
            JOIN model_provider_connection_probe_results m ON m.probe_result_id = p.id
            WHERE q.connection_id = c.id AND q.enabled_row_version = c.row_version
              AND p.id = NEW.qualified_probe_result_id AND p.connection_revision_id = r.id
              AND p.credential_generation_id = g.id AND p.outcome = 'passed'
              AND m.streaming_supported = 1 AND m.native_tool_calling_supported = 1
              AND m.cancellation_observed = 1 AND m.usage_observed = 1
              AND (NEW.purpose <> 'embedding' OR m.embedding_supported = 1))
        )
        OR (NEW.purpose = 'thanos_query' AND c.type IN ('prometheus','thanos') AND NEW.qualified_probe_result_id IS NULL)
        OR (NEW.purpose = 'config_thanos_query' AND c.type IN ('prometheus','thanos') AND NEW.qualified_probe_result_id IS NULL
            AND a.attempt_type = 'inspection_collection' AND (
              a.scope_type = 'observation_run'
              OR (a.scope_type = 'run_check' AND EXISTS (
                SELECT 1 FROM inspection_runs r
                JOIN config_plans p ON p.config_version_id = r.config_version_id AND p.plan_key = r.plan_key
                JOIN config_checks check_definition ON check_definition.plan_id = p.id AND check_definition.check_key = a.check_key
                WHERE r.id = a.scope_id AND r.state = 'Running' AND check_definition.kind = 'promql'
              ))
              OR (a.scope_type = 'run_check' AND EXISTS (
                SELECT 1 FROM inspection_runs r
                JOIN inspection_run_checks check_definition ON check_definition.run_id = r.id AND check_definition.check_key = a.check_key
                WHERE r.id = a.scope_id AND r.state = 'Running' AND r.plan_id IS NOT NULL
              ))
            ))
      ))
    )
    AND (NEW.created_by_tool_call_id IS NULL OR EXISTS (
      SELECT 1 FROM tool_calls t WHERE t.id = NEW.created_by_tool_call_id AND t.attempt_id = NEW.attempt_id))
)
BEGIN SELECT RAISE(ABORT, 'attempt connection grant must close over the exact active binding, purpose and selected qualification'); END;
CREATE TRIGGER trg_model_call_grant_closure BEFORE INSERT ON model_calls
WHEN NOT EXISTS (
  SELECT 1 FROM attempt_connection_grants g
  WHERE g.id = NEW.connection_grant_id AND g.attempt_id = NEW.attempt_id
    AND ((NEW.operation = 'chat' AND g.purpose IN ('chat_model','model_probe_chat'))
      OR (NEW.operation = 'embedding' AND g.purpose IN ('embedding','model_probe_embedding')))
)
BEGIN SELECT RAISE(ABORT, 'model call must use the same Attempt model/embedding grant'); END;
CREATE TRIGGER trg_model_call_operation_attempt BEFORE INSERT ON model_calls
WHEN NOT EXISTS (
  SELECT 1 FROM execution_attempts a WHERE a.id = NEW.attempt_id AND a.state = 'Running'
    AND ((NEW.operation = 'embedding' AND a.attempt_type IN ('embedding','connection_probe'))
      OR (NEW.operation = 'chat' AND a.attempt_type IN ('initial_analysis','investigation','inspection_analysis','knowledge_extraction','connection_probe')))
)
BEGIN SELECT RAISE(ABORT, 'model call operation must match a Running fixed Plinth work mode'); END;
CREATE TRIGGER trg_model_call_input_item_closure BEFORE INSERT ON model_call_input_items
WHEN EXISTS (SELECT 1 FROM model_calls m WHERE m.id = NEW.model_call_id) AND (
  (NEW.prior_model_call_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM model_calls current JOIN model_calls prior ON prior.id = NEW.prior_model_call_id
    WHERE current.id = NEW.model_call_id AND prior.attempt_id = current.attempt_id AND prior.status = 'succeeded'
      AND NEW.item_role = 'assistant'
      AND (prior.call_seq < current.call_seq OR (prior.call_seq = current.call_seq AND prior.retry_seq < current.retry_seq))))
  OR (NEW.tool_call_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM model_calls current JOIN tool_calls t ON t.id = NEW.tool_call_id
    WHERE current.id = NEW.model_call_id AND t.attempt_id = current.attempt_id AND NEW.item_role = 'tool'
      AND (t.status = 'succeeded' OR (t.status = 'failed' AND t.failure_mode = 'return_to_model' AND t.result_json IS NOT NULL))))
  OR (NEW.investigation_message_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM model_calls current JOIN investigation_messages im ON im.id = NEW.investigation_message_id
    JOIN execution_attempts a ON a.id = current.attempt_id
    WHERE current.id = NEW.model_call_id AND a.scope_type = 'investigation' AND im.investigation_id = a.scope_id
      AND im.status = 'active' AND NEW.item_role = im.role))
  OR (NEW.attempt_input_snapshot_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM model_calls current JOIN attempt_input_snapshots s ON s.id = NEW.attempt_input_snapshot_id
    WHERE current.id = NEW.model_call_id AND s.attempt_id = current.attempt_id
      AND NEW.item_role = CASE current.operation WHEN 'embedding' THEN 'user' ELSE 'system' END))
  OR (NEW.evidence_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM model_calls current JOIN evidence e ON e.id = NEW.evidence_id
     WHERE current.id = NEW.model_call_id
       AND NEW.item_role = CASE current.operation WHEN 'embedding' THEN 'user' ELSE 'system' END
       AND (e.attempt_id = current.attempt_id OR EXISTS (
      SELECT 1 FROM attempt_input_snapshots s JOIN attempt_input_items i ON i.snapshot_id = s.id
      WHERE s.attempt_id = current.attempt_id AND i.evidence_id = e.id))))
  OR (NEW.artifact_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM model_calls current JOIN attempt_artifact_grants g ON g.attempt_id = current.attempt_id
    WHERE current.id = NEW.model_call_id AND g.artifact_id = NEW.artifact_id
      AND NEW.item_role = CASE current.operation WHEN 'embedding' THEN 'user' ELSE 'system' END))
  OR (NEW.knowledge_version_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM model_calls current JOIN attempt_input_snapshots s ON s.attempt_id = current.attempt_id
    JOIN attempt_input_items i ON i.snapshot_id = s.id
    WHERE current.id = NEW.model_call_id AND i.knowledge_version_id = NEW.knowledge_version_id
      AND NEW.item_role = CASE current.operation WHEN 'embedding' THEN 'user' ELSE 'system' END))
  OR (NEW.synthetic_kind IS NOT NULL AND NEW.item_role <> 'system')
)
BEGIN SELECT RAISE(ABORT, 'model call context item must belong to the same Attempt and valid history state'); END;

-- Tool Call 在执行前以 pending 行落库；model_call、Attempt、provider ID、ordinal 与 grant 均不可混淆。
-- The durable ledger is also a contract boundary: a direct SQL writer cannot
-- resurrect retired tool names. Per-agent catalog membership is enforced by
-- Quoin before this insert; this trigger seals the global name set.
CREATE TRIGGER trg_tool_call_fixed_name BEFORE INSERT ON tool_calls
WHEN NEW.tool_name NOT IN ('bash','read','write','grep','artifact_read','artifact_grep','thanos_query')
BEGIN SELECT RAISE(ABORT, 'tool call name is not in the frozen catalog'); END;
CREATE TRIGGER trg_tool_call_closure BEFORE INSERT ON tool_calls
WHEN NEW.status <> 'pending' OR NOT EXISTS (
  SELECT 1 FROM model_calls m JOIN execution_attempts a ON a.id = m.attempt_id
  WHERE m.id = NEW.model_call_id AND m.attempt_id = NEW.attempt_id AND m.call_seq = NEW.call_seq AND m.status = 'succeeded'
    AND EXISTS (SELECT 1 FROM model_call_outputs o WHERE o.model_call_id = m.id AND o.complete = 1)
    AND a.state = 'Running' AND a.attempt_type IN ('initial_analysis','investigation','inspection_analysis','knowledge_extraction','connection_probe')
)
BEGIN SELECT RAISE(ABORT, 'tool call must be inserted pending after a successful model call in the same Running Attempt'); END;
CREATE TRIGGER trg_tool_call_proposal_closure BEFORE INSERT ON tool_calls
WHEN EXISTS (
  SELECT 1 FROM model_calls m
  WHERE m.id = NEW.model_call_id AND m.attempt_id = NEW.attempt_id AND m.call_seq = NEW.call_seq AND m.status = 'succeeded'
) AND NOT EXISTS (
  SELECT 1 FROM model_call_outputs o
  WHERE o.model_call_id = NEW.model_call_id AND o.complete = 1
    AND json_type(o.response_json, '$.tool_calls') = 'array'
    AND NEW.tool_index < json_array_length(o.response_json, '$.tool_calls')
    AND json_extract(o.response_json, '$.tool_calls[' || NEW.tool_index || '].id') = NEW.provider_tool_call_id
    AND json_extract(o.response_json, '$.tool_calls[' || NEW.tool_index || '].name') = NEW.tool_name
)
BEGIN SELECT RAISE(ABORT, 'tool call must match the provider proposal at the same ordinal'); END;
CREATE TRIGGER trg_tool_call_connection_grant_closure BEFORE INSERT ON tool_call_connection_grants
WHEN NOT EXISTS (
  SELECT 1 FROM tool_calls t JOIN attempt_connection_grants g ON g.id = NEW.connection_grant_id
  WHERE t.id = NEW.tool_call_id AND g.attempt_id = t.attempt_id
    AND t.tool_name = 'thanos_query' AND g.purpose = 'thanos_query'
)
BEGIN SELECT RAISE(ABORT, 'tool call connection grant must match the same Attempt and typed external tool'); END;
CREATE TRIGGER trg_execution_attempts_success_requires_closed_calls BEFORE UPDATE OF state ON execution_attempts
WHEN NEW.state = 'Succeeded' AND OLD.state <> 'Succeeded' AND (
  EXISTS (SELECT 1 FROM model_calls mc WHERE mc.attempt_id = NEW.id AND mc.status = 'running')
  OR EXISTS (SELECT 1 FROM tool_calls tc WHERE tc.attempt_id = NEW.id AND tc.status IN ('pending','running'))
  OR (NEW.attempt_type = 'inspection_collection' AND NEW.scope_type = 'run_check' AND NOT EXISTS (
    SELECT 1 FROM inspection_check_results r
    WHERE r.run_id = NEW.scope_id AND r.check_key = NEW.check_key
      AND r.attempt_id = NEW.id AND r.result_digest IS NOT NULL))
  OR (NEW.attempt_type = 'inspection_collection' AND NEW.scope_type = 'observation_run' AND NOT EXISTS (
    SELECT 1 FROM observation_run_objects x
    WHERE x.attempt_id = NEW.id AND x.result_digest IS NOT NULL))
)
BEGIN SELECT RAISE(ABORT, 'Succeeded Attempt requires all Model Calls and Tool Calls terminal'); END;
CREATE TRIGGER trg_execution_attempts_close_calls_after_terminal AFTER UPDATE OF state ON execution_attempts
WHEN NEW.state IN ('Cancelling','Failed','Cancelled','Interrupted')
  AND OLD.state NOT IN ('Succeeded','Failed','Cancelled','Interrupted')
BEGIN
  UPDATE model_calls
  SET status = 'cancelled', termination_reason = 'cancelled', ended_at = COALESCE(NEW.ended_at, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
  WHERE attempt_id = NEW.id AND status = 'running';
  UPDATE tool_calls
  SET status = 'cancelled', row_version = row_version + 1,
      result_json = NULL, result_artifact_id = NULL,
      error_detail = COALESCE(error_detail, 'attempt terminated'), ended_at = COALESCE(NEW.ended_at, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
  WHERE attempt_id = NEW.id AND status IN ('pending','running');
END;

-- 成功 Attempt 的领域结果必须已在同一事务写入；模型 ResultProposal 不能直接改写任意领域表。
CREATE TRIGGER trg_execution_attempts_success_result BEFORE UPDATE OF state ON execution_attempts
WHEN NEW.state = 'Succeeded' AND OLD.state <> 'Succeeded' AND (
  (NEW.attempt_type = 'initial_analysis' AND NOT EXISTS (SELECT 1 FROM initial_analysis_outputs o WHERE o.attempt_id = NEW.id))
  OR (NEW.attempt_type = 'investigation' AND NOT EXISTS (
      SELECT 1 FROM investigation_messages m WHERE m.attempt_id = NEW.id AND m.role = 'assistant' AND m.status = 'active'))
  OR (NEW.attempt_type = 'inspection_analysis' AND NOT EXISTS (SELECT 1 FROM inspection_reports r WHERE r.attempt_id = NEW.id))
  OR (NEW.attempt_type = 'knowledge_extraction' AND NOT EXISTS (
      SELECT 1 FROM knowledge_import_batches b WHERE b.id = NEW.scope_id AND b.state = 'AwaitingConfirmation'
        AND EXISTS (SELECT 1 FROM knowledge_candidates c WHERE c.import_batch_id = b.id AND c.generation = b.generation)))
  OR (NEW.attempt_type = 'embedding' AND (
      EXISTS (SELECT 1 FROM attempt_input_snapshots s JOIN attempt_input_items i ON i.snapshot_id = s.id
              WHERE s.attempt_id = NEW.id AND i.knowledge_version_id IS NOT NULL
                AND NOT EXISTS (SELECT 1 FROM embeddings e WHERE e.knowledge_version_id = i.knowledge_version_id
                                AND e.embedding_generation_id = NEW.scope_id AND e.state = 'ready'))
      OR NOT EXISTS (SELECT 1 FROM embedding_generations g WHERE g.id = NEW.scope_id AND g.vector_dim IS NOT NULL)))
  OR (NEW.attempt_type = 'inspection_collection' AND NOT EXISTS (
    SELECT 1 FROM inspection_check_results r
    WHERE NEW.scope_type = 'run_check' AND r.run_id = NEW.scope_id AND r.check_key = NEW.check_key
      AND r.attempt_id = NEW.id AND r.result_digest IS NOT NULL
    UNION ALL
    SELECT 1 FROM observation_run_objects o
    WHERE NEW.scope_type = 'observation_run' AND o.observation_run_id = NEW.scope_id
      AND o.attempt_id = NEW.id AND o.result_digest IS NOT NULL
  ))
  OR (NEW.attempt_type = 'connection_probe' AND NOT EXISTS (
    SELECT 1 FROM connection_probe_results p
      WHERE p.attempt_id = NEW.id AND p.connection_id = NEW.scope_id
        AND ((p.connection_type = 'model_provider' AND EXISTS (SELECT 1 FROM model_provider_connection_probe_results m WHERE m.probe_result_id = p.id))
          OR (p.connection_type IN ('prometheus','thanos') AND EXISTS (SELECT 1 FROM thanos_connection_probe_results t WHERE t.probe_result_id = p.id)))))
  OR (NEW.attempt_type IN ('initial_analysis','investigation','inspection_analysis','knowledge_extraction') AND (
      NOT EXISTS (SELECT 1 FROM model_calls m WHERE m.attempt_id = NEW.id AND m.status = 'succeeded')
      OR EXISTS (
        SELECT 1 FROM model_calls m
        JOIN model_call_outputs o ON o.model_call_id = m.id AND o.complete = 1
        WHERE m.attempt_id = NEW.id AND m.operation = 'chat' AND m.status = 'succeeded'
          AND (json_type(o.response_json, '$.tool_calls') IS NOT 'array'
            OR json_array_length(o.response_json, '$.tool_calls') <>
               (SELECT count(*) FROM tool_calls t WHERE t.model_call_id = m.id))
      )
      OR EXISTS (SELECT 1 FROM tool_calls t WHERE t.attempt_id = NEW.id
                 AND (t.status NOT IN ('succeeded','failed','cancelled')
                   OR (t.status = 'failed' AND t.failure_mode = 'fail_attempt')))
  ))
)
BEGIN SELECT RAISE(ABORT, 'Succeeded Attempt must atomically commit the valid domain result for its fixed work mode'); END;
-- 普通巡检结果只在 Running 阶段追加并闭合到精确 check/Evidence 来源。PromQL
-- 绑定精确 collection Attempt；PromQL ok 引用唯一完整 Evidence，业务 gap
-- 和技术 gap 不制造空 Evidence。
CREATE TRIGGER trg_inspection_check_results_closure BEFORE INSERT ON inspection_check_results
WHEN NOT EXISTS (
  -- 任一有效形状即放行；三种来源互相独立（UNION ALL 隔离各自 JOIN 作用域）。
  SELECT 1 FROM inspection_runs r
  JOIN config_plans p ON p.config_version_id = r.config_version_id AND p.plan_key = r.plan_key
  JOIN config_checks c ON c.plan_id = p.id AND c.check_key = NEW.check_key
  WHERE r.id = NEW.run_id AND r.state = 'Running' AND (
    (c.kind = 'promql' AND NEW.attempt_id IS NOT NULL AND NEW.result_digest IS NOT NULL AND EXISTS (
      SELECT 1 FROM execution_attempts a
      WHERE a.id = NEW.attempt_id AND a.attempt_type = 'inspection_collection'
        AND a.scope_type = 'run_check' AND a.scope_id = NEW.run_id AND a.check_key = NEW.check_key
        AND a.runtime_slot = 'plinth' AND a.state = 'Running' AND a.accepted_at IS NOT NULL
    ) AND (
      (NEW.status = 'ok' AND EXISTS (
        SELECT 1 FROM evidence e WHERE e.id = NEW.evidence_id AND e.attempt_id = NEW.attempt_id
          AND e.tool_call_id IS NULL AND e.target_type = 'inspection_run' AND e.target_id = NEW.run_id
          AND e.integrity = 'complete' AND e.result_json IS NOT NULL AND e.artifact_id IS NULL
          AND json_extract(e.params_json, '$.check_key') = NEW.check_key))
      OR (NEW.status IN ('error','gap') AND NEW.evidence_id IS NULL
          AND NEW.gap_reason IN ('query_failed','partial_response','no_data','cancelled','interrupted'))))
    OR (c.kind = 'promql' AND NEW.attempt_id IS NOT NULL
      AND NEW.status IN ('error','gap') AND NEW.evidence_id IS NULL
      AND NEW.result_digest IS NULL AND NEW.gap_reason = 'runtime_unavailable'
      AND EXISTS (
        SELECT 1 FROM execution_attempts a
        WHERE a.id = NEW.attempt_id AND a.attempt_type = 'inspection_collection'
          AND a.scope_type = 'run_check' AND a.scope_id = NEW.run_id AND a.check_key = NEW.check_key
          AND a.state = 'Failed' AND a.runtime_slot IS NULL AND a.accepted_at IS NULL
      ))
  )
  UNION ALL
  -- 独立计划 Run（ADR-0004）：插件采集子 Attempt 的运行中结果，正文必须是该
  -- Attempt 的完整 Evidence；缺口/错误不制造 Evidence。
  SELECT 1 FROM inspection_runs pr
  JOIN inspection_run_checks c ON c.run_id = pr.id AND c.check_key = NEW.check_key
  WHERE pr.id = NEW.run_id AND pr.state = 'Running' AND pr.plan_id IS NOT NULL
    AND NEW.attempt_id IS NOT NULL AND NEW.result_digest IS NOT NULL AND EXISTS (
      SELECT 1 FROM execution_attempts a
      WHERE a.id = NEW.attempt_id AND a.attempt_type = 'inspection_collection'
        AND a.scope_type = 'run_check' AND a.scope_id = NEW.run_id AND a.check_key = NEW.check_key
        AND a.runtime_slot = 'plinth' AND a.state = 'Running' AND a.accepted_at IS NOT NULL
    ) AND (
      (NEW.status = 'ok' AND EXISTS (
        SELECT 1 FROM evidence e WHERE e.id = NEW.evidence_id AND e.attempt_id = NEW.attempt_id
          AND e.tool_call_id IS NULL AND e.target_type = 'inspection_run' AND e.target_id = NEW.run_id
          AND e.integrity = 'complete' AND e.result_json IS NOT NULL AND e.artifact_id IS NULL
          AND json_extract(e.params_json, '$.check_key') = NEW.check_key))
      OR (NEW.status IN ('error','gap') AND NEW.evidence_id IS NULL
          AND NEW.gap_reason IN ('query_failed','partial_response','no_data','cancelled','interrupted'))
    )
  UNION ALL
  -- 边界时 Runtime 缺位的插件采集技术缺口：不制造 Evidence，Attempt 已终态。
  SELECT 1 FROM inspection_runs pr
  JOIN inspection_run_checks c ON c.run_id = pr.id AND c.check_key = NEW.check_key
  WHERE pr.id = NEW.run_id AND pr.state = 'Running' AND pr.plan_id IS NOT NULL
    AND NEW.attempt_id IS NOT NULL
    AND NEW.status IN ('error','gap') AND NEW.evidence_id IS NULL
    AND NEW.result_digest IS NULL AND NEW.gap_reason = 'runtime_unavailable'
    AND EXISTS (
      SELECT 1 FROM execution_attempts a
      WHERE a.id = NEW.attempt_id AND a.attempt_type = 'inspection_collection'
        AND a.scope_type = 'run_check' AND a.scope_id = NEW.run_id AND a.check_key = NEW.check_key
        AND a.state = 'Failed' AND a.runtime_slot IS NULL AND a.accepted_at IS NULL
    )
)
OR (NEW.evidence_id IS NOT NULL AND EXISTS (
  SELECT 1 FROM inspection_check_results r WHERE r.evidence_id = NEW.evidence_id))
BEGIN SELECT RAISE(ABORT, 'inspection result must be one exact PromQL result, a plugin collection result, or a terminal technical gap'); END;
-- PromQL ResultProposal commits its typed Evidence, check result, and Attempt
-- completion in this one outer INSERT statement (DATA-TX-018). A gap/error
-- remains a successful transport collection with a typed domain result.
CREATE TRIGGER trg_inspection_promql_result_commit AFTER INSERT ON inspection_check_results
WHEN NEW.result_digest IS NOT NULL AND EXISTS (
  SELECT 1 FROM execution_attempts a
  JOIN inspection_runs r ON r.id = a.scope_id
  JOIN config_plans p ON p.config_version_id = r.config_version_id AND p.plan_key = r.plan_key
  JOIN config_checks c ON c.plan_id = p.id AND c.check_key = a.check_key
  WHERE a.id = NEW.attempt_id AND a.scope_type = 'run_check' AND c.kind = 'promql'
)
BEGIN
  UPDATE execution_attempts
  SET state = 'Succeeded', ended_at = NEW.created_at, row_version = row_version + 1
  WHERE id = NEW.attempt_id AND state = 'Running';
END;
-- 插件采集 ResultProposal 的 Attempt 收口：只闭合运行中且绑定精确 run_check 的
-- 独立计划子 Attempt；历史声明 PromQL 由其原有触发器收口。
CREATE TRIGGER trg_inspection_plugin_result_commit AFTER INSERT ON inspection_check_results
WHEN NEW.result_digest IS NOT NULL AND EXISTS (
  SELECT 1 FROM execution_attempts a
  JOIN inspection_run_checks c ON c.run_id = a.scope_id AND c.check_key = a.check_key
  WHERE a.id = NEW.attempt_id AND a.scope_type = 'run_check'
)
BEGIN
  UPDATE execution_attempts
  SET state = 'Succeeded', ended_at = NEW.created_at, row_version = row_version + 1
  WHERE id = NEW.attempt_id AND state = 'Running';
END;
CREATE TRIGGER trg_config_discoveries_parent_frozen BEFORE INSERT ON config_discoveries
WHEN NOT EXISTS (
  SELECT 1 FROM business_system_config_versions v
  WHERE v.id = NEW.config_version_id AND v.state = 'draft' AND v.published_at IS NULL
    AND NOT EXISTS (SELECT 1 FROM inspection_runs r WHERE r.config_version_id = NEW.config_version_id)
)
BEGIN SELECT RAISE(ABORT, 'config_discoveries can only be inserted while parent config is draft with no publications, and no inspection runs'); END;
CREATE TRIGGER trg_config_plans_parent_frozen BEFORE INSERT ON config_plans
WHEN NOT EXISTS (
  SELECT 1 FROM business_system_config_versions v
  WHERE v.id = NEW.config_version_id AND v.state = 'draft' AND v.published_at IS NULL
    AND NOT EXISTS (SELECT 1 FROM inspection_runs r WHERE r.config_version_id = NEW.config_version_id)
)
BEGIN SELECT RAISE(ABORT, 'config_plans can only be inserted while parent config is draft with no publications, and no inspection runs'); END;
CREATE TRIGGER trg_config_checks_parent_frozen BEFORE INSERT ON config_checks
WHEN NOT EXISTS (
  SELECT 1 FROM config_plans p JOIN business_system_config_versions v ON v.id = p.config_version_id
  WHERE p.id = NEW.plan_id AND v.state = 'draft' AND v.published_at IS NULL
    AND NOT EXISTS (SELECT 1 FROM inspection_runs r WHERE r.config_version_id = v.id)
)
BEGIN SELECT RAISE(ABORT, 'config_checks can only be inserted while parent config is draft with no publications, and no inspection runs'); END;
-- check_key 只在其 plan 父作用域内唯一，因而不同 plan 可合法复用同一 check_key（DATA-CONFIG-004）。
CREATE TRIGGER trg_config_discoveries_identity_labels_unique BEFORE INSERT ON config_discoveries
WHEN (SELECT COUNT(*) FROM json_each(NEW.identity_labels_json)) <> (SELECT COUNT(DISTINCT value) FROM json_each(NEW.identity_labels_json))
BEGIN SELECT RAISE(ABORT, 'identity_labels must not contain duplicates'); END;
-- 12.42 配置验证/资源刷新 Run 纳入任务变更日志（DATA-SSE-004）：与权威状态同一事务派生。
-- Immutable Inspection Report closure (T24b). Runtime inserts only the typed
-- ledger; direct Report writes and a successful analysis without that ledger
-- are rejected by these fences.
CREATE TRIGGER trg_inspection_reports_ledger_only BEFORE INSERT ON inspection_reports
WHEN NOT EXISTS (
  SELECT 1 FROM inspection_report_result_ledgers l
  WHERE l.attempt_id=NEW.attempt_id AND l.inspection_run_id=NEW.run_id AND NEW.version=l.report_version
)
BEGIN SELECT RAISE(ABORT, 'inspection report must be created by its typed ResultProposal ledger'); END;
CREATE TRIGGER trg_inspection_report_result_closure BEFORE INSERT ON inspection_report_result_ledgers
WHEN NOT EXISTS (
  SELECT 1 FROM execution_attempts a JOIN inspection_runs r ON r.id=a.scope_id
  WHERE a.id=NEW.attempt_id AND a.attempt_type='inspection_analysis' AND a.scope_type='run'
    AND a.scope_id=NEW.inspection_run_id AND a.state='Running' AND a.accepted_at IS NOT NULL
    AND r.state IN ('Completed','CompletedWithGaps')
)
OR NOT EXISTS (SELECT 1 FROM model_calls m WHERE m.id=NEW.model_call_id AND m.attempt_id=NEW.attempt_id AND m.status='succeeded' AND m.prompt_digest=NEW.prompt_digest)
OR NOT EXISTS (SELECT 1 FROM attempt_input_snapshots s WHERE s.attempt_id=NEW.attempt_id AND s.inspection_report_version=NEW.report_version)
OR (SELECT count(*) FROM attempt_input_snapshots s JOIN attempt_input_items i ON i.snapshot_id=s.id WHERE s.attempt_id=NEW.attempt_id AND i.inspection_run_id IS NOT NULL) <> 1
OR NOT EXISTS (SELECT 1 FROM attempt_input_snapshots s JOIN attempt_input_items i ON i.snapshot_id=s.id WHERE s.attempt_id=NEW.attempt_id AND i.inspection_run_id=NEW.inspection_run_id)
OR NEW.report_version <> (SELECT count(*) + 1 FROM inspection_reports r WHERE r.run_id=NEW.inspection_run_id)
OR EXISTS (SELECT 1 FROM inspection_check_results c WHERE c.run_id=NEW.inspection_run_id AND NOT EXISTS (
             SELECT 1 FROM attempt_input_snapshots s JOIN attempt_input_items i ON i.snapshot_id=s.id WHERE s.attempt_id=NEW.attempt_id AND i.inspection_check_result_id=c.id))
OR EXISTS (SELECT 1 FROM inspection_check_results c WHERE c.run_id=NEW.inspection_run_id AND c.evidence_id IS NOT NULL AND NOT EXISTS (
             SELECT 1 FROM json_each(NEW.evidence_ids_json) x WHERE x.value=c.evidence_id))
OR EXISTS (SELECT 1 FROM json_each(NEW.evidence_ids_json) x
           WHERE x.type <> 'integer' OR NOT EXISTS (
             SELECT 1 FROM inspection_check_results c JOIN evidence e ON e.id=c.evidence_id
             WHERE c.run_id=NEW.inspection_run_id AND e.id=x.value AND e.integrity='complete'
               AND EXISTS (SELECT 1 FROM attempt_input_snapshots s JOIN attempt_input_items i ON i.snapshot_id=s.id WHERE s.attempt_id=NEW.attempt_id AND i.evidence_id=e.id)))
OR EXISTS (SELECT 1 FROM json_each(NEW.artifact_ids_json) x
           WHERE x.type <> 'integer' OR NOT EXISTS (
             SELECT 1 FROM evidence e WHERE e.target_type='inspection_run' AND e.target_id=NEW.inspection_run_id
               AND (e.artifact_id=x.value
                 OR EXISTS (SELECT 1 FROM artifacts a WHERE a.id=x.value AND a.owner_type='evidence' AND a.owner_id=e.id))
               AND EXISTS (SELECT 1 FROM attempt_input_snapshots s JOIN attempt_input_items i ON i.snapshot_id=s.id WHERE s.attempt_id=NEW.attempt_id AND i.artifact_id=x.value)))
OR EXISTS (SELECT 1 FROM json_each(NEW.knowledge_version_ids_json) x
           WHERE x.type <> 'integer' OR NOT EXISTS (SELECT 1 FROM knowledge_versions k WHERE k.id=x.value
               AND EXISTS (SELECT 1 FROM attempt_input_snapshots s JOIN attempt_input_items i ON i.snapshot_id=s.id WHERE s.attempt_id=NEW.attempt_id AND i.knowledge_version_id=k.id)))
OR (SELECT count(*) FROM json_each(NEW.evidence_ids_json)) <> (SELECT count(DISTINCT value) FROM json_each(NEW.evidence_ids_json))
OR (SELECT count(*) FROM json_each(NEW.artifact_ids_json)) <> (SELECT count(DISTINCT value) FROM json_each(NEW.artifact_ids_json))
OR (SELECT count(*) FROM json_each(NEW.knowledge_version_ids_json)) <> (SELECT count(DISTINCT value) FROM json_each(NEW.knowledge_version_ids_json))
OR NEW.evidence_ids_json <> json(NEW.evidence_ids_json)
OR NEW.artifact_ids_json <> json(NEW.artifact_ids_json)
OR NEW.knowledge_version_ids_json <> json(NEW.knowledge_version_ids_json)
OR NEW.evidence_digest <> lower(hex(sha256(NEW.evidence_ids_json)))
OR NEW.result_digest <> sha256('inspection_report_result_v1|' || NEW.attempt_id || '|' || NEW.inspection_run_id || '|' || NEW.model_call_id || '|success|' || NEW.content || '|' || NEW.evidence_ids_json || '|' || NEW.artifact_ids_json || '|' || NEW.knowledge_version_ids_json || '|' || NEW.evidence_digest || '|' || NEW.prompt_digest)
BEGIN SELECT RAISE(ABORT, 'inspection Report ResultProposal must close one running Run analysis and only its immutable references'); END;
CREATE TRIGGER trg_inspection_report_result_commit AFTER INSERT ON inspection_report_result_ledgers
BEGIN
  INSERT INTO inspection_reports(run_id,version,attempt_id,evidence_digest,model_id,prompt_digest,content,created_at)
  SELECT NEW.inspection_run_id,NEW.report_version,NEW.attempt_id,NEW.evidence_digest,m.model_id,NEW.prompt_digest,NEW.content,NEW.created_at
  FROM model_calls m WHERE m.id=NEW.model_call_id AND m.attempt_id=NEW.attempt_id AND m.status='succeeded';
  INSERT INTO inspection_report_evidence(report_id,evidence_id,ordinal)
  SELECT r.id,x.value,x.key FROM inspection_reports r JOIN json_each(NEW.evidence_ids_json) x WHERE r.attempt_id=NEW.attempt_id;
  INSERT INTO inspection_report_artifacts(report_id,artifact_id,ordinal)
  SELECT r.id,x.value,x.key FROM inspection_reports r JOIN json_each(NEW.artifact_ids_json) x WHERE r.attempt_id=NEW.attempt_id;
  INSERT INTO inspection_report_knowledge_versions(report_id,knowledge_version_id,ordinal)
  SELECT r.id,x.value,x.key FROM inspection_reports r JOIN json_each(NEW.knowledge_version_ids_json) x WHERE r.attempt_id=NEW.attempt_id;
  UPDATE execution_attempts SET state='Succeeded',ended_at=NEW.created_at,row_version=row_version+1
  WHERE id=NEW.attempt_id AND state='Running';
END;

CREATE TRIGGER trg_inspection_analysis_success_report BEFORE UPDATE OF state ON execution_attempts
WHEN OLD.state='Running' AND NEW.state='Succeeded' AND NEW.attempt_type='inspection_analysis'
  AND NOT EXISTS (SELECT 1 FROM inspection_report_result_ledgers l WHERE l.attempt_id=NEW.id)
BEGIN SELECT RAISE(ABORT, 'Succeeded inspection analysis Attempt must atomically commit its immutable Report ledger'); END;

CREATE TRIGGER trg_inspection_report_result_ledgers_immutable BEFORE UPDATE ON inspection_report_result_ledgers
BEGIN SELECT RAISE(ABORT, 'inspection Report ResultProposal ledger is immutable'); END;
CREATE TRIGGER trg_inspection_report_result_ledgers_no_delete BEFORE DELETE ON inspection_report_result_ledgers
BEGIN SELECT RAISE(ABORT, 'inspection Report ResultProposal ledger is append-only'); END;

CREATE TRIGGER trg_inspection_analysis_requires_closed_run BEFORE INSERT ON execution_attempts
WHEN NEW.attempt_type='inspection_analysis' AND NEW.scope_type='run' AND NOT EXISTS (
  SELECT 1 FROM inspection_runs r WHERE r.id=NEW.scope_id AND r.state IN ('Completed','CompletedWithGaps')
)
BEGIN SELECT RAISE(ABORT, 'inspection analysis requires a collection-complete Inspection Run'); END;

CREATE TRIGGER trg_inspection_report_evidence_ledger_closure BEFORE INSERT ON inspection_report_evidence
WHEN NOT EXISTS (
  SELECT 1 FROM inspection_reports r JOIN inspection_report_result_ledgers l ON l.attempt_id=r.attempt_id
  JOIN json_each(l.evidence_ids_json) x ON x.value=NEW.evidence_id AND x.key=NEW.ordinal
  WHERE r.id=NEW.report_id
)
BEGIN SELECT RAISE(ABORT, 'inspection Report Evidence reference must exactly match its immutable ResultProposal ledger'); END;
CREATE TRIGGER trg_inspection_report_artifact_ledger_closure BEFORE INSERT ON inspection_report_artifacts
WHEN NOT EXISTS (
  SELECT 1 FROM inspection_reports r JOIN inspection_report_result_ledgers l ON l.attempt_id=r.attempt_id
  JOIN json_each(l.artifact_ids_json) x ON x.value=NEW.artifact_id AND x.key=NEW.ordinal
  WHERE r.id=NEW.report_id
)
BEGIN SELECT RAISE(ABORT, 'inspection Report Artifact reference must exactly match its immutable ResultProposal ledger'); END;
CREATE TRIGGER trg_inspection_report_knowledge_ledger_closure BEFORE INSERT ON inspection_report_knowledge_versions
WHEN NOT EXISTS (
  SELECT 1 FROM inspection_reports r JOIN inspection_report_result_ledgers l ON l.attempt_id=r.attempt_id
  JOIN json_each(l.knowledge_version_ids_json) x ON x.value=NEW.knowledge_version_id AND x.key=NEW.ordinal
  WHERE r.id=NEW.report_id
)
BEGIN SELECT RAISE(ABORT, 'inspection Report Knowledge reference must exactly match its immutable ResultProposal ledger'); END;
