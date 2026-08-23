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


-- Runtime 注册与长期服务 token 凭据（CONTEXT「服务身份」）：注册状态与 Admin 并发前提
-- （row_version）是持久权威；在线连接、boot/epoch、心跳 last_seen 是瞬时投影（内存），不落库
-- ——避免心跳改写 Admin row_version（DATA-RUNTIME-001）。当前 active 长期 token 的唯一权威是
-- runtime_slots.current_credential_id（单一 owner-side current authority，DATA-RUNTIME-001）；
-- 两阶段轮换的待确认 token 由 pending_credential_id 单行表达；已提升但等待 Admin 显式退休的旧
-- token 由 retiring_credential_id 表达。runtime_credentials 行只记录不可变 generation 生命周期历史
-- （confirmed_at/first_authenticated_at/retired_at 事实），不存在可独立写入的第二状态权威。
CREATE TABLE runtime_slots (
  slot                  TEXT PRIMARY KEY CHECK (slot IN ('plinth','lintel')),
  state                 TEXT NOT NULL DEFAULT 'unregistered' CHECK (state IN ('unregistered','registered','revoked')),
  current_credential_id INTEGER REFERENCES runtime_credentials(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  pending_credential_id INTEGER REFERENCES runtime_credentials(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  retiring_credential_id INTEGER REFERENCES runtime_credentials(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  row_version           INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- 注册/替换/轮换命令并发前提（DATA-ROWVER-001）
  created_at            TEXT NOT NULL,
  CHECK (
    (state IN ('unregistered','revoked') AND current_credential_id IS NULL AND pending_credential_id IS NULL AND retiring_credential_id IS NULL)
    OR (state = 'registered' AND current_credential_id IS NOT NULL)
  ),
  CHECK (current_credential_id IS NULL OR pending_credential_id IS NULL OR current_credential_id <> pending_credential_id),
  CHECK (current_credential_id IS NULL OR retiring_credential_id IS NULL OR current_credential_id <> retiring_credential_id),
  CHECK (pending_credential_id IS NULL OR retiring_credential_id IS NULL OR pending_credential_id <> retiring_credential_id) -- 三个角色指针不得指向同一行
) STRICT;

-- Runtime 长期服务 token 的不可变 credential generation 历史（两阶段轮换：下发新 token -> Runtime
-- 持久化确认 -> 原子切换 current/retiring -> 新 current 首次认证 -> Admin 显式退休旧 token，
-- CONTEXT「服务身份」）。本表只保存不可变来源字段（slot/generation/token_digest/created_at）与
-- 三个一次性生命周期事实：confirmed_at、first_authenticated_at、retired_at；时间均不可回退、
-- 不可改写。current/pending/retiring 选择完全由 runtime_slots 指针承载，本表不宣称任何
-- 状态（DATA-RUNTIME-002）。一次性注册令牌不落库（内存短生命周期、单次使用，HTTP-COMMAND-012）；
-- 本表只保存长期 token digest。
CREATE TABLE runtime_credentials (
  id           INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  slot         TEXT NOT NULL REFERENCES runtime_slots(slot) ON UPDATE RESTRICT ON DELETE RESTRICT,
  generation   INTEGER NOT NULL CHECK (generation >= 1),
  token_digest BLOB NOT NULL CHECK (length(token_digest) = 32), -- 长期 token 只存 digest
  created_at   TEXT NOT NULL,
  confirmed_at          TEXT,   -- Runtime 持久化确认时间（NULL -> 时间戳一次；DATA-RUNTIME-002）
  first_authenticated_at TEXT,   -- 长期 token 第一次成功认证（NULL -> 时间戳一次；Pending Retirement 可见性）
  retired_at            TEXT,   -- Admin/替换吊销时间（NULL -> 时间戳一次；DATA-RUNTIME-002）
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
      'tool_call','knowledge_import_batch','knowledge_candidate','browser_operation',
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
  resource_refresh_interval_seconds  INTEGER CHECK (resource_refresh_interval_seconds IS NULL OR resource_refresh_interval_seconds > 0),
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
  label_contract_version_id         INTEGER NOT NULL REFERENCES label_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 上传时显式目标契约版本；不静默使用当前契约（DATA-CONFIG-003/CFG-CONTRACT-003）
  journey_catalog_digest            TEXT NOT NULL CHECK (length(journey_catalog_digest) = 64 AND journey_catalog_digest NOT GLOB '*[^0-9a-f]*'),  -- 上传时 Quoin 嵌入 Journey Catalog 生成文件原始字节 digest（DATA-CONFIG-008）
  journey_catalog_version           TEXT NOT NULL,
  digest                            TEXT NOT NULL CHECK (length(digest) = 64),
  created_by                        INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at                        TEXT NOT NULL,
  published_at                      TEXT,  -- 一次性发布事实：NULL -> 时间戳一次，只由 current 指针变化触发器写入
  -- 解析一次的类型化根投影（DATA-CONFIG-003）：运行只使用类型结构，不重新解析 YAML
  system_key                        TEXT NOT NULL,  -- 必须等于 business_systems.key（trg_business_config_versions_system_key_match）
  display_name                      TEXT NOT NULL,
  enabled                           INTEGER NOT NULL CHECK (enabled IN (0,1)),
  timezone                          TEXT NOT NULL,  -- IANA 时区（根节点统一提供，DATA-CONFIG-004）
  resource_refresh_interval_seconds INTEGER NOT NULL CHECK (resource_refresh_interval_seconds > 0), -- 根节点统一刷新周期
  UNIQUE (business_system_id, version_seq)
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
  cron              TEXT,                          -- 标准五字段 cron；NULL = 仅人工运行；时区由配置根节点统一提供（DATA-CONFIG-004）
  UNIQUE (config_version_id, plan_key)
) STRICT;

CREATE TABLE config_checks (
  id                INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  plan_id           INTEGER NOT NULL REFERENCES config_plans(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  check_key         TEXT NOT NULL,                  -- 跨版本稳定 key
  display_name      TEXT NOT NULL,
  analysis_question TEXT NOT NULL,
  kind              TEXT NOT NULL CHECK (kind IN ('promql','browser')),
  query_mode        TEXT CHECK (query_mode IN ('instant','range')), -- promql：查询模式；range 以真实 evidence_at 为终点并保存实际 start/end/step
  expression        TEXT,                          -- promql：字面量表达式（上传时经 Prometheus 官方 AST 校验）
  range_seconds     INTEGER CHECK (range_seconds IS NULL OR range_seconds > 0),  -- range 查询窗口
  step_seconds      INTEGER CHECK (step_seconds IS NULL OR step_seconds > 0),    -- range 查询步长
  journey_id        TEXT,                          -- browser：Journey Catalog 中的稳定 ID
  journey_params_json TEXT CHECK (journey_params_json IS NULL OR json_valid(journey_params_json)), -- browser：类型化参数（上传时按 catalog params_schema 静态校验）
  UNIQUE (plan_id, check_key),
  CHECK (
    (kind = 'promql' AND query_mode IS NOT NULL AND expression IS NOT NULL
      AND journey_id IS NULL AND journey_params_json IS NULL
      AND ((query_mode = 'instant' AND range_seconds IS NULL AND step_seconds IS NULL)
           OR (query_mode = 'range' AND range_seconds IS NOT NULL AND step_seconds IS NOT NULL)))
    OR
    (kind = 'browser' AND query_mode IS NULL AND expression IS NULL
      AND range_seconds IS NULL AND step_seconds IS NULL
      AND journey_id IS NOT NULL AND journey_params_json IS NOT NULL)
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

CREATE TABLE observed_refresh_log (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  resource_refresh_run_id INTEGER NOT NULL REFERENCES resource_refresh_runs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  attempt_id         INTEGER REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  business_system_id INTEGER NOT NULL REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  discovery_key      TEXT NOT NULL,
  started_at         TEXT NOT NULL,
  completed_at       TEXT,
  complete           INTEGER NOT NULL DEFAULT 0 CHECK (complete IN (0,1)),
  warnings_json      TEXT CHECK (warnings_json IS NULL OR json_valid(warnings_json)),
  error_detail       TEXT,
  result_digest      BLOB,
  CHECK ((complete = 1 AND completed_at IS NOT NULL AND error_detail IS NULL)
      OR (complete = 0 AND completed_at IS NOT NULL AND error_detail IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX ux_observed_refresh_log_attempt ON observed_refresh_log (attempt_id)
  WHERE attempt_id IS NOT NULL;

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
  connection_type               TEXT NOT NULL CHECK (connection_type IN ('model_provider','thanos','kubernetes')),
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

CREATE TABLE kubernetes_connection_probe_results (
  probe_result_id       INTEGER PRIMARY KEY REFERENCES connection_probe_results(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  effective_namespace   TEXT NOT NULL CHECK (length(effective_namespace) > 0),
  version_ok             INTEGER NOT NULL CHECK (version_ok IN (0,1)),
  core_discovery_ok      INTEGER NOT NULL CHECK (core_discovery_ok IN (0,1)),
  grouped_discovery_ok   INTEGER NOT NULL CHECK (grouped_discovery_ok IN (0,1)),
  pods_get_allowed       INTEGER NOT NULL CHECK (pods_get_allowed IN (0,1)),
  pods_list_allowed      INTEGER NOT NULL CHECK (pods_list_allowed IN (0,1)),
  events_list_allowed    INTEGER NOT NULL CHECK (events_list_allowed IN (0,1)),
  pods_log_get_allowed   INTEGER NOT NULL CHECK (pods_log_get_allowed IN (0,1)),
  detail_json            TEXT NOT NULL CHECK (json_valid(detail_json))
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

-- Business System ↔ Kubernetes Connection 的管理面绑定；不进入业务系统 YAML，也不暴露给模型。
-- 解绑只做 Active -> Retired，保留历史以解释旧 Attempt 的确定性路由。
CREATE TABLE business_system_kubernetes_connections (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  business_system_id INTEGER NOT NULL REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  connection_id      INTEGER NOT NULL REFERENCES connections(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  state              TEXT NOT NULL CHECK (state IN ('Active','Retired')),
  row_version        INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  created_by         INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at         TEXT NOT NULL,
  retired_by         INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  retired_at         TEXT,
  CHECK ((state = 'Active' AND retired_by IS NULL AND retired_at IS NULL)
      OR (state = 'Retired' AND retired_by IS NOT NULL AND retired_at IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX ux_business_system_kubernetes_connection_active
  ON business_system_kubernetes_connections (business_system_id, connection_id) WHERE state = 'Active';
CREATE INDEX idx_business_system_kubernetes_connections_system
  ON business_system_kubernetes_connections (business_system_id, state, connection_id);

-- 一个部署只能有一个 active model provider 与一个全局 active Thanos；Kubernetes 连接允许多个。
CREATE UNIQUE INDEX ux_connections_one_enabled_model_provider ON connections ((1))
  WHERE type = 'model_provider' AND enabled = 1;
CREATE UNIQUE INDEX ux_connections_one_enabled_thanos ON connections ((1))
  WHERE type = 'thanos' AND enabled = 1;

-- Browser Identity 配置是不可变 revision；stable identity 只持有当前指针（DATA-BROWSER-001/010）。
CREATE TABLE browser_identity_revisions (
  id                       INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  business_system_id       INTEGER NOT NULL REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  revision                 INTEGER NOT NULL CHECK (revision >= 1),
  name                     TEXT NOT NULL,
  start_url                TEXT NOT NULL CHECK (
                             ((start_url GLOB 'http://?*' AND substr(start_url, 8, 1) NOT IN ('/','?','#'))
                               OR (start_url GLOB 'https://?*' AND substr(start_url, 9, 1) NOT IN ('/','?','#')))
                             AND instr(start_url, ' ') = 0 AND instr(start_url, char(9)) = 0
                             AND instr(start_url, char(10)) = 0 AND instr(start_url, char(13)) = 0),
  probe_journey_id         TEXT NOT NULL,
  probe_journey_version    INTEGER NOT NULL CHECK (probe_journey_version >= 1),
  probe_params_json        TEXT NOT NULL CHECK (json_valid(probe_params_json) AND json_type(probe_params_json) = 'object'),
  journey_catalog_digest   TEXT NOT NULL CHECK (length(journey_catalog_digest) = 64 AND journey_catalog_digest NOT GLOB '*[^0-9a-f]*'),
  journey_catalog_version  TEXT NOT NULL,
  created_by               INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at               TEXT NOT NULL,
  UNIQUE (business_system_id, revision)
) STRICT;

CREATE TABLE browser_identities (
  id                            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  business_system_id            INTEGER NOT NULL UNIQUE REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  current_revision_id           INTEGER NOT NULL REFERENCES browser_identity_revisions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  current_profile_generation_id INTEGER REFERENCES browser_profile_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  state                         TEXT NOT NULL CHECK (state IN ('Ready','AuthenticationRequired')),
  row_version                   INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  created_at                    TEXT NOT NULL,
  CHECK (state <> 'Ready' OR current_profile_generation_id IS NOT NULL)
) STRICT;

CREATE TABLE browser_profile_generations (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  identity_id               INTEGER NOT NULL REFERENCES browser_identities(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  identity_revision_id      INTEGER NOT NULL REFERENCES browser_identity_revisions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  generation                INTEGER NOT NULL CHECK (generation >= 1),
  chromium_revision         TEXT NOT NULL,
  profile_manifest_digest   TEXT NOT NULL CHECK (length(profile_manifest_digest) = 64 AND profile_manifest_digest NOT GLOB '*[^0-9a-f]*'),
  probe_journey_id          TEXT NOT NULL,
  probe_journey_version     INTEGER NOT NULL CHECK (probe_journey_version >= 1),
  probe_catalog_digest      TEXT NOT NULL CHECK (length(probe_catalog_digest) = 64 AND probe_catalog_digest NOT GLOB '*[^0-9a-f]*'),
  probe_catalog_version     TEXT NOT NULL,
  published_operation_id    INTEGER NOT NULL UNIQUE REFERENCES browser_operations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  published_by              INTEGER NOT NULL REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  published_at              TEXT NOT NULL,
  UNIQUE (identity_id, generation)
) STRICT;

-- 会话级 Browser Operation；active identity lock 与全局容量 FIFO 的持久权威（DATA-BROWSER-003）。
CREATE TABLE browser_operations (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  identity_id                INTEGER NOT NULL REFERENCES browser_identities(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  identity_revision_id       INTEGER NOT NULL REFERENCES browser_identity_revisions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  profile_generation_id      INTEGER REFERENCES browser_profile_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  owner_attempt_id           INTEGER REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  kind                       TEXT NOT NULL CHECK (kind IN ('manual_login','authentication_probe','journey','exploration','deployment_verification')),
  actor_user_id              INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  actor_session_id           INTEGER REFERENCES sessions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  verification_manifest_item_id INTEGER REFERENCES verification_invocation_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  clone_identity             TEXT,
  state                      TEXT NOT NULL CHECK (state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect','Succeeded','Failed','Cancelled','Interrupted')),
  journey_catalog_digest     TEXT NOT NULL CHECK (length(journey_catalog_digest) = 64 AND journey_catalog_digest NOT GLOB '*[^0-9a-f]*'),
  journey_catalog_version    TEXT NOT NULL,
  journey_id                 TEXT,
  journey_version            INTEGER CHECK (journey_version IS NULL OR journey_version >= 1),
  probe_phase                TEXT CHECK (probe_phase IS NULL OR probe_phase IN ('revision_change','admission','completion','publish','mid_operation')),
  requested_at               TEXT NOT NULL,
  start_dispatched_at        TEXT,                   -- StartBrowserOperation 已写入控制流的持久 fence；Ack 丢失时仍证明物理启动结果未知
  lintel_boot_id             TEXT,                   -- Start 派发时冻结的 Lintel boot；是 operation 分配事实，不是在线状态投影
  lintel_connection_epoch    INTEGER CHECK (lintel_connection_epoch IS NULL OR lintel_connection_epoch >= 1),
  started_at                 TEXT,
  reconnect_deadline         TEXT,
  ended_at                   TEXT,
  stop_confirmed_at          TEXT,                   -- 物理 Chromium/隧道停止确认；终态可先提交，但本列非空前仍持有身份/容量 fence
  stop_confirmation_basis    TEXT CHECK (stop_confirmation_basis IS NULL OR stop_confirmation_basis IN ('not_dispatched','start_rejected','stop_ack','same_boot_cleanup_ack','inventory_absent','new_boot','new_boot_cleanup_confirmed','externally_fenced_storage_retired')),
  cleanup_state_hash         BLOB CHECK (cleanup_state_hash IS NULL OR length(cleanup_state_hash) = 32),
  start_rejected_at          TEXT,                   -- accepted=false 且非 NO_CAPACITY 的持久事实；与拒绝原因同事务写入
  start_reject_reason        TEXT CHECK (start_reject_reason IS NULL OR start_reject_reason IN ('identity_busy','profile_unavailable','authentication_required','input_unsupported','reconcile_required','stale_stream','internal')),
  terminal_reason            TEXT CHECK (terminal_reason IS NULL OR terminal_reason IN (
                               'client_closed_without_publish','grace_expired','session_revoked','new_boot','shutdown',
                               'slot_revoked','slot_replaced','profile_missing','profile_manifest_invalid','chromium_revision_mismatch',
                               'authentication_required','authentication_probe_unavailable','artifact_commit_failed','journey_failed',
                               'cancelled','parent_terminal','lease_expired','runtime_unavailable','browser_crashed','protocol_error')),
  trace_artifact_id          INTEGER REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  trace_integrity            TEXT CHECK (trace_integrity IS NULL OR trace_integrity IN ('complete','incomplete')),
  completion_digest          BLOB CHECK (completion_digest IS NULL OR length(completion_digest) = 32), -- CompleteBrowserOperation 重放裁决；Journey ResultProposal 使用独立 ledger
  row_version                INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  CHECK (
    (state IN ('Queued','WaitingForCapacity') AND started_at IS NULL AND reconnect_deadline IS NULL AND ended_at IS NULL AND terminal_reason IS NULL
      AND ((start_dispatched_at IS NULL AND lintel_boot_id IS NULL AND lintel_connection_epoch IS NULL)
        OR (state = 'WaitingForCapacity' AND start_dispatched_at IS NOT NULL AND lintel_boot_id IS NOT NULL AND lintel_connection_epoch IS NOT NULL)))
    OR (state = 'Starting' AND start_dispatched_at IS NOT NULL AND lintel_boot_id IS NOT NULL AND lintel_connection_epoch IS NOT NULL AND started_at IS NULL AND reconnect_deadline IS NULL AND ended_at IS NULL AND terminal_reason IS NULL)
    OR (state = 'Running' AND start_dispatched_at IS NOT NULL AND lintel_boot_id IS NOT NULL AND lintel_connection_epoch IS NOT NULL AND started_at IS NOT NULL AND reconnect_deadline IS NULL AND ended_at IS NULL AND terminal_reason IS NULL)
    OR (state = 'AwaitingReconnect' AND kind IN ('manual_login','deployment_verification') AND started_at IS NOT NULL AND reconnect_deadline IS NOT NULL AND ended_at IS NULL AND terminal_reason IS NULL)
    OR (state = 'Succeeded' AND started_at IS NOT NULL AND reconnect_deadline IS NULL AND ended_at IS NOT NULL AND terminal_reason IS NULL)
    OR (state IN ('Failed','Cancelled','Interrupted') AND ended_at IS NOT NULL AND terminal_reason IS NOT NULL)
  ),
  CHECK (
    (kind = 'manual_login' AND actor_user_id IS NOT NULL AND actor_session_id IS NULL AND verification_manifest_item_id IS NULL AND clone_identity IS NULL AND owner_attempt_id IS NULL AND journey_id IS NULL AND journey_version IS NULL AND probe_phase IS NULL)
    OR (kind = 'authentication_probe' AND actor_user_id IS NULL AND actor_session_id IS NULL AND verification_manifest_item_id IS NULL AND clone_identity IS NULL AND owner_attempt_id IS NULL AND journey_id IS NOT NULL AND journey_version IS NOT NULL AND probe_phase = 'revision_change' AND profile_generation_id IS NOT NULL)
    OR (kind = 'journey' AND actor_user_id IS NULL AND actor_session_id IS NULL AND verification_manifest_item_id IS NULL AND clone_identity IS NULL AND owner_attempt_id IS NOT NULL AND journey_id IS NOT NULL AND journey_version IS NOT NULL AND probe_phase IS NULL AND profile_generation_id IS NOT NULL)
    OR (kind = 'exploration' AND actor_user_id IS NULL AND actor_session_id IS NULL AND verification_manifest_item_id IS NULL AND clone_identity IS NULL AND owner_attempt_id IS NOT NULL AND journey_id IS NULL AND journey_version IS NULL AND probe_phase IS NULL AND profile_generation_id IS NOT NULL)
    OR (kind = 'deployment_verification' AND actor_user_id IS NULL AND actor_session_id IS NOT NULL AND verification_manifest_item_id IS NOT NULL AND clone_identity IS NOT NULL AND owner_attempt_id IS NULL AND journey_id IS NULL AND journey_version IS NULL AND probe_phase IS NULL AND profile_generation_id IS NOT NULL)
  ),
  CHECK (terminal_reason IS NULL OR
    (kind = 'manual_login' AND terminal_reason IN (
      'client_closed_without_publish','grace_expired','session_revoked','new_boot','shutdown','slot_revoked','slot_replaced',
      'cancelled','runtime_unavailable','browser_crashed','protocol_error'))
    OR (kind = 'authentication_probe' AND terminal_reason IN (
      'new_boot','shutdown','slot_revoked','slot_replaced','profile_missing','profile_manifest_invalid','chromium_revision_mismatch',
      'authentication_required','authentication_probe_unavailable','cancelled','runtime_unavailable','browser_crashed','protocol_error'))
    OR (kind = 'journey' AND terminal_reason IN (
      'new_boot','shutdown','slot_revoked','slot_replaced','profile_missing','profile_manifest_invalid','chromium_revision_mismatch',
      'authentication_required','authentication_probe_unavailable','artifact_commit_failed','journey_failed','cancelled','parent_terminal',
      'lease_expired','runtime_unavailable','browser_crashed','protocol_error'))
    OR (kind = 'exploration' AND terminal_reason IN (
      'new_boot','shutdown','slot_revoked','slot_replaced','profile_missing','profile_manifest_invalid','chromium_revision_mismatch',
      'authentication_required','authentication_probe_unavailable','artifact_commit_failed','cancelled','parent_terminal',
      'lease_expired','runtime_unavailable','browser_crashed','protocol_error'))
    OR (kind = 'deployment_verification' AND terminal_reason IN (
      'new_boot','shutdown','slot_revoked','slot_replaced','profile_missing','profile_manifest_invalid','chromium_revision_mismatch',
      'authentication_required','grace_expired','session_revoked','cancelled','runtime_unavailable','browser_crashed','protocol_error'))),
  CHECK ((stop_confirmed_at IS NULL) = (stop_confirmation_basis IS NULL)),
  CHECK (stop_confirmed_at IS NULL OR state IN ('Succeeded','Failed','Cancelled','Interrupted')),
  CHECK ((start_rejected_at IS NULL) = (start_reject_reason IS NULL)),
  CHECK (start_rejected_at IS NULL OR (start_dispatched_at IS NOT NULL AND started_at IS NULL AND state = 'Failed')),
  CHECK (stop_confirmation_basis IS NULL
    OR (stop_confirmation_basis = 'not_dispatched' AND start_dispatched_at IS NULL)
    OR (stop_confirmation_basis = 'start_rejected' AND start_rejected_at IS NOT NULL)
    OR (stop_confirmation_basis IN ('stop_ack','same_boot_cleanup_ack','inventory_absent','new_boot','new_boot_cleanup_confirmed','externally_fenced_storage_retired') AND start_dispatched_at IS NOT NULL)),
  CHECK ((stop_confirmation_basis IN ('same_boot_cleanup_ack','new_boot_cleanup_confirmed')) = (cleanup_state_hash IS NOT NULL)),
  CHECK (kind <> 'deployment_verification' OR stop_confirmation_basis IS NULL OR stop_confirmation_basis IN ('not_dispatched','start_rejected','same_boot_cleanup_ack','new_boot_cleanup_confirmed','externally_fenced_storage_retired')),
  CHECK (terminal_reason <> 'artifact_commit_failed' OR state = 'Failed'),
  CHECK (terminal_reason <> 'journey_failed' OR state = 'Failed'),
  CHECK (state NOT IN ('Succeeded','Failed','Cancelled','Interrupted')
    OR ((start_dispatched_at IS NOT NULL AND lintel_boot_id IS NOT NULL AND lintel_connection_epoch IS NOT NULL)
      OR (start_dispatched_at IS NULL AND lintel_boot_id IS NULL AND lintel_connection_epoch IS NULL
        AND stop_confirmed_at IS NOT NULL AND stop_confirmation_basis = 'not_dispatched'))),
  CHECK (completion_digest IS NULL OR state IN ('Succeeded','Failed','Cancelled','Interrupted')),
  CHECK ((trace_artifact_id IS NULL) = (trace_integrity IS NULL)),
  CHECK (trace_artifact_id IS NULL OR started_at IS NOT NULL), -- trace 是启动后事实；INSERT 必须以 Queued 创建，故不得预置或借用其它 operation 的 trace
  CHECK (kind = 'exploration' OR trace_integrity IS NULL OR state <> 'Succeeded'),
  CHECK (kind <> 'journey' OR state <> 'Failed' OR terminal_reason <> 'journey_failed' OR trace_artifact_id IS NOT NULL),
  CHECK (state <> 'Succeeded' OR kind <> 'exploration' OR trace_integrity = 'complete')
) STRICT;
CREATE INDEX idx_browser_operations_identity ON browser_operations (identity_id, requested_at);
CREATE INDEX idx_browser_operations_fifo ON browser_operations (id) WHERE state IN ('Queued','WaitingForCapacity');
CREATE UNIQUE INDEX ux_browser_operation_active_identity ON browser_operations (identity_id)
  WHERE state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect') OR stop_confirmed_at IS NULL;
CREATE UNIQUE INDEX ux_browser_operation_journey_attempt ON browser_operations (owner_attempt_id)
  WHERE kind = 'journey';
CREATE UNIQUE INDEX ux_browser_operation_active_exploration_parent ON browser_operations (owner_attempt_id)
  WHERE kind = 'exploration' AND state IN ('Queued','WaitingForCapacity','Starting','Running');

-- Deployment Verification 的功能结论与 cleanup 结论分别冻结；晚到 stop confirmation 只调和
-- browser_operations.stop_confirmed_at，不得改写本结果。
CREATE TABLE browser_deployment_verification_results (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  operation_id               INTEGER NOT NULL UNIQUE REFERENCES browser_operations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  verification_result_id     INTEGER NOT NULL UNIQUE REFERENCES verification_item_results(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  functional_outcome         TEXT NOT NULL CHECK (functional_outcome IN ('passed','warned','failed')),
  functional_evidence_digest TEXT NOT NULL CHECK (length(functional_evidence_digest) = 64 AND functional_evidence_digest NOT GLOB '*[^0-9a-f]*'),
  cleanup_outcome            TEXT NOT NULL CHECK (cleanup_outcome IN ('clean','residue','indeterminate')),
  original_boot_id           TEXT NOT NULL,
  cleanup_boot_id            TEXT,
  cleanup_epoch              INTEGER CHECK (cleanup_epoch IS NULL OR cleanup_epoch >= 1),
  cleanup_state_hash         TEXT CHECK (cleanup_state_hash IS NULL OR (length(cleanup_state_hash) = 64 AND cleanup_state_hash NOT GLOB '*[^0-9a-f]*')),
  stop_fence_digest          TEXT CHECK (stop_fence_digest IS NULL OR (length(stop_fence_digest) = 64 AND stop_fence_digest NOT GLOB '*[^0-9a-f]*')),
  clone_identity             TEXT NOT NULL,
  operation_process_count    INTEGER CHECK (operation_process_count IS NULL OR operation_process_count >= 0),
  cgroup_process_count       INTEGER CHECK (cgroup_process_count IS NULL OR cgroup_process_count >= 0),
  chromium_process_count     INTEGER CHECK (chromium_process_count IS NULL OR chromium_process_count >= 0),
  x0vnc_process_count        INTEGER CHECK (x0vnc_process_count IS NULL OR x0vnc_process_count >= 0),
  novnc_tunnel_count         INTEGER CHECK (novnc_tunnel_count IS NULL OR novnc_tunnel_count >= 0),
  clone_namespace_count      INTEGER CHECK (clone_namespace_count IS NULL OR clone_namespace_count >= 0),
  temporary_file_count       INTEGER CHECK (temporary_file_count IS NULL OR temporary_file_count >= 0),
  runtime_handle_count       INTEGER CHECK (runtime_handle_count IS NULL OR runtime_handle_count >= 0),
  slot_lease_count           INTEGER CHECK (slot_lease_count IS NULL OR slot_lease_count >= 0),
  result_digest              TEXT NOT NULL CHECK (length(result_digest) = 64 AND result_digest NOT GLOB '*[^0-9a-f]*'),
  created_at                 TEXT NOT NULL,
  CHECK ((cleanup_outcome IN ('clean','residue')
      AND cleanup_boot_id IS NOT NULL AND cleanup_epoch IS NOT NULL AND cleanup_state_hash IS NOT NULL AND stop_fence_digest IS NOT NULL
       AND operation_process_count IS NOT NULL AND cgroup_process_count IS NOT NULL AND chromium_process_count IS NOT NULL
       AND x0vnc_process_count IS NOT NULL AND novnc_tunnel_count IS NOT NULL AND clone_namespace_count IS NOT NULL
       AND temporary_file_count IS NOT NULL AND runtime_handle_count IS NOT NULL AND slot_lease_count IS NOT NULL)
     OR (cleanup_outcome = 'indeterminate' AND cleanup_state_hash IS NULL AND stop_fence_digest IS NULL
       AND operation_process_count IS NULL AND cgroup_process_count IS NULL AND chromium_process_count IS NULL
       AND x0vnc_process_count IS NULL AND novnc_tunnel_count IS NULL AND clone_namespace_count IS NULL
       AND temporary_file_count IS NULL AND runtime_handle_count IS NULL AND slot_lease_count IS NULL)),
   CHECK (cleanup_outcome <> 'clean' OR
     (operation_process_count + cgroup_process_count + chromium_process_count + x0vnc_process_count + novnc_tunnel_count
       + clone_namespace_count + temporary_file_count + runtime_handle_count + slot_lease_count) = 0),
   CHECK (cleanup_outcome <> 'residue' OR
     (operation_process_count + cgroup_process_count + chromium_process_count + x0vnc_process_count + novnc_tunnel_count
       + clone_namespace_count + temporary_file_count + runtime_handle_count + slot_lease_count) > 0)
) STRICT;

CREATE TABLE browser_probe_results (
  id                       INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  operation_id             INTEGER NOT NULL REFERENCES browser_operations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  probe_seq                INTEGER NOT NULL CHECK (probe_seq >= 1),
  phase                    TEXT NOT NULL CHECK (phase IN ('revision_change','admission','completion','publish','mid_operation')),
  identity_revision_id     INTEGER NOT NULL REFERENCES browser_identity_revisions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  journey_id               TEXT NOT NULL,
  journey_version          INTEGER NOT NULL CHECK (journey_version >= 1),
  journey_catalog_digest   TEXT NOT NULL CHECK (length(journey_catalog_digest) = 64 AND journey_catalog_digest NOT GLOB '*[^0-9a-f]*'),
  journey_catalog_version  TEXT NOT NULL,
  result                   TEXT NOT NULL CHECK (result IN ('Authenticated','Unauthenticated','Indeterminate')),
  reason_code              TEXT,
  observed_at              TEXT NOT NULL,
  UNIQUE (operation_id, probe_seq),
  CHECK ((result = 'Indeterminate' AND reason_code IS NOT NULL) OR (result <> 'Indeterminate' AND reason_code IS NULL))
) STRICT;

CREATE TABLE browser_profile_reconciliations (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  boot_id                    TEXT NOT NULL,
  connection_epoch           INTEGER NOT NULL CHECK (connection_epoch >= 1),
  identity_id                INTEGER NOT NULL REFERENCES browser_identities(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  profile_generation_id      INTEGER NOT NULL REFERENCES browser_profile_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  result                     TEXT NOT NULL CHECK (result IN ('compatible','missing','manifest_invalid','chromium_revision_mismatch')),
  observed_chromium_revision TEXT,
  observed_manifest_digest   TEXT CHECK (observed_manifest_digest IS NULL OR (length(observed_manifest_digest) = 64 AND observed_manifest_digest NOT GLOB '*[^0-9a-f]*')),
  reconciled_at              TEXT NOT NULL,
  UNIQUE (boot_id, connection_epoch, identity_id),
  CHECK ((result = 'missing' AND observed_chromium_revision IS NULL AND observed_manifest_digest IS NULL)
    OR result = 'manifest_invalid'
    OR (result IN ('compatible','chromium_revision_mismatch')
      AND observed_chromium_revision IS NOT NULL AND observed_manifest_digest IS NOT NULL))
) STRICT;

CREATE TABLE browser_exploration_actions (
  id                       INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  operation_id             INTEGER NOT NULL REFERENCES browser_operations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  action_seq               INTEGER NOT NULL CHECK (action_seq >= 1),
  child_attempt_id         INTEGER NOT NULL UNIQUE REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  tool_call_id             INTEGER NOT NULL UNIQUE REFERENCES tool_calls(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  action_kind              TEXT NOT NULL CHECK (action_kind IN (
                             'open','close_session','switch_page','close_page','goto','back','forward','reload','click','fill','select',
                             'check','uncheck','press','scroll','read','screenshot','wait_for','accept_dialog','dismiss_dialog')),
  page_id                  TEXT,
  origin                   TEXT,
  target_description       TEXT,
  started_at               TEXT NOT NULL,
  ended_at                 TEXT,
  outcome                  TEXT CHECK (outcome IS NULL OR outcome IN ('success','recoverable_error','session_closed')),
  error_code               TEXT CHECK (error_code IS NULL OR error_code IN (
                             'ElementNotFound','ElementNotUnique','ActionTimeout','NavigationFailed','DialogBlocked','DownloadBlocked',
                             'ElementReferenceStale','AuthenticationRequired','AuthenticationProbeUnavailable',
                             'BrowserCrashed','ProfileUnavailable','RuntimeUnavailable','ProtocolError','ArtifactCommitFailed',
                             'Cancelled','LeaseExpired','ParentTerminated')),
  observation_version      INTEGER CHECK (observation_version IS NULL OR observation_version >= 1),
  observation_digest       TEXT CHECK (observation_digest IS NULL OR (length(observation_digest) = 64 AND observation_digest NOT GLOB '*[^0-9a-f]*')),
  observation_size_bytes   INTEGER CHECK (observation_size_bytes IS NULL OR observation_size_bytes >= 0),
  screenshot_artifact_id   INTEGER REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  UNIQUE (operation_id, action_seq),
  CHECK ((action_seq = 1 AND action_kind = 'open') OR (action_seq > 1 AND action_kind <> 'open')),
  CHECK (
    (outcome IS NULL AND ended_at IS NULL AND error_code IS NULL)
    OR (outcome = 'success' AND ended_at IS NOT NULL AND error_code IS NULL)
    OR (outcome IN ('recoverable_error','session_closed') AND ended_at IS NOT NULL AND error_code IS NOT NULL)
  ),
  CHECK (
    (outcome IS NULL AND observation_version IS NULL AND observation_digest IS NULL AND observation_size_bytes IS NULL AND screenshot_artifact_id IS NULL)
    OR (outcome IN ('success','recoverable_error') AND observation_version IS NOT NULL AND observation_digest IS NOT NULL AND observation_size_bytes IS NOT NULL)
    OR (outcome = 'session_closed' AND (
      (observation_version IS NULL AND observation_digest IS NULL AND observation_size_bytes IS NULL AND screenshot_artifact_id IS NULL)
      OR (observation_version IS NOT NULL AND observation_digest IS NOT NULL AND observation_size_bytes IS NOT NULL)))
  ),
  CHECK (outcome <> 'recoverable_error' OR error_code IN (
    'ElementNotFound','ElementNotUnique','ActionTimeout','NavigationFailed','DialogBlocked','DownloadBlocked','ElementReferenceStale')),
  CHECK (outcome <> 'session_closed' OR error_code IN (
    'AuthenticationRequired','AuthenticationProbeUnavailable','BrowserCrashed','ProfileUnavailable','RuntimeUnavailable',
    'ProtocolError','ArtifactCommitFailed','Cancelled','LeaseExpired','ParentTerminated')),
  CHECK (screenshot_artifact_id IS NULL OR (action_kind = 'screenshot' AND outcome = 'success'))
) STRICT;
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
  state                     TEXT NOT NULL CHECK (state IN ('Queued','Running','Completed','CompletedWithGaps','Failed','Cancelled','Interrupted','SkippedOverlap')),
  row_version               INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  evidence_at               TEXT,                    -- 真正采证开始时生成
  rerun_of_id               INTEGER REFERENCES inspection_runs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at                TEXT NOT NULL,
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
CREATE UNIQUE INDEX ux_inspection_run_scheduled ON inspection_runs (business_system_id, plan_key, scheduled_for) WHERE scheduled_for IS NOT NULL;
CREATE UNIQUE INDEX ux_inspection_run_active ON inspection_runs (business_system_id, plan_key)
  WHERE state IN ('Queued','Running');
CREATE INDEX idx_inspection_runs_plan ON inspection_runs (business_system_id, plan_key, created_at DESC);

CREATE TABLE inspection_check_results (
  id            INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  run_id        INTEGER NOT NULL REFERENCES inspection_runs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  check_key     TEXT NOT NULL,
  status        TEXT NOT NULL CHECK (status IN ('ok','error','gap')),
  evidence_id   INTEGER REFERENCES evidence(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  attempt_id    INTEGER REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- browser result 绑定精确 inspection_collection Attempt；PromQL 为 NULL
  result_digest BLOB CHECK (result_digest IS NULL OR length(result_digest) = 32), -- operation-less Journey Result 重放摘要；有 operation 时与 browser_journey_results 同值
  gap_reason  TEXT CHECK (gap_reason IS NULL OR gap_reason IN (
                'runtime_unavailable','authentication_required','authentication_probe_unavailable','identity_busy',
                'artifact_commit_failed','journey_failed','query_failed','partial_response','no_data','cancelled','interrupted')),
  created_at  TEXT NOT NULL,
  UNIQUE (run_id, check_key),
  CHECK (
    (status = 'ok' AND evidence_id IS NOT NULL AND gap_reason IS NULL)
    OR (status IN ('error','gap') AND gap_reason IS NOT NULL)
  ),
  CHECK (attempt_id IS NOT NULL OR result_digest IS NULL),
  CHECK (result_digest IS NULL OR status = 'gap' OR evidence_id IS NOT NULL)
) STRICT;
CREATE UNIQUE INDEX ux_inspection_check_result_evidence ON inspection_check_results (evidence_id) WHERE evidence_id IS NOT NULL;

-- Config Verification Run：prepublish 与 deployment_acceptance 共用唯一机械执行模型。
-- prepublish 只绑定未发布草稿并可被 Label Contract 联合激活采用；deployment_acceptance 只绑定
-- manifest 创建时的 current published config/Label Contract，绝不移动发布指针或成为激活证据。
CREATE TABLE config_verification_runs (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  purpose                   TEXT NOT NULL CHECK (purpose IN ('prepublish','deployment_acceptance')),
  business_system_id        INTEGER NOT NULL REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  config_version_id         INTEGER NOT NULL REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  label_contract_version_id INTEGER NOT NULL REFERENCES label_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  verification_manifest_item_id INTEGER REFERENCES verification_invocation_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  state                     TEXT NOT NULL CHECK (state IN ('Queued','Running','Passed','Failed','Cancelled','Interrupted')),
  row_version               INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  evidence_at               TEXT,                    -- 真正开始采证时生成
  result_detail             TEXT,                    -- Failed/Cancelled/Interrupted 的人工可读说明（非秘密）
  created_by                INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at                TEXT NOT NULL,
  CHECK (
    (state IN ('Queued','Running','Passed') AND result_detail IS NULL)
    OR (state IN ('Failed','Cancelled','Interrupted') AND result_detail IS NOT NULL)
  ),
  CHECK ((purpose = 'prepublish' AND verification_manifest_item_id IS NULL)
      OR (purpose = 'deployment_acceptance' AND verification_manifest_item_id IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX ux_config_verification_run_active ON config_verification_runs (purpose, business_system_id, config_version_id)
  WHERE state IN ('Queued','Running');
CREATE INDEX idx_config_verification_runs_version ON config_verification_runs (config_version_id, created_at DESC);

-- Resource Refresh Run 是已发布配置的一次完整发现采集；它是 scheduler/manual 命令和每项
-- observed_refresh_log 的唯一持久根，不能以 Config Verification Run 或当前配置指针替代。
CREATE TABLE resource_refresh_runs (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  business_system_id        INTEGER NOT NULL REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  config_version_id         INTEGER NOT NULL REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  label_contract_version_id INTEGER NOT NULL REFERENCES label_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  trigger_kind              TEXT NOT NULL CHECK (trigger_kind IN ('manual','schedule')),
  scheduled_for             TEXT,
  state                     TEXT NOT NULL CHECK (state IN ('Queued','Running','Completed','CompletedWithWarnings','Failed','Cancelled','Interrupted')),
  row_version               INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  evidence_at               TEXT,
  result_detail             TEXT,
  created_by                INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at                TEXT NOT NULL,
  CHECK ((trigger_kind = 'manual' AND scheduled_for IS NULL) OR (trigger_kind = 'schedule' AND scheduled_for IS NOT NULL)),
  CHECK ((state IN ('Queued','Running','Completed','CompletedWithWarnings') AND result_detail IS NULL)
      OR (state IN ('Failed','Cancelled','Interrupted') AND result_detail IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX ux_resource_refresh_run_active ON resource_refresh_runs (business_system_id)
  WHERE state IN ('Queued','Running');
CREATE UNIQUE INDEX ux_resource_refresh_run_scheduled ON resource_refresh_runs (business_system_id, config_version_id, scheduled_for)
  WHERE scheduled_for IS NOT NULL;
CREATE INDEX idx_resource_refresh_runs_system ON resource_refresh_runs (business_system_id, created_at DESC);

CREATE TABLE config_verification_run_check_results (
  id          INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  verification_run_id INTEGER NOT NULL REFERENCES config_verification_runs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  plan_key    TEXT NOT NULL,
  check_key   TEXT NOT NULL,
  status      TEXT NOT NULL CHECK (status IN ('ok','error','gap')),
  evidence_id INTEGER REFERENCES evidence(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  attempt_id  INTEGER REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- 每个机械 check 绑定同 Config Verification Run/check 的 inspection_collection Attempt
  result_digest BLOB CHECK (result_digest IS NULL OR length(result_digest) = 32), -- ResultProposal 重放摘要；Journey 有 operation 时与 browser_journey_results 同值
  gap_reason  TEXT CHECK (gap_reason IS NULL OR gap_reason IN (
                'runtime_unavailable','authentication_required','authentication_probe_unavailable','identity_busy',
                'artifact_commit_failed','journey_failed','query_failed','partial_response','no_data','cancelled','interrupted')),
  warnings_json TEXT CHECK (warnings_json IS NULL OR json_valid(warnings_json)),
  created_at  TEXT NOT NULL,
  UNIQUE (verification_run_id, plan_key, check_key),
  CHECK (
    (status = 'ok' AND evidence_id IS NOT NULL AND gap_reason IS NULL)
    OR (status IN ('error','gap') AND gap_reason IS NOT NULL)
  ),
  CHECK (attempt_id IS NOT NULL OR result_digest IS NULL),
  CHECK (result_digest IS NULL OR status = 'gap' OR evidence_id IS NOT NULL)
) STRICT;
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

-- Deployment Acceptance：manifest/items/results/conflicts/receipt 是唯一持久证据闭包；
-- 不建立可变 invocation status 或 current acceptance pointer。
CREATE TABLE verification_invocation_manifests (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  admin_session_id           INTEGER NOT NULL REFERENCES sessions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  principal_user_id          INTEGER NOT NULL REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  release_subject_digest     TEXT NOT NULL CHECK (length(release_subject_digest) = 64 AND release_subject_digest NOT GLOB '*[^0-9a-f]*'),
  catalog_digest             TEXT NOT NULL CHECK (length(catalog_digest) = 64 AND catalog_digest NOT GLOB '*[^0-9a-f]*'),
  result_profile_digest      TEXT NOT NULL CHECK (length(result_profile_digest) = 64 AND result_profile_digest NOT GLOB '*[^0-9a-f]*'),
  deployment_config_digest   TEXT NOT NULL CHECK (length(deployment_config_digest) = 64 AND deployment_config_digest NOT GLOB '*[^0-9a-f]*'),
  public_origin_digest       TEXT NOT NULL CHECK (length(public_origin_digest) = 64 AND public_origin_digest NOT GLOB '*[^0-9a-f]*'),
  applicable_set_digest      TEXT NOT NULL CHECK (length(applicable_set_digest) = 64 AND applicable_set_digest NOT GLOB '*[^0-9a-f]*'),
  item_count                 INTEGER NOT NULL CHECK (item_count >= 1),
  item_set_digest            TEXT NOT NULL CHECK (length(item_set_digest) = 64 AND item_set_digest NOT GLOB '*[^0-9a-f]*'),
  manifest_digest            TEXT NOT NULL UNIQUE CHECK (length(manifest_digest) = 64 AND manifest_digest NOT GLOB '*[^0-9a-f]*'),
  canonical_input_digest     TEXT NOT NULL CHECK (length(canonical_input_digest) = 64 AND canonical_input_digest NOT GLOB '*[^0-9a-f]*'),
  started_at                 TEXT NOT NULL,
  deadline_at                TEXT NOT NULL,
  created_at                 TEXT NOT NULL,
  CHECK (julianday(deadline_at) = julianday(started_at, '+8 hours'))
) STRICT;

CREATE TABLE verification_invocation_items (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  invocation_id              INTEGER NOT NULL REFERENCES verification_invocation_manifests(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  item_seq                   INTEGER NOT NULL CHECK (item_seq >= 1),
  scenario_id                TEXT NOT NULL CHECK (length(scenario_id) > 0),
  cell_id                    TEXT NOT NULL CHECK (length(cell_id) > 0),
  object_kind                TEXT NOT NULL CHECK (object_kind IN ('deployment','connection','config','browser_identity','ui_observation')),
  input_digest               TEXT NOT NULL CHECK (length(input_digest) = 64 AND input_digest NOT GLOB '*[^0-9a-f]*'),
  created_at                 TEXT NOT NULL,
  UNIQUE (invocation_id, item_seq),
  UNIQUE (invocation_id, scenario_id, cell_id, object_kind, input_digest)
) STRICT;

CREATE TABLE verification_deployment_item_locators (
  item_id                    INTEGER PRIMARY KEY REFERENCES verification_invocation_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  release_subject_digest     TEXT NOT NULL CHECK (length(release_subject_digest) = 64 AND release_subject_digest NOT GLOB '*[^0-9a-f]*'),
  deployment_config_digest   TEXT NOT NULL CHECK (length(deployment_config_digest) = 64 AND deployment_config_digest NOT GLOB '*[^0-9a-f]*'),
  public_origin_digest       TEXT NOT NULL CHECK (length(public_origin_digest) = 64 AND public_origin_digest NOT GLOB '*[^0-9a-f]*'),
  backend                    TEXT NOT NULL CHECK (backend IN ('compose','kubernetes')),
  architecture               TEXT NOT NULL CHECK (architecture IN ('linux/amd64','linux/arm64'))
) STRICT;
CREATE TABLE verification_connection_item_locators (
  item_id                    INTEGER PRIMARY KEY REFERENCES verification_invocation_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  connection_id              INTEGER NOT NULL REFERENCES connections(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  connection_revision_id     INTEGER NOT NULL REFERENCES connection_revisions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  credential_generation_id   INTEGER NOT NULL REFERENCES credential_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  root_binding_revision      INTEGER NOT NULL CHECK (root_binding_revision >= 1),
  probe_contract_digest      TEXT NOT NULL CHECK (length(probe_contract_digest) = 64 AND probe_contract_digest NOT GLOB '*[^0-9a-f]*')
) STRICT;
CREATE TABLE verification_config_item_locators (
  item_id                    INTEGER PRIMARY KEY REFERENCES verification_invocation_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  business_system_id         INTEGER NOT NULL REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  config_version_id          INTEGER NOT NULL REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  label_contract_version_id  INTEGER NOT NULL REFERENCES label_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT
) STRICT;
CREATE TABLE verification_browser_identity_item_locators (
  item_id                    INTEGER PRIMARY KEY REFERENCES verification_invocation_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  browser_identity_id        INTEGER NOT NULL REFERENCES browser_identities(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  identity_revision_id       INTEGER NOT NULL REFERENCES browser_identity_revisions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  profile_generation_id      INTEGER NOT NULL REFERENCES browser_profile_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  current_inventory_digest   TEXT NOT NULL CHECK (length(current_inventory_digest) = 64 AND current_inventory_digest NOT GLOB '*[^0-9a-f]*')
) STRICT;
CREATE TABLE verification_ui_observation_item_locators (
  item_id                    INTEGER PRIMARY KEY REFERENCES verification_invocation_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  browser_artifact           TEXT NOT NULL CHECK (browser_artifact IN ('playwright_chromium','branded_chrome')),
  browser_version            TEXT NOT NULL,
  architecture               TEXT NOT NULL CHECK (architecture IN ('linux/amd64','linux/arm64')),
  viewport_css_px            INTEGER NOT NULL CHECK (viewport_css_px IN (320,768,1024,1440)),
  motion                     TEXT NOT NULL CHECK (motion IN ('normal','reduced')),
  CHECK (browser_artifact <> 'branded_chrome' OR architecture = 'linux/amd64')
) STRICT;

CREATE TABLE verification_item_results (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  item_id                    INTEGER NOT NULL REFERENCES verification_invocation_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  input_digest               TEXT NOT NULL CHECK (length(input_digest) = 64 AND input_digest NOT GLOB '*[^0-9a-f]*'),
  result_digest              TEXT NOT NULL CHECK (length(result_digest) = 64 AND result_digest NOT GLOB '*[^0-9a-f]*'),
  producer_type              TEXT NOT NULL CHECK (producer_type IN ('quoin','runtime','deployment_helper','admin_observation')),
  outcome                    TEXT NOT NULL CHECK (outcome IN ('passed','warned','failed')),
  category                   TEXT NOT NULL CHECK (category IN ('passed','functional_assertion_failed','cleanup_residue','verifier_conflict','subject_drift','environment_unavailable','operator_cancelled','infrastructure_interrupted','cleanup_indeterminate','not_run','verifier_invariant_violation')),
  observed_at                TEXT NOT NULL,
  committed_at               TEXT NOT NULL,
  evidence_index_digest      TEXT NOT NULL CHECK (length(evidence_index_digest) = 64 AND evidence_index_digest NOT GLOB '*[^0-9a-f]*'),
  artifact_id                INTEGER REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  UNIQUE (item_id, input_digest, result_digest),
  CHECK (julianday(committed_at) >= julianday(observed_at)),
  CHECK ((outcome = 'passed' AND category = 'passed')
      OR (outcome = 'failed' AND category IN ('functional_assertion_failed','cleanup_residue','verifier_conflict','verifier_invariant_violation'))
      OR (outcome = 'warned' AND category IN ('subject_drift','environment_unavailable','operator_cancelled','infrastructure_interrupted','cleanup_indeterminate','not_run')))
) STRICT;

CREATE TABLE verification_result_conflicts (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  item_id                    INTEGER NOT NULL REFERENCES verification_invocation_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  first_result_id            INTEGER NOT NULL REFERENCES verification_item_results(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  conflicting_result_id      INTEGER NOT NULL REFERENCES verification_item_results(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at                 TEXT NOT NULL,
  UNIQUE (item_id, first_result_id, conflicting_result_id),
  CHECK (first_result_id <> conflicting_result_id)
) STRICT;

CREATE TABLE verification_helper_imports (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  invocation_id              INTEGER NOT NULL REFERENCES verification_invocation_manifests(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  request_digest             TEXT NOT NULL CHECK (length(request_digest) = 64 AND request_digest NOT GLOB '*[^0-9a-f]*'),
  report_digest              TEXT NOT NULL CHECK (length(report_digest) = 64 AND report_digest NOT GLOB '*[^0-9a-f]*'),
  helper_reported_started_at TEXT NOT NULL,
  helper_reported_finished_at TEXT NOT NULL,
  received_at                TEXT NOT NULL,
  artifact_id                INTEGER NOT NULL REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  UNIQUE (invocation_id, request_digest, report_digest),
  CHECK (julianday(helper_reported_finished_at) >= julianday(helper_reported_started_at))
) STRICT;

CREATE TABLE verification_typed_observations (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  result_id                  INTEGER NOT NULL UNIQUE REFERENCES verification_item_results(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  admin_session_id           INTEGER NOT NULL REFERENCES sessions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  visual_result              TEXT NOT NULL CHECK (visual_result IN ('passed','failed')),
  motion_result              TEXT NOT NULL CHECK (motion_result IN ('passed','failed')),
  focus_occlusion_result     TEXT NOT NULL CHECK (focus_occlusion_result IN ('passed','failed')),
  note                       TEXT,
  submitted_at               TEXT NOT NULL
) STRICT;

CREATE TABLE verification_subject_drifts (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  invocation_id              INTEGER NOT NULL REFERENCES verification_invocation_manifests(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  object_kind                TEXT NOT NULL CHECK (object_kind IN ('deployment','connection','config','browser_identity','ui_observation')),
  drift_field                TEXT NOT NULL CHECK (drift_field IN ('release_subject_digest','deployment_config_digest','public_origin_digest','connection_revision','credential_generation','root_binding_revision','probe_contract_digest','config_version','label_contract_version','browser_identity_revision','browser_profile_generation','browser_inventory_observation','browser_artifact_digest','browser_artifact_version')),
  item_id                    INTEGER NOT NULL REFERENCES verification_invocation_items(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  frozen_digest              TEXT NOT NULL CHECK (length(frozen_digest) = 64 AND frozen_digest NOT GLOB '*[^0-9a-f]*'),
  current_digest             TEXT NOT NULL CHECK (length(current_digest) = 64 AND current_digest NOT GLOB '*[^0-9a-f]*'),
  observed_at                TEXT NOT NULL,
  UNIQUE (invocation_id, object_kind, item_id, current_digest),
  CHECK (frozen_digest <> current_digest),
  CHECK ((object_kind = 'deployment' AND drift_field IN ('release_subject_digest','deployment_config_digest','public_origin_digest'))
      OR (object_kind = 'connection' AND drift_field IN ('connection_revision','credential_generation','root_binding_revision','probe_contract_digest'))
      OR (object_kind = 'config' AND drift_field IN ('config_version','label_contract_version'))
      OR (object_kind = 'browser_identity' AND drift_field IN ('browser_identity_revision','browser_profile_generation','browser_inventory_observation'))
      OR (object_kind = 'ui_observation' AND drift_field IN ('release_subject_digest','public_origin_digest','browser_artifact_digest','browser_artifact_version')))
) STRICT;

CREATE TABLE verification_finalization_receipts (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  invocation_id              INTEGER NOT NULL UNIQUE REFERENCES verification_invocation_manifests(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  manifest_digest            TEXT NOT NULL CHECK (length(manifest_digest) = 64 AND manifest_digest NOT GLOB '*[^0-9a-f]*'),
  applicable_set_digest      TEXT NOT NULL CHECK (length(applicable_set_digest) = 64 AND applicable_set_digest NOT GLOB '*[^0-9a-f]*'),
  item_set_digest            TEXT NOT NULL CHECK (length(item_set_digest) = 64 AND item_set_digest NOT GLOB '*[^0-9a-f]*'),
  result_set_digest          TEXT NOT NULL CHECK (length(result_set_digest) = 64 AND result_set_digest NOT GLOB '*[^0-9a-f]*'),
  helper_import_set_digest   TEXT NOT NULL CHECK (length(helper_import_set_digest) = 64 AND helper_import_set_digest NOT GLOB '*[^0-9a-f]*'),
  typed_observation_set_digest TEXT NOT NULL CHECK (length(typed_observation_set_digest) = 64 AND typed_observation_set_digest NOT GLOB '*[^0-9a-f]*'),
  conflict_set_digest        TEXT NOT NULL CHECK (length(conflict_set_digest) = 64 AND conflict_set_digest NOT GLOB '*[^0-9a-f]*'),
  subject_drift_digest       TEXT NOT NULL CHECK (length(subject_drift_digest) = 64 AND subject_drift_digest NOT GLOB '*[^0-9a-f]*'),
  overall_outcome            TEXT NOT NULL CHECK (overall_outcome IN ('passed','warned','failed')),
  final_result_digest        TEXT NOT NULL CHECK (length(final_result_digest) = 64 AND final_result_digest NOT GLOB '*[^0-9a-f]*'),
  canonical_artifact_id      INTEGER NOT NULL UNIQUE REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  snapshot_at                TEXT NOT NULL,
  finalized_at               TEXT NOT NULL CHECK (julianday(finalized_at) >= julianday(snapshot_at)),
  finalized_by_type          TEXT NOT NULL CHECK (finalized_by_type IN ('initiating_admin_session','system_deadline')),
  finalized_by_session_id    INTEGER REFERENCES sessions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CHECK ((finalized_by_type = 'initiating_admin_session' AND finalized_by_session_id IS NOT NULL)
      OR (finalized_by_type = 'system_deadline' AND finalized_by_session_id IS NULL))
) STRICT;

-- ============================================================================
-- 8. 执行尝试、模型/工具调用、证据与报告
-- ============================================================================

CREATE TABLE execution_attempts (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  attempt_type              TEXT NOT NULL CHECK (attempt_type IN
                              ('initial_analysis','investigation','inspection_analysis','knowledge_extraction','embedding',
                               'inspection_collection','browser_exploration','connection_probe')),
  scope_type                TEXT NOT NULL CHECK (scope_type IN
                               ('analysis','investigation','run','knowledge_import_batch','embedding_generation','connection','run_check','config_verification_run','resource_refresh_run')),
  scope_id                  INTEGER NOT NULL,
  plan_key                  TEXT,   -- config_verification_run 子 Attempt 必填；其它 scope 为空
  check_key                 TEXT,   -- run_check/config_verification_run 子 Attempt 非空；其它 scope 为空
  discovery_key             TEXT,   -- resource_refresh_run 子 Attempt 必填；其它 scope 为空
  state                     TEXT NOT NULL CHECK (state IN ('Queued','Assigned','Running','Cancelling','Succeeded','Failed','Cancelled','Interrupted')),
  row_version               INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  runtime_slot              TEXT CHECK (runtime_slot IN ('plinth','lintel')), -- 派发绑定；一旦绑定不可改
  requested_by_tool_call_id INTEGER REFERENCES tool_calls(id) ON UPDATE RESTRICT ON DELETE RESTRICT, -- Plinth Browser Tool 请求的 Lintel 子 Attempt
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
    OR (state IN ('Running','Cancelling') AND runtime_slot IS NOT NULL AND boot_id IS NOT NULL AND connection_epoch IS NOT NULL AND lease_until IS NOT NULL AND accepted_at IS NOT NULL AND runtime_release_version IS NOT NULL)
    OR (state IN ('Succeeded','Failed','Cancelled','Interrupted') AND (
      (runtime_slot IS NULL AND boot_id IS NULL AND connection_epoch IS NULL AND lease_until IS NULL AND accepted_at IS NULL AND runtime_release_version IS NULL)
      OR (runtime_slot IS NOT NULL AND boot_id IS NOT NULL AND connection_epoch IS NOT NULL AND lease_until IS NOT NULL AND runtime_release_version IS NOT NULL)
    ))
  ),
  CHECK (connection_epoch IS NULL OR connection_epoch >= 1),
  CHECK (requested_by_tool_call_id IS NULL OR attempt_type = 'browser_exploration'),
  CHECK (
    (scope_type = 'config_verification_run' AND plan_key IS NOT NULL AND check_key IS NOT NULL AND discovery_key IS NULL)
    OR (scope_type = 'run_check' AND plan_key IS NULL AND check_key IS NOT NULL AND discovery_key IS NULL)
    OR (scope_type = 'resource_refresh_run' AND plan_key IS NULL AND check_key IS NULL AND discovery_key IS NOT NULL)
    OR (scope_type NOT IN ('run_check','config_verification_run','resource_refresh_run') AND plan_key IS NULL AND check_key IS NULL AND discovery_key IS NULL)
  ),
  CHECK (
    runtime_slot IS NULL
    OR (attempt_type = 'browser_exploration' AND runtime_slot = 'lintel')
    OR (attempt_type = 'inspection_collection' AND (
      (scope_type = 'run_check' AND runtime_slot = 'lintel')
      OR (scope_type IN ('config_verification_run','resource_refresh_run') AND runtime_slot = 'plinth')
    ))
    OR (attempt_type IN ('initial_analysis','investigation','inspection_analysis','knowledge_extraction','embedding','connection_probe') AND runtime_slot = 'plinth')
  ),
  CHECK (
    (attempt_type = 'initial_analysis' AND scope_type = 'analysis')
    OR (attempt_type = 'investigation' AND scope_type = 'investigation')
    OR (attempt_type = 'browser_exploration' AND scope_type = 'investigation')
    OR (attempt_type = 'inspection_analysis' AND scope_type = 'run')
    OR (attempt_type = 'knowledge_extraction' AND scope_type = 'knowledge_import_batch')
    OR (attempt_type = 'embedding' AND scope_type = 'embedding_generation')
    OR (attempt_type = 'connection_probe' AND scope_type = 'connection')
    OR (attempt_type = 'inspection_collection' AND scope_type IN ('run_check','config_verification_run','resource_refresh_run'))
  )
) STRICT;
CREATE UNIQUE INDEX ux_execution_attempt_active_scope ON execution_attempts (scope_type, scope_id)
  WHERE state IN ('Queued','Assigned','Running','Cancelling') AND check_key IS NULL
    AND scope_type <> 'resource_refresh_run' AND attempt_type <> 'browser_exploration';
CREATE UNIQUE INDEX ux_execution_attempt_active_run_check ON execution_attempts (scope_type, scope_id, check_key)
  WHERE scope_type = 'run_check' AND state IN ('Queued','Assigned','Running','Cancelling');
CREATE UNIQUE INDEX ux_execution_attempt_active_config_verification_check ON execution_attempts (scope_type, scope_id, plan_key, check_key)
  WHERE scope_type = 'config_verification_run' AND state IN ('Queued','Assigned','Running','Cancelling');
CREATE UNIQUE INDEX ux_execution_attempt_active_resource_refresh_discovery ON execution_attempts (scope_type, scope_id, discovery_key)
  WHERE scope_type = 'resource_refresh_run' AND state IN ('Queued','Assigned','Running','Cancelling');
CREATE UNIQUE INDEX ux_execution_attempt_browser_requestor ON execution_attempts (requested_by_tool_call_id)
  WHERE requested_by_tool_call_id IS NOT NULL;
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
-- 是持久权威。Kubernetes binding 可在 Tool Call 持久化事务中追加，连接轮换不改写旧 binding。
CREATE TABLE attempt_connection_grants (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  attempt_id                INTEGER NOT NULL REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  purpose                   TEXT NOT NULL CHECK (purpose IN ('chat_model','embedding','thanos_query','config_thanos_query','kubernetes_read','model_probe_chat','model_probe_embedding','thanos_probe','kubernetes_probe')),
  business_system_id        INTEGER REFERENCES business_systems(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  connection_id             INTEGER NOT NULL REFERENCES connections(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  connection_revision_id    INTEGER NOT NULL REFERENCES connection_revisions(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  credential_generation_id  INTEGER NOT NULL REFERENCES credential_generations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  qualified_probe_result_id INTEGER REFERENCES connection_probe_results(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_by_tool_call_id    INTEGER REFERENCES tool_calls(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  created_at                TEXT NOT NULL,
  CHECK ((purpose = 'kubernetes_read' AND business_system_id IS NOT NULL AND created_by_tool_call_id IS NOT NULL AND qualified_probe_result_id IS NULL)
      OR (purpose = 'thanos_query' AND business_system_id IS NULL AND created_by_tool_call_id IS NOT NULL AND qualified_probe_result_id IS NULL)
      OR (purpose = 'config_thanos_query' AND business_system_id IS NULL AND created_by_tool_call_id IS NULL AND qualified_probe_result_id IS NULL)
      OR (purpose IN ('chat_model','embedding') AND business_system_id IS NULL AND created_by_tool_call_id IS NULL AND qualified_probe_result_id IS NOT NULL)
      OR (purpose IN ('model_probe_chat','model_probe_embedding','thanos_probe','kubernetes_probe') AND business_system_id IS NULL AND created_by_tool_call_id IS NULL AND qualified_probe_result_id IS NULL))
) STRICT;
CREATE UNIQUE INDEX ux_attempt_connection_grant_binding ON attempt_connection_grants
  (attempt_id, purpose, connection_id, connection_revision_id, credential_generation_id, COALESCE(business_system_id, 0));

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
  execution_mode        TEXT NOT NULL CHECK (execution_mode IN ('worker_local','supervisor_typed','quoin_browser')),
  failure_mode          TEXT NOT NULL CHECK (failure_mode IN ('return_to_model','fail_attempt')),
  status                TEXT NOT NULL CHECK (status IN ('pending','running','succeeded','failed','cancelled')),
  row_version           INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  result_json           TEXT CHECK (result_json IS NULL OR json_valid(result_json)), -- 有界模型可见预览/结构化结果
  result_artifact_id    INTEGER REFERENCES artifacts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  error_detail          TEXT,
  created_at            TEXT NOT NULL,
  started_at            TEXT,
  ended_at              TEXT,
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

-- Lintel Journey ResultProposal 的不可变重放账本与单一提交入口。单 INSERT 先绑定 primary structured Evidence，
-- 再由 AFTER trigger 派生 check result 并原子收口 Browser Operation/Attempt；相同 operation 只能有一个 digest，
-- 因而 Ack 丢失可按 digest 重建，不同 payload 无法覆盖。operation-less identity_busy 结果直接由 check result
-- 行承载 digest，并由对应 AFTER trigger 原子收口未派发 Attempt。
CREATE TABLE browser_journey_results (
  operation_id       INTEGER PRIMARY KEY REFERENCES browser_operations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  attempt_id         INTEGER NOT NULL UNIQUE REFERENCES execution_attempts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  result_digest      BLOB NOT NULL CHECK (length(result_digest) = 32),
  outcome            TEXT NOT NULL CHECK (outcome IN ('success','gap')),
  primary_evidence_id INTEGER UNIQUE REFERENCES evidence(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  gap_code           TEXT CHECK (gap_code IS NULL OR gap_code IN (
                       'runtime_unavailable','authentication_required','authentication_probe_unavailable',
                       'artifact_commit_failed','journey_failed','cancelled','interrupted')),
  original_gap_code  TEXT CHECK (original_gap_code IS NULL OR original_gap_code = 'journey_failed'),
  terminal_reason    TEXT CHECK (terminal_reason IS NULL OR terminal_reason IN (
                       'new_boot','shutdown','slot_revoked','slot_replaced','authentication_required',
                       'authentication_probe_unavailable','artifact_commit_failed','journey_failed','cancelled',
                       'parent_terminal','lease_expired','runtime_unavailable','browser_crashed','protocol_error')),
  error_detail       TEXT,
  created_at         TEXT NOT NULL,
  CHECK (
    (outcome = 'success' AND primary_evidence_id IS NOT NULL AND gap_code IS NULL AND original_gap_code IS NULL AND terminal_reason IS NULL AND error_detail IS NULL)
    OR (outcome = 'gap' AND primary_evidence_id IS NULL AND gap_code IS NOT NULL AND terminal_reason IS NOT NULL AND error_detail IS NOT NULL
      AND ((gap_code = 'artifact_commit_failed' AND original_gap_code IS NOT NULL AND original_gap_code = 'journey_failed')
        OR (gap_code <> 'artifact_commit_failed' AND original_gap_code IS NULL)))
  ),
  CHECK (
    gap_code IS NULL
    OR (gap_code = 'authentication_required' AND terminal_reason = 'authentication_required')
    OR (gap_code = 'authentication_probe_unavailable' AND terminal_reason = 'authentication_probe_unavailable')
    OR (gap_code = 'artifact_commit_failed' AND terminal_reason = 'artifact_commit_failed')
    OR (gap_code = 'journey_failed' AND terminal_reason = 'journey_failed')
    OR (gap_code = 'runtime_unavailable' AND terminal_reason = 'runtime_unavailable')
    OR (gap_code = 'cancelled' AND terminal_reason = 'cancelled')
    OR (gap_code = 'interrupted' AND terminal_reason IN (
      'new_boot','shutdown','slot_revoked','slot_replaced','parent_terminal','lease_expired','browser_crashed','protocol_error'))
  )
) STRICT;

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
  kind           TEXT NOT NULL CHECK (kind IN ('attachment','screenshot','trace','tool_result','report_file','verification_bundle','verification_attachment')),
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
  kind           TEXT NOT NULL CHECK (kind IN ('attachment','screenshot','trace','tool_result','report_file')),
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
  CHECK (kind <> 'trace' OR sensitive = 1),
  CHECK (attempt_id IS NOT NULL OR (kind = 'trace' AND owner_type = 'browser_operation'))
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
  retention_count INTEGER NOT NULL DEFAULT 30 CHECK (retention_count >= 1),
  row_version     INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1), -- 设置更新并发前提（DATA-BACKUP-008）
  updated_by      INTEGER REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  updated_at      TEXT NOT NULL
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

CREATE TABLE backups (
  id              INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  status          TEXT NOT NULL CHECK (status IN ('queued','running','succeeded','failed')),
  stage           TEXT NOT NULL CHECK (stage IN ('queued','preflight','database_snapshot','artifact_copy','manifest_publish','completed')),
  trigger_kind    TEXT NOT NULL CHECK (trigger_kind IN ('manual','scheduled','upgrade')),
  execution_mode  TEXT NOT NULL CHECK (execution_mode IN ('online','offline')),
  scheduled_for   TEXT,  -- UTC 计划边界；仅 scheduled 非空，用于停机 catch-up 幂等
  db_sha256       TEXT CHECK (db_sha256 IS NULL OR length(db_sha256) = 64),
  manifest_sha256 TEXT CHECK (manifest_sha256 IS NULL OR length(manifest_sha256) = 64),
  artifact_count  INTEGER CHECK (artifact_count IS NULL OR artifact_count >= 0),
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
      AND db_sha256 IS NULL AND manifest_sha256 IS NULL AND artifact_count IS NULL AND manifest_path IS NULL
      AND error_code IS NULL AND retryable IS NULL AND error_detail IS NULL)
    OR
    (status = 'running' AND stage IN ('preflight','database_snapshot','artifact_copy','manifest_publish')
      AND started_at IS NOT NULL AND completed_at IS NULL
      AND db_sha256 IS NULL AND manifest_sha256 IS NULL AND artifact_count IS NULL AND manifest_path IS NULL
      AND error_code IS NULL AND retryable IS NULL AND error_detail IS NULL)
    OR
    (status = 'succeeded' AND stage = 'completed' AND started_at IS NOT NULL AND completed_at IS NOT NULL
      AND db_sha256 IS NOT NULL AND manifest_sha256 IS NOT NULL AND artifact_count IS NOT NULL AND manifest_path IS NOT NULL
      AND error_code IS NULL AND retryable IS NULL AND error_detail IS NULL)
    OR
    (status = 'failed' AND stage <> 'completed' AND completed_at IS NOT NULL AND db_sha256 IS NULL AND manifest_sha256 IS NULL
      AND artifact_count IS NULL AND manifest_path IS NULL AND error_code IS NOT NULL AND retryable IS NOT NULL AND error_detail IS NOT NULL)
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

-- 恢复、协调升级、根密钥 rebind 与 Lintel offline recovery 共用的维护聚合；
-- LintelRecovery 只能由 deployment helper 进入并以 helper-only finalize 退出。
CREATE TABLE maintenance_state (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  active          INTEGER NOT NULL CHECK (active IN (0,1)),
  reason          TEXT CHECK (reason IS NULL OR reason IN ('Restore','Upgrade','RootKeyRebind','LintelRecovery')),
  row_version     INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
  entered_at      TEXT,
  entered_by_type TEXT CHECK (entered_by_type IS NULL OR entered_by_type IN ('user','system','deployment_helper')),
  entered_by_id   INTEGER,
  exited_at       TEXT,
  exited_by_type  TEXT CHECK (exited_by_type IS NULL OR exited_by_type IN ('user','system','deployment_helper')),
  exited_by_id    INTEGER,
  CHECK ((active = 1 AND reason IS NOT NULL AND entered_at IS NOT NULL AND entered_by_type IS NOT NULL AND exited_at IS NULL AND exited_by_type IS NULL AND exited_by_id IS NULL)
      OR (active = 0 AND reason IS NULL AND entered_at IS NULL AND entered_by_type IS NULL AND entered_by_id IS NULL))
) STRICT;

CREATE TABLE maintenance_items (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  maintenance_revision INTEGER NOT NULL CHECK (maintenance_revision >= 1),
  kind                 TEXT NOT NULL CHECK (kind IN ('AdminPassword','User','Connection','RuntimeSlot','AlertSource','BrowserIdentity','ActiveAttempt','ActiveBrowserOperation','BackupPreflight','SchemaMigration','ReleaseVersion','Integrity','SearchProjection','LintelRecoveryFence')),
  object_key           TEXT NOT NULL CHECK (length(object_key) BETWEEN 1 AND 256),
  safe_state           TEXT NOT NULL CHECK (safe_state IN ('Safe','Blocking')),
  detail_code          TEXT NOT NULL CHECK (length(detail_code) BETWEEN 1 AND 128),
  updated_at           TEXT NOT NULL,
  UNIQUE (maintenance_revision, kind, object_key)
) STRICT;
CREATE INDEX idx_maintenance_items_state ON maintenance_items (maintenance_revision, safe_state, kind);

CREATE TABLE lintel_recovery_receipts (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  maintenance_revision       INTEGER NOT NULL UNIQUE CHECK (maintenance_revision >= 1),
  old_slot_id                TEXT NOT NULL CHECK (old_slot_id = 'lintel'),
  old_runtime_credential_id  INTEGER NOT NULL REFERENCES runtime_credentials(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  old_token_generation       INTEGER NOT NULL CHECK (old_token_generation >= 1),
  replacement_runtime_credential_id INTEGER NOT NULL REFERENCES runtime_credentials(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  replacement_token_generation INTEGER NOT NULL CHECK (replacement_token_generation >= 1),
  storage_disposition        TEXT NOT NULL CHECK (storage_disposition IN ('exclusively_reattached','retired')),
  disposition_digest         TEXT NOT NULL CHECK (length(disposition_digest) = 64 AND disposition_digest NOT GLOB '*[^0-9a-f]*'),
  fence_report_digest        TEXT NOT NULL CHECK (length(fence_report_digest) = 64 AND fence_report_digest NOT GLOB '*[^0-9a-f]*'),
  recovery_report_digest     TEXT NOT NULL CHECK (length(recovery_report_digest) = 64 AND recovery_report_digest NOT GLOB '*[^0-9a-f]*'),
  post_verify_digest         TEXT NOT NULL CHECK (length(post_verify_digest) = 64 AND post_verify_digest NOT GLOB '*[^0-9a-f]*'),
  created_at                 TEXT NOT NULL,
  CHECK (old_runtime_credential_id <> replacement_runtime_credential_id),
  CHECK (old_token_generation < replacement_token_generation),
  UNIQUE (old_slot_id, old_token_generation, disposition_digest)
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
CREATE TRIGGER trg_attempt_connection_grants_config_thanos_closure BEFORE INSERT ON attempt_connection_grants
WHEN NEW.purpose = 'config_thanos_query' AND NOT EXISTS (
  SELECT 1 FROM execution_attempts a
  WHERE a.id = NEW.attempt_id
    AND a.attempt_type = 'inspection_collection'
    AND a.scope_type IN ('config_verification_run','resource_refresh_run')
    AND a.state = 'Queued'
)
BEGIN SELECT RAISE(ABORT, 'config_thanos_query grant requires one Queued Config Verification or Resource Refresh inspection_collection Attempt'); END;
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
        AND ((a.runtime_slot = 'lintel' AND a.attempt_type IN ('inspection_collection','browser_exploration'))
          OR (a.runtime_slot = 'plinth' AND a.attempt_type = 'inspection_collection' AND a.scope_type = 'config_verification_run'))
    ))
BEGIN SELECT RAISE(ABORT, 'Evidence must be Quoin-local, close to one same Running Attempt and running Tool Call, or close to one accepted Runtime collection/exploration Attempt'); END;
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
CREATE TRIGGER trg_kubernetes_connection_probe_results_no_update BEFORE UPDATE ON kubernetes_connection_probe_results
BEGIN SELECT RAISE(ABORT, 'typed connection probe results are append-only'); END;
CREATE TRIGGER trg_kubernetes_connection_probe_results_no_delete BEFORE DELETE ON kubernetes_connection_probe_results
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
CREATE TRIGGER trg_browser_identity_revisions_no_update BEFORE UPDATE ON browser_identity_revisions
BEGIN SELECT RAISE(ABORT, 'browser_identity_revisions is append-only'); END;
CREATE TRIGGER trg_browser_identity_revisions_no_delete BEFORE DELETE ON browser_identity_revisions
BEGIN SELECT RAISE(ABORT, 'browser_identity_revisions is retained history'); END;
CREATE TRIGGER trg_browser_profile_generations_no_update BEFORE UPDATE ON browser_profile_generations
BEGIN SELECT RAISE(ABORT, 'browser_profile_generations is append-only'); END;
CREATE TRIGGER trg_browser_profile_generations_no_delete BEFORE DELETE ON browser_profile_generations
BEGIN SELECT RAISE(ABORT, 'browser_profile_generations is append-only'); END;
CREATE TRIGGER trg_browser_probe_results_no_update BEFORE UPDATE ON browser_probe_results
BEGIN SELECT RAISE(ABORT, 'browser_probe_results is append-only'); END;
CREATE TRIGGER trg_browser_probe_results_no_delete BEFORE DELETE ON browser_probe_results
BEGIN SELECT RAISE(ABORT, 'browser_probe_results is retained history'); END;
CREATE TRIGGER trg_browser_profile_reconciliations_no_update BEFORE UPDATE ON browser_profile_reconciliations
BEGIN SELECT RAISE(ABORT, 'browser_profile_reconciliations is append-only'); END;
CREATE TRIGGER trg_browser_profile_reconciliations_no_delete BEFORE DELETE ON browser_profile_reconciliations
BEGIN SELECT RAISE(ABORT, 'browser_profile_reconciliations is retained history'); END;
CREATE TRIGGER trg_browser_exploration_actions_no_delete BEFORE DELETE ON browser_exploration_actions
BEGIN SELECT RAISE(ABORT, 'browser_exploration_actions is retained history'); END;
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
  label_contract_version_id, journey_catalog_digest, journey_catalog_version,
  system_key, display_name, enabled, timezone, resource_refresh_interval_seconds,
  digest, created_by, created_at ON business_system_config_versions
BEGIN SELECT RAISE(ABORT, 'business_system_config_version content is immutable'); END;

-- 12.8 Label Contract：只允许 state/activation 变化，正文与类型化投影不可变
CREATE TRIGGER trg_label_contracts_no_content_update BEFORE UPDATE OF
  version, yaml_body, contract_json, digest, parser_version, schema_version, created_at ON label_contracts
BEGIN SELECT RAISE(ABORT, 'label_contract content is immutable'); END;

-- 12.9 连接：name/type 不可变；Business System ↔ Kubernetes binding 只允许 Active -> Retired。
CREATE TRIGGER trg_connections_no_identity_update BEFORE UPDATE OF name, type, created_at ON connections
BEGIN SELECT RAISE(ABORT, 'connection identity is immutable'); END;
CREATE TRIGGER trg_business_system_kubernetes_connection_origin_immutable BEFORE UPDATE OF
  business_system_id, connection_id, created_by, created_at ON business_system_kubernetes_connections
BEGIN SELECT RAISE(ABORT, 'business_system kubernetes connection origin is immutable'); END;
CREATE TRIGGER trg_business_system_kubernetes_connection_retire_only BEFORE UPDATE ON business_system_kubernetes_connections
WHEN NOT (OLD.state = 'Active' AND NEW.state = 'Retired'
          AND NEW.row_version = OLD.row_version + 1
          AND OLD.retired_by IS NULL AND NEW.retired_by IS NOT NULL
          AND OLD.retired_at IS NULL AND NEW.retired_at IS NOT NULL)
BEGIN SELECT RAISE(ABORT, 'business_system kubernetes connection update must be Active -> Retired'); END;
CREATE TRIGGER trg_business_system_kubernetes_connections_no_delete BEFORE DELETE ON business_system_kubernetes_connections
BEGIN SELECT RAISE(ABORT, 'business_system kubernetes connection history is not deletable'); END;

-- 12.10 浏览器身份：业务系统绑定不可变
CREATE TRIGGER trg_browser_identities_no_system_update BEFORE UPDATE OF business_system_id, created_at ON browser_identities
BEGIN SELECT RAISE(ABORT, 'browser_identity system binding is immutable'); END;

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
  OR (NEW.kind IN ('screenshot','trace') AND NEW.owner_type = 'browser_operation' AND EXISTS (SELECT 1 FROM browser_operations b WHERE b.id = NEW.owner_id))
  OR (NEW.kind = 'report_file' AND NEW.owner_type = 'inspection_report' AND EXISTS (SELECT 1 FROM inspection_reports r WHERE r.id = NEW.owner_id))
  OR (NEW.kind = 'report_file' AND NEW.owner_type = 'backup' AND EXISTS (SELECT 1 FROM backups b WHERE b.id = NEW.owner_id))
  OR (NEW.kind = 'attachment' AND NEW.owner_type = 'source_material' AND EXISTS (SELECT 1 FROM source_materials s WHERE s.id = NEW.owner_id))
  OR (NEW.kind = 'verification_bundle' AND NEW.owner_type = 'verification_invocation' AND NEW.retention_kind = 'long_term'
    AND EXISTS (SELECT 1 FROM verification_invocation_manifests v WHERE v.id = NEW.owner_id))
  OR (NEW.kind = 'verification_attachment' AND NEW.owner_type = 'verification_invocation'
    AND EXISTS (SELECT 1 FROM verification_invocation_manifests v WHERE v.id = NEW.owner_id))
)
BEGIN SELECT RAISE(ABORT, 'artifact kind/owner_type/owner_id must reference an existing compatible authority row'); END;

-- 12.13 Browser Identity/Operation/Exploration：revision、generation、probe/reconcile 事实不可变；
-- Operation 只沿封闭状态机推进，profile 发布由 generation INSERT 原子切换 identity/operation。
CREATE TRIGGER trg_browser_operations_no_origin_update BEFORE UPDATE OF
  identity_id, identity_revision_id, profile_generation_id, owner_attempt_id, kind, actor_user_id,
  journey_catalog_digest, journey_catalog_version, journey_id, journey_version, probe_phase, requested_at ON browser_operations
BEGIN SELECT RAISE(ABORT, 'browser_operation origin is immutable'); END;
CREATE TRIGGER trg_browser_operations_completion_digest_once BEFORE UPDATE OF completion_digest ON browser_operations
WHEN OLD.completion_digest IS NOT NULL AND NEW.completion_digest IS NOT OLD.completion_digest
BEGIN SELECT RAISE(ABORT, 'browser operation completion digest is immutable once committed'); END;
CREATE TRIGGER trg_browser_operations_insert_closure BEFORE INSERT ON browser_operations
WHEN NEW.state <> 'Queued' OR NOT EXISTS (
  SELECT 1 FROM browser_identities i JOIN browser_identity_revisions r ON r.id = NEW.identity_revision_id
  WHERE i.id = NEW.identity_id AND r.business_system_id = i.business_system_id
    AND ((NEW.kind = 'authentication_probe' AND NEW.probe_phase = 'revision_change') OR i.current_revision_id = NEW.identity_revision_id)
) OR (NEW.profile_generation_id IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM browser_profile_generations g
  WHERE g.id = NEW.profile_generation_id AND g.identity_id = NEW.identity_id
)) OR (NEW.kind = 'authentication_probe' AND NOT EXISTS (
  SELECT 1 FROM browser_identity_revisions r WHERE r.id = NEW.identity_revision_id
    AND r.probe_journey_id = NEW.journey_id AND NEW.journey_version >= r.probe_journey_version
  )) OR (NEW.kind = 'journey' AND NOT EXISTS (
  SELECT 1 FROM execution_attempts a JOIN browser_identities i ON i.id = NEW.identity_id
  WHERE a.id = NEW.owner_attempt_id AND a.attempt_type = 'inspection_collection'
    AND ((a.state = 'Queued' AND a.runtime_slot IS NULL)
      OR (a.state IN ('Assigned','Running') AND a.runtime_slot = 'lintel'))
    AND a.scope_type IN ('run_check','config_verification_run') AND a.check_key IS NOT NULL
    AND ((a.scope_type = 'run_check' AND i.business_system_id = (SELECT business_system_id FROM inspection_runs WHERE id = a.scope_id))
      OR (a.scope_type = 'config_verification_run' AND i.business_system_id = (SELECT business_system_id FROM config_verification_runs WHERE id = a.scope_id)))
  )) OR (NEW.kind = 'exploration' AND NOT EXISTS (
  SELECT 1 FROM execution_attempts a WHERE a.id = NEW.owner_attempt_id
    AND a.attempt_type = 'investigation' AND a.runtime_slot = 'plinth'
    AND a.scope_type = 'investigation' AND a.check_key IS NULL AND a.state = 'Running'
  ))
BEGIN SELECT RAISE(ABORT, 'browser_operation must start Queued with an owned revision/profile and a compatible owner Attempt/Journey binding'); END;
CREATE TRIGGER trg_browser_operations_global_fifo BEFORE UPDATE OF state ON browser_operations
WHEN NEW.state = 'Starting' AND OLD.state IN ('Queued','WaitingForCapacity') AND EXISTS (
  SELECT 1 FROM browser_operations q
  WHERE q.id < NEW.id AND q.state IN ('Queued','WaitingForCapacity')
)
BEGIN SELECT RAISE(ABORT, 'browser operation may dispatch Start only at the global FIFO head'); END;
CREATE TRIGGER trg_execution_attempts_browser_dispatch_after_operation_running BEFORE UPDATE OF state ON execution_attempts
WHEN NEW.state = 'Assigned' AND OLD.state = 'Queued' AND (
  (NEW.attempt_type = 'inspection_collection' AND NEW.scope_type = 'run_check'
    AND NOT EXISTS (SELECT 1 FROM browser_operations o WHERE o.owner_attempt_id = NEW.id AND o.kind = 'journey' AND o.state = 'Running'))
  OR (NEW.attempt_type = 'browser_exploration'
    AND NOT EXISTS (
      SELECT 1 FROM tool_calls t JOIN browser_operations o ON o.owner_attempt_id = t.attempt_id
      WHERE t.id = NEW.requested_by_tool_call_id AND o.kind = 'exploration' AND o.state = 'Running'))
)
BEGIN SELECT RAISE(ABORT, 'browser Attempt may dispatch only after its Browser Operation owns a physical slot'); END;
CREATE TRIGGER trg_browser_journey_results_closure BEFORE INSERT ON browser_journey_results
WHEN NOT EXISTS (
  SELECT 1
  FROM browser_operations o
  JOIN execution_attempts a ON a.id = NEW.attempt_id AND a.id = o.owner_attempt_id
  WHERE o.id = NEW.operation_id AND o.kind = 'journey' AND o.state = 'Running'
    AND a.attempt_type = 'inspection_collection' AND a.state = 'Running'
    AND a.runtime_slot = 'lintel' AND a.accepted_at IS NOT NULL
    AND (
      (NEW.outcome = 'success' AND EXISTS (
        SELECT 1 FROM evidence e
        WHERE e.id = NEW.primary_evidence_id AND e.attempt_id = a.id
          AND e.integrity = 'complete' AND e.result_json IS NOT NULL AND e.artifact_id IS NULL
          AND ((a.scope_type = 'run_check' AND e.target_type = 'inspection_run' AND e.target_id = a.scope_id
                AND json_type(e.params_json, '$.check_key') = 'text' AND (e.params_json ->> '$.check_key') = a.check_key)
            OR (a.scope_type = 'config_verification_run' AND e.target_type = 'config_verification_run' AND e.target_id = a.scope_id
                AND json_type(e.params_json, '$.plan_key') = 'text' AND (e.params_json ->> '$.plan_key') = a.plan_key
                AND json_type(e.params_json, '$.check_key') = 'text' AND (e.params_json ->> '$.check_key') = a.check_key))))
      OR (NEW.outcome = 'gap' AND NEW.primary_evidence_id IS NULL)
    )
)
BEGIN SELECT RAISE(ABORT, 'Journey ResultProposal must bind one Running journey operation/Attempt; success additionally requires its committed primary structured Evidence'); END;
CREATE TRIGGER trg_browser_journey_results_commit AFTER INSERT ON browser_journey_results
BEGIN
  INSERT INTO inspection_check_results
    (run_id,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,created_at)
  SELECT a.scope_id,a.check_key,
         CASE WHEN NEW.outcome = 'success' THEN 'ok' ELSE 'gap' END,
         NEW.primary_evidence_id,a.id,NEW.result_digest,NEW.gap_code,NEW.created_at
  FROM execution_attempts a WHERE a.id = NEW.attempt_id AND a.scope_type = 'run_check';
  INSERT INTO config_verification_run_check_results
    (verification_run_id,plan_key,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,created_at)
  SELECT a.scope_id,a.plan_key,a.check_key,
         CASE WHEN NEW.outcome = 'success' THEN 'ok' ELSE 'gap' END,
         NEW.primary_evidence_id,a.id,NEW.result_digest,NEW.gap_code,NEW.created_at
  FROM execution_attempts a WHERE a.id = NEW.attempt_id AND a.scope_type = 'config_verification_run';
  UPDATE browser_operations
  SET state = CASE WHEN NEW.outcome = 'success' THEN 'Succeeded' ELSE 'Failed' END,
      terminal_reason = NEW.terminal_reason,
      ended_at = NEW.created_at,
      row_version = row_version + 1
  WHERE id = NEW.operation_id AND state = 'Running';
  UPDATE execution_attempts
  SET state = 'Succeeded', ended_at = NEW.created_at, row_version = row_version + 1
  WHERE id = NEW.attempt_id AND state = 'Running';
END;
CREATE TRIGGER trg_browser_journey_results_no_update BEFORE UPDATE ON browser_journey_results
BEGIN SELECT RAISE(ABORT, 'browser Journey results are immutable'); END;
CREATE TRIGGER trg_browser_journey_results_no_delete BEFORE DELETE ON browser_journey_results
BEGIN SELECT RAISE(ABORT, 'browser Journey results are retained as replay lineage'); END;

CREATE TRIGGER trg_browser_operations_success_probe_fence BEFORE UPDATE OF state ON browser_operations
WHEN NEW.state = 'Succeeded' AND (
  (NEW.kind = 'authentication_probe' AND NOT EXISTS (
    SELECT 1 FROM browser_probe_results p WHERE p.operation_id = NEW.id
      AND p.phase = NEW.probe_phase AND p.result IN ('Authenticated','Unauthenticated')))
  OR (NEW.kind IN ('journey','exploration') AND (
    NOT EXISTS (SELECT 1 FROM browser_probe_results p WHERE p.operation_id = NEW.id
      AND p.phase = 'admission' AND p.result = 'Authenticated')
    OR NOT EXISTS (SELECT 1 FROM browser_probe_results p WHERE p.operation_id = NEW.id
      AND p.phase = 'completion' AND p.result = 'Authenticated')
    OR NOT EXISTS (SELECT 1 FROM browser_identities i WHERE i.id = NEW.identity_id AND i.state = 'Ready')
    OR (NEW.kind = 'journey' AND NOT EXISTS (
      SELECT 1 FROM execution_attempts a JOIN evidence e ON e.attempt_id = a.id
      WHERE a.id = NEW.owner_attempt_id AND (
        (a.scope_type = 'run_check' AND EXISTS (
          SELECT 1 FROM inspection_check_results r
          WHERE r.run_id = a.scope_id AND r.check_key = a.check_key AND r.status = 'ok' AND r.evidence_id = e.id))
        OR (a.scope_type = 'config_verification_run' AND EXISTS (
          SELECT 1 FROM config_verification_run_check_results r
          WHERE r.verification_run_id = a.scope_id AND r.plan_key = a.plan_key AND r.check_key = a.check_key
            AND r.status = 'ok' AND r.evidence_id = e.id AND r.attempt_id = a.id))
      )))
    OR (NEW.kind = 'exploration' AND NOT (NEW.trace_artifact_id IS NOT NULL AND NEW.trace_integrity = 'complete'))
  ))
  OR (NEW.kind = 'manual_login' AND NOT EXISTS (
    SELECT 1 FROM browser_profile_generations g WHERE g.published_operation_id = NEW.id))
)
BEGIN SELECT RAISE(ABORT, 'successful browser operation requires its authoritative probe/profile result'); END;
CREATE TRIGGER trg_browser_operations_failed_journey_trace BEFORE UPDATE OF state ON browser_operations
WHEN NEW.state = 'Failed' AND NEW.kind = 'journey' AND NEW.started_at IS NOT NULL
  AND NEW.terminal_reason NOT IN ('artifact_commit_failed','new_boot','runtime_unavailable')
  AND NOT (NEW.trace_artifact_id IS NOT NULL AND NEW.trace_integrity IN ('complete','incomplete'))
BEGIN SELECT RAISE(ABORT, 'failed journey requires its diagnostic trace unless trace commit itself failed'); END;
CREATE TRIGGER trg_browser_operations_exploration_terminal_trace BEFORE UPDATE OF state ON browser_operations
WHEN NEW.kind = 'exploration' AND NEW.started_at IS NOT NULL
  AND NEW.state IN ('Succeeded','Failed','Cancelled','Interrupted')
  AND (NEW.terminal_reason IS NULL OR NEW.terminal_reason NOT IN ('artifact_commit_failed','new_boot','runtime_unavailable'))
  AND NOT (NEW.trace_artifact_id IS NOT NULL AND NEW.trace_integrity IN ('complete','incomplete'))
BEGIN SELECT RAISE(ABORT, 'started exploration requires its continuous trace unless commit failed or process loss made the trace unavailable'); END;
CREATE TRIGGER trg_browser_operations_trace_owner BEFORE UPDATE OF trace_artifact_id ON browser_operations
WHEN NEW.trace_artifact_id IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM artifacts a WHERE a.id = NEW.trace_artifact_id AND a.kind = 'trace'
    AND a.owner_type = 'browser_operation' AND a.owner_id = NEW.id AND a.sensitive = 1 AND a.body_expired = 0)
BEGIN SELECT RAISE(ABORT, 'browser operation trace must be an available sensitive trace artifact owned by the operation'); END;
CREATE TRIGGER trg_browser_operation_manual_login_marks_auth_required AFTER INSERT ON browser_operations
WHEN NEW.kind = 'manual_login'
BEGIN
  UPDATE browser_identities SET state = 'AuthenticationRequired', row_version = row_version + 1
  WHERE id = NEW.identity_id AND state <> 'AuthenticationRequired';
END;
CREATE TRIGGER trg_browser_probe_results_closure BEFORE INSERT ON browser_probe_results
WHEN NOT EXISTS (
  SELECT 1 FROM browser_operations o JOIN browser_identity_revisions r ON r.id = o.identity_revision_id
  WHERE o.id = NEW.operation_id
    AND o.identity_revision_id = NEW.identity_revision_id
    AND r.probe_journey_id = NEW.journey_id AND NEW.journey_version >= r.probe_journey_version
    AND o.journey_catalog_digest = NEW.journey_catalog_digest
    AND o.journey_catalog_version = NEW.journey_catalog_version
    AND (o.state = 'Running' OR (o.kind = 'manual_login' AND o.state = 'AwaitingReconnect'))
    AND (o.kind <> 'authentication_probe' OR (o.journey_id = NEW.journey_id AND o.journey_version = NEW.journey_version AND o.probe_phase = NEW.phase))
    AND (NEW.phase <> 'publish' OR o.kind = 'manual_login')
    AND (NEW.phase <> 'revision_change' OR o.kind = 'authentication_probe')
)
BEGIN SELECT RAISE(ABORT, 'probe result must match its operation revision, catalog, Journey and phase'); END;
CREATE TRIGGER trg_browser_probe_unauthenticated_marks_auth_required AFTER INSERT ON browser_probe_results
WHEN NEW.result = 'Unauthenticated'
BEGIN
  UPDATE browser_identities SET state = 'AuthenticationRequired', row_version = row_version + 1
  WHERE id = (SELECT identity_id FROM browser_operations WHERE id = NEW.operation_id)
    AND state <> 'AuthenticationRequired';
END;
-- Browser Identity 配置切换与 profile 指针切换都不能绕过仍持有物理进程 fence 的 operation。
CREATE TRIGGER trg_browser_identities_revision_switch_busy BEFORE UPDATE OF current_revision_id ON browser_identities
WHEN NEW.current_revision_id IS NOT OLD.current_revision_id AND EXISTS (
  SELECT 1 FROM browser_operations o
  WHERE o.identity_id = OLD.id AND o.stop_confirmed_at IS NULL
    AND NOT (o.kind = 'authentication_probe' AND o.probe_phase = 'revision_change'
      AND o.identity_revision_id = NEW.current_revision_id AND o.state IN ('Queued','WaitingForCapacity','Starting','Running')))
BEGIN SELECT RAISE(ABORT, 'identity revision cannot switch while an unrelated active operation holds the identity'); END;
CREATE TRIGGER trg_browser_identities_profile_pointer_requires_no_active_operation BEFORE UPDATE OF current_profile_generation_id ON browser_identities
WHEN NEW.current_profile_generation_id IS NOT OLD.current_profile_generation_id AND EXISTS (
  SELECT 1 FROM browser_operations o
  WHERE o.identity_id = OLD.id AND o.stop_confirmed_at IS NULL AND o.kind <> 'manual_login')
BEGIN SELECT RAISE(ABORT, 'profile pointer cannot switch while a non-publication operation holds the identity'); END;
CREATE TRIGGER trg_browser_profile_reconciliations_closure BEFORE INSERT ON browser_profile_reconciliations
WHEN NOT EXISTS (
  SELECT 1 FROM browser_identities i JOIN browser_profile_generations g ON g.id = NEW.profile_generation_id
  WHERE i.id = NEW.identity_id AND g.identity_id = i.id AND i.current_profile_generation_id = g.id
    AND (
      (NEW.result = 'compatible' AND NEW.observed_chromium_revision = g.chromium_revision
        AND NEW.observed_manifest_digest = g.profile_manifest_digest)
      OR (NEW.result = 'missing')
      OR (NEW.result = 'manifest_invalid'
        AND (NEW.observed_manifest_digest IS NULL OR NEW.observed_manifest_digest <> g.profile_manifest_digest))
      OR (NEW.result = 'chromium_revision_mismatch'
        AND NEW.observed_chromium_revision <> g.chromium_revision
        AND NEW.observed_manifest_digest = g.profile_manifest_digest)
    )
)
BEGIN SELECT RAISE(ABORT, 'profile reconciliation must classify the identity current generation by exact Chromium revision and manifest digest'); END;
CREATE TRIGGER trg_browser_reconcile_incompatible_marks_auth_required AFTER INSERT ON browser_profile_reconciliations
WHEN NEW.result IN ('missing','manifest_invalid','chromium_revision_mismatch')
BEGIN
  UPDATE browser_identities SET state = 'AuthenticationRequired', row_version = row_version + 1
  WHERE id = NEW.identity_id AND state <> 'AuthenticationRequired';
END;
CREATE TRIGGER trg_browser_profile_generation_sequence BEFORE INSERT ON browser_profile_generations
WHEN NEW.generation <> COALESCE((SELECT MAX(generation) + 1 FROM browser_profile_generations WHERE identity_id = NEW.identity_id), 1)
BEGIN SELECT RAISE(ABORT, 'profile generation must be the next monotonic generation for its identity'); END;
CREATE TRIGGER trg_browser_profile_generation_publish_guard BEFORE INSERT ON browser_profile_generations
WHEN NOT EXISTS (
  SELECT 1 FROM browser_operations o JOIN browser_identities i ON i.id = o.identity_id
  WHERE o.id = NEW.published_operation_id AND o.kind = 'manual_login'
    AND o.state IN ('Running','AwaitingReconnect') AND o.identity_id = NEW.identity_id
    AND o.identity_revision_id = NEW.identity_revision_id AND i.current_revision_id = NEW.identity_revision_id
    AND o.actor_user_id = NEW.published_by
) OR NOT EXISTS (
  SELECT 1 FROM browser_probe_results p WHERE p.operation_id = NEW.published_operation_id
    AND p.phase = 'publish' AND p.result = 'Authenticated'
    AND p.identity_revision_id = NEW.identity_revision_id
    AND p.journey_id = NEW.probe_journey_id AND p.journey_version = NEW.probe_journey_version
    AND p.journey_catalog_digest = NEW.probe_catalog_digest
    AND p.journey_catalog_version = NEW.probe_catalog_version
)
BEGIN SELECT RAISE(ABORT, 'profile generation requires the active actor-owned manual login and a matching authenticated publish probe'); END;
CREATE TRIGGER trg_browser_profile_generation_publish_atomic AFTER INSERT ON browser_profile_generations
BEGIN
  UPDATE browser_identities
  SET current_profile_generation_id = NEW.id, state = 'Ready', row_version = row_version + 1
  WHERE id = NEW.identity_id AND current_revision_id = NEW.identity_revision_id;
  UPDATE browser_operations
  SET state = 'Succeeded', reconnect_deadline = NULL, ended_at = NEW.published_at, row_version = row_version + 1
  WHERE id = NEW.published_operation_id AND state IN ('Running','AwaitingReconnect');
END;
CREATE TRIGGER trg_browser_exploration_actions_no_origin_update BEFORE UPDATE OF
  operation_id, action_seq, child_attempt_id, tool_call_id, action_kind, page_id, origin, target_description, started_at ON browser_exploration_actions
BEGIN SELECT RAISE(ABORT, 'browser exploration action origin is immutable'); END;
CREATE TRIGGER trg_browser_exploration_actions_closure BEFORE INSERT ON browser_exploration_actions
WHEN NEW.action_seq <> COALESCE((SELECT MAX(action_seq) + 1 FROM browser_exploration_actions WHERE operation_id = NEW.operation_id), 1)
  OR (NEW.action_seq = 1 AND NEW.action_kind <> 'open')
  OR (NEW.action_seq > 1 AND NEW.action_kind = 'open')
  OR (NEW.action_seq > 1 AND NOT EXISTS (
    SELECT 1 FROM browser_probe_results p
    WHERE p.operation_id = NEW.operation_id AND p.phase = 'admission' AND p.result = 'Authenticated'))
  OR NOT EXISTS (
  SELECT 1 FROM browser_operations o
  JOIN execution_attempts parent ON parent.id = o.owner_attempt_id
  JOIN tool_calls t ON t.id = NEW.tool_call_id AND t.attempt_id = parent.id
  JOIN execution_attempts child ON child.id = NEW.child_attempt_id
    AND child.attempt_type = 'browser_exploration' AND child.state = 'Running'
    AND child.scope_type = 'investigation' AND child.scope_id = parent.scope_id
    AND child.requested_by_tool_call_id = t.id
  WHERE o.id = NEW.operation_id AND o.kind = 'exploration' AND o.state = 'Running'
    AND parent.attempt_type = 'investigation' AND parent.state = 'Running'
    AND t.execution_mode = 'quoin_browser' AND t.status = 'running'
    AND json_extract(t.arguments_json, '$.action') = NEW.action_kind
)
BEGIN SELECT RAISE(ABORT, 'browser exploration action must bind one parent Tool Call to its matching child Attempt in the same exploration operation'); END;
CREATE TRIGGER trg_browser_exploration_first_action_success_probe BEFORE UPDATE OF outcome ON browser_exploration_actions
WHEN NEW.action_seq = 1 AND NEW.action_kind = 'open' AND NEW.outcome = 'success'
  AND NOT EXISTS (
    SELECT 1 FROM browser_probe_results p
    WHERE p.operation_id = NEW.operation_id AND p.phase = 'admission' AND p.result = 'Authenticated')
BEGIN SELECT RAISE(ABORT, 'successful exploration open requires an authenticated admission probe'); END;
CREATE TRIGGER trg_browser_exploration_actions_screenshot_owner_insert BEFORE INSERT ON browser_exploration_actions
WHEN NEW.screenshot_artifact_id IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM artifacts a WHERE a.id = NEW.screenshot_artifact_id AND a.kind = 'screenshot'
    AND a.owner_type = 'browser_operation' AND a.owner_id = NEW.operation_id AND a.body_expired = 0)
BEGIN SELECT RAISE(ABORT, 'browser action screenshot must be an available screenshot artifact owned by its operation'); END;
CREATE TRIGGER trg_browser_exploration_actions_screenshot_owner BEFORE UPDATE OF screenshot_artifact_id ON browser_exploration_actions
WHEN NEW.screenshot_artifact_id IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM artifacts a JOIN browser_operations o ON o.id = NEW.operation_id
  WHERE a.id = NEW.screenshot_artifact_id AND a.kind = 'screenshot'
    AND a.owner_type = 'browser_operation' AND a.owner_id = o.id AND a.body_expired = 0)
BEGIN SELECT RAISE(ABORT, 'browser action screenshot must be an available screenshot artifact owned by its operation'); END;
CREATE TRIGGER trg_browser_exploration_actions_result_immutable BEFORE UPDATE OF
  ended_at, outcome, error_code, observation_version, observation_digest, observation_size_bytes, screenshot_artifact_id
  ON browser_exploration_actions
WHEN OLD.outcome IS NOT NULL AND (
  NEW.ended_at IS NOT OLD.ended_at OR NEW.outcome IS NOT OLD.outcome OR NEW.error_code IS NOT OLD.error_code
  OR NEW.observation_version IS NOT OLD.observation_version OR NEW.observation_digest IS NOT OLD.observation_digest
  OR NEW.observation_size_bytes IS NOT OLD.observation_size_bytes OR NEW.screenshot_artifact_id IS NOT OLD.screenshot_artifact_id)
BEGIN SELECT RAISE(ABORT, 'browser exploration action result is final once set'); END;

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
-- 首次 YAML 上传创建的聚合必须从 Disabled/未发布状态开始；YAML 的 enabled/timezone/interval
-- 只存在于第一份不可变草稿，显式发布后才投影到 business_systems（DATA-CONFIG-001）。
CREATE TRIGGER trg_business_systems_insert_unconfigured BEFORE INSERT ON business_systems
WHEN NEW.enabled <> 0 OR NEW.current_config_version_id IS NOT NULL
  OR NEW.timezone IS NOT NULL OR NEW.resource_refresh_interval_seconds IS NOT NULL
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
  trigger_kind, scheduled_for, rerun_of_id, created_at ON inspection_runs
BEGIN SELECT RAISE(ABORT, 'inspection_run binding is immutable'); END;
CREATE TRIGGER trg_execution_attempts_origin_immutable BEFORE UPDATE OF
  attempt_type, scope_type, scope_id, plan_key, check_key, requested_by_tool_call_id,
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
CREATE TRIGGER trg_maintenance_state_row_version_increment BEFORE UPDATE ON maintenance_state
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'maintenance_state row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_business_system_kubernetes_connections_row_version_increment BEFORE UPDATE ON business_system_kubernetes_connections
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'business_system_kubernetes_connections row_version must increase exactly by 1'); END;
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
CREATE TRIGGER trg_artifact_retention_settings_row_version_increment BEFORE UPDATE ON artifact_retention_settings
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'artifact_retention_settings row_version must increase exactly by 1'); END;
CREATE TRIGGER trg_backups_row_version_increment BEFORE UPDATE ON backups
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'backups row_version must increase exactly by 1'); END;
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
CREATE TRIGGER trg_business_systems_config_contract_fence BEFORE UPDATE OF current_config_version_id ON business_systems
WHEN NEW.current_config_version_id IS NOT NULL
  AND (OLD.current_config_version_id IS NULL OR NEW.current_config_version_id IS NOT OLD.current_config_version_id)
  AND NOT EXISTS (
    SELECT 1 FROM business_system_config_versions v
    JOIN label_contract_state s ON s.id = 1 AND s.current_contract_id = v.label_contract_version_id
    WHERE v.id = NEW.current_config_version_id
  )
  AND NOT EXISTS (
    SELECT 1 FROM label_contract_activations a
    JOIN business_system_config_versions v ON v.id = NEW.current_config_version_id AND v.label_contract_version_id = a.contract_id
    JOIN json_each(a.items_json) je
    WHERE a.applied_at IS NULL
      AND CAST(je.value ->> '$.business_system_id' AS INTEGER) = NEW.id
      AND CAST(je.value ->> '$.config_version_id' AS INTEGER) = NEW.current_config_version_id
  )
BEGIN SELECT RAISE(ABORT, 'config targeting a non-current label contract can only be published by that contract atomic activation'); END;
CREATE TRIGGER trg_business_systems_no_unset_config_pointer BEFORE UPDATE OF current_config_version_id ON business_systems
WHEN OLD.current_config_version_id IS NOT NULL AND NEW.current_config_version_id IS NULL
BEGIN SELECT RAISE(ABORT, 'business_systems current_config_version_id cannot be unset (no deactivation)'); END;
-- 根投影守卫：business_systems 的 display_name/enabled/timezone/resource_refresh_interval_seconds 必须
-- 等于 current 指针所指版本的类型化根投影（不允许绕过 YAML 发布直接改写，DATA-CONFIG-001）。
-- 指针变化与投影变化在同一 UPDATE 中强制一致（不允许只改指针不改投影或反之）。
CREATE TRIGGER trg_business_systems_projection_matches_version BEFORE UPDATE OF display_name, enabled, timezone, resource_refresh_interval_seconds ON business_systems
WHEN NEW.current_config_version_id IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM business_system_config_versions v
  WHERE v.id = NEW.current_config_version_id
    AND v.display_name = NEW.display_name AND v.enabled = NEW.enabled
    AND v.timezone = NEW.timezone AND v.resource_refresh_interval_seconds = NEW.resource_refresh_interval_seconds
)
BEGIN SELECT RAISE(ABORT, 'business_system root projection must equal its current config version root projection'); END;
-- 指针本身变化时也强制投影一致（指针 UPDATE 必须同时携带目标版本投影）。
CREATE TRIGGER trg_business_systems_pointer_projection_on_pointer_change BEFORE UPDATE OF current_config_version_id ON business_systems
WHEN NEW.current_config_version_id IS NOT NULL AND (OLD.current_config_version_id IS NULL OR NEW.current_config_version_id IS NOT OLD.current_config_version_id) AND NOT EXISTS (
  SELECT 1 FROM business_system_config_versions v
  WHERE v.id = NEW.current_config_version_id
    AND v.display_name = NEW.display_name AND v.enabled = NEW.enabled
    AND v.timezone = NEW.timezone AND v.resource_refresh_interval_seconds = NEW.resource_refresh_interval_seconds
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
CREATE TRIGGER trg_browser_identities_revision_owner_insert AFTER INSERT ON browser_identities
WHEN NOT EXISTS (
  SELECT 1 FROM browser_identity_revisions r
  WHERE r.id = NEW.current_revision_id AND r.business_system_id = NEW.business_system_id)
BEGIN SELECT RAISE(ABORT, 'current_revision_id must belong to the identity business system'); END;
CREATE TRIGGER trg_browser_identities_revision_owner_update BEFORE UPDATE OF current_revision_id ON browser_identities
WHEN NOT EXISTS (
  SELECT 1 FROM browser_identity_revisions r
  WHERE r.id = NEW.current_revision_id AND r.business_system_id = NEW.business_system_id)
  OR (NEW.current_profile_generation_id IS NULL AND NEW.state <> 'AuthenticationRequired')
  OR (NEW.current_profile_generation_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM browser_operations o WHERE o.identity_id = NEW.id
      AND o.kind = 'authentication_probe' AND o.probe_phase = 'revision_change'
      AND o.identity_revision_id = NEW.current_revision_id
      AND o.profile_generation_id = NEW.current_profile_generation_id
      AND o.state IN ('Queued','WaitingForCapacity','Starting','Running')
  ))
BEGIN SELECT RAISE(ABORT, 'revision switch requires an owned target and, when a profile exists, its active revision-change probe'); END;
CREATE TRIGGER trg_browser_identities_profile_pointer_publish_only BEFORE UPDATE OF current_profile_generation_id ON browser_identities
WHEN NEW.current_profile_generation_id IS NOT OLD.current_profile_generation_id AND NOT EXISTS (
  SELECT 1 FROM browser_profile_generations g JOIN browser_operations o ON o.id = g.published_operation_id
  WHERE g.id = NEW.current_profile_generation_id AND g.identity_id = NEW.id
    AND g.identity_revision_id = NEW.current_revision_id
    AND o.kind = 'manual_login' AND o.state IN ('Running','AwaitingReconnect')
)
BEGIN SELECT RAISE(ABORT, 'current profile pointer can only move through an active manual-login publication'); END;
CREATE TRIGGER trg_browser_identities_ready_publish_only BEFORE UPDATE OF state ON browser_identities
WHEN OLD.state = 'AuthenticationRequired' AND NEW.state = 'Ready' AND NOT EXISTS (
  SELECT 1 FROM browser_profile_generations g JOIN browser_operations o ON o.id = g.published_operation_id
  WHERE g.id = NEW.current_profile_generation_id AND g.identity_id = NEW.id
    AND g.identity_revision_id = NEW.current_revision_id
    AND o.kind = 'manual_login' AND o.state IN ('Running','AwaitingReconnect')
)
BEGIN SELECT RAISE(ABORT, 'AuthenticationRequired can return Ready only through a successful manual profile publication'); END;
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
CREATE TRIGGER trg_maintenance_state_lintel_recovery_actor BEFORE UPDATE OF active ON maintenance_state
WHEN (OLD.active = 0 AND NEW.active = 1 AND (
       (NEW.reason = 'LintelRecovery' AND NEW.entered_by_type <> 'deployment_helper')
       OR (NEW.reason <> 'LintelRecovery' AND NEW.entered_by_type = 'deployment_helper')))
   OR (OLD.active = 1 AND NEW.active = 0 AND (
       (OLD.reason = 'LintelRecovery' AND NEW.exited_by_type <> 'deployment_helper')
       OR (OLD.reason <> 'LintelRecovery' AND NEW.exited_by_type = 'deployment_helper')))
BEGIN SELECT RAISE(ABORT, 'LintelRecovery is entered and exited only by the deployment helper'); END;
CREATE TRIGGER trg_maintenance_state_lintel_recovery_receipt BEFORE UPDATE OF active ON maintenance_state
WHEN OLD.active = 1 AND OLD.reason = 'LintelRecovery' AND NEW.active = 0
  AND NOT EXISTS (SELECT 1 FROM lintel_recovery_receipts r WHERE r.maintenance_revision = OLD.row_version)
BEGIN SELECT RAISE(ABORT, 'LintelRecovery exit requires its immutable recovery receipt'); END;
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
-- Runtime slot 是固定键（CONTEXT「服务身份」）
CREATE TRIGGER trg_runtime_slots_slot_immutable BEFORE UPDATE OF slot ON runtime_slots
BEGIN SELECT RAISE(ABORT, 'runtime_slot key is fixed'); END;
-- Browser Operation 显式前向状态机；终态不可复活，row_version 每次 UPDATE 精确 +1。
CREATE TRIGGER trg_browser_operations_start_dispatch_once BEFORE UPDATE OF start_dispatched_at ON browser_operations
WHEN NOT (OLD.start_dispatched_at IS NULL AND NEW.start_dispatched_at IS NOT NULL
  AND OLD.lintel_boot_id IS NULL AND NEW.lintel_boot_id IS NOT NULL
  AND OLD.lintel_connection_epoch IS NULL AND NEW.lintel_connection_epoch IS NOT NULL
  AND OLD.state IN ('Queued','WaitingForCapacity') AND NEW.state = 'Starting')
BEGIN SELECT RAISE(ABORT, 'browser operation Start dispatch fence is write-once and must accompany entry to Starting'); END;
CREATE TRIGGER trg_browser_operations_start_binding_once BEFORE UPDATE OF lintel_boot_id, lintel_connection_epoch ON browser_operations
WHEN NOT (OLD.start_dispatched_at IS NULL AND NEW.start_dispatched_at IS NOT NULL
  AND OLD.lintel_boot_id IS NULL AND NEW.lintel_boot_id IS NOT NULL
  AND OLD.lintel_connection_epoch IS NULL AND NEW.lintel_connection_epoch IS NOT NULL
  AND OLD.state IN ('Queued','WaitingForCapacity') AND NEW.state = 'Starting')
BEGIN SELECT RAISE(ABORT, 'browser operation Lintel boot/epoch binding is immutable and must accompany Start dispatch'); END;
CREATE TRIGGER trg_browser_operations_start_ack_once BEFORE UPDATE OF started_at ON browser_operations
WHEN NOT (OLD.started_at IS NULL AND NEW.started_at IS NOT NULL AND OLD.state = 'Starting' AND NEW.state = 'Running')
BEGIN SELECT RAISE(ABORT, 'browser operation started_at is write-once and must accompany an accepted Start Ack'); END;
CREATE TRIGGER trg_browser_operations_start_rejection_once BEFORE UPDATE OF start_rejected_at, start_reject_reason ON browser_operations
WHEN NOT (OLD.start_rejected_at IS NULL AND OLD.start_reject_reason IS NULL
  AND NEW.start_rejected_at IS NOT NULL AND NEW.start_reject_reason IS NOT NULL
  AND OLD.state = 'Starting' AND NEW.state = 'Failed'
  AND NEW.stop_confirmed_at IS NOT NULL AND NEW.stop_confirmation_basis = 'start_rejected'
  AND (
    (NEW.start_reject_reason IN ('identity_busy','input_unsupported','reconcile_required','stale_stream','internal') AND NEW.terminal_reason = 'protocol_error')
    OR (NEW.start_reject_reason = 'authentication_required' AND NEW.kind <> 'manual_login' AND NEW.terminal_reason = 'authentication_required')
    OR (NEW.start_reject_reason = 'profile_unavailable' AND NEW.kind <> 'manual_login'
      AND NEW.terminal_reason IN ('profile_missing','profile_manifest_invalid','chromium_revision_mismatch'))
  ))
BEGIN SELECT RAISE(ABORT, 'browser operation Start rejection is write-once and must atomically fail and confirm no process'); END;
CREATE TRIGGER trg_browser_operations_state_transition BEFORE UPDATE OF state ON browser_operations
WHEN OLD.state <> NEW.state AND NOT (
  (OLD.state = 'Queued' AND NEW.state IN ('WaitingForCapacity','Starting','Failed','Cancelled','Interrupted'))
  OR (OLD.state = 'WaitingForCapacity' AND NEW.state IN ('Starting','Failed','Cancelled','Interrupted'))
  OR (OLD.state = 'Starting' AND NEW.state IN ('WaitingForCapacity','Running','Failed','Cancelled','Interrupted'))
  OR (OLD.state = 'Running' AND NEW.state IN ('AwaitingReconnect','Succeeded','Failed','Cancelled','Interrupted'))
  OR (OLD.state = 'AwaitingReconnect' AND NEW.state IN ('Running','Succeeded','Failed','Cancelled','Interrupted'))
)
BEGIN SELECT RAISE(ABORT, 'invalid browser_operation state transition'); END;
CREATE TRIGGER trg_browser_operations_terminal_immutable BEFORE UPDATE OF
  identity_id, identity_revision_id, profile_generation_id, owner_attempt_id, kind, actor_user_id, state,
  journey_catalog_digest, journey_catalog_version, journey_id, journey_version, probe_phase,
  requested_at, start_dispatched_at, lintel_boot_id, lintel_connection_epoch, started_at, start_rejected_at, start_reject_reason, reconnect_deadline, ended_at, terminal_reason, trace_artifact_id, trace_integrity
  ON browser_operations
WHEN OLD.state IN ('Succeeded','Failed','Cancelled','Interrupted')
BEGIN SELECT RAISE(ABORT, 'terminal browser_operation domain result is immutable'); END;
CREATE TRIGGER trg_browser_operations_stop_confirmation_once BEFORE UPDATE OF stop_confirmed_at, stop_confirmation_basis ON browser_operations
WHEN NOT (
  (OLD.stop_confirmed_at IS NULL AND OLD.stop_confirmation_basis IS NULL
    AND NEW.stop_confirmed_at IS NOT NULL AND NEW.stop_confirmation_basis IS NOT NULL
    AND ((OLD.state NOT IN ('Succeeded','Failed','Cancelled','Interrupted')
          AND NEW.state IN ('Succeeded','Failed','Cancelled','Interrupted'))
      OR (OLD.state IN ('Succeeded','Failed','Cancelled','Interrupted') AND NEW.state = OLD.state)))
  OR (NEW.stop_confirmed_at IS OLD.stop_confirmed_at AND NEW.stop_confirmation_basis IS OLD.stop_confirmation_basis)
)
BEGIN SELECT RAISE(ABORT, 'browser_operation stop confirmation and its basis are terminal and write-once'); END;
-- 显式取消的父 Attempt 先进入 Cancelling；自然成功/失败/中断时保持 Running 直至收口。
-- 任何终态都必须等待会话级 operation、completion probe 与强制 trace 先提交；数据库不得用父状态 UPDATE 伪造浏览器终局。
CREATE TRIGGER trg_execution_attempts_exploration_requires_closed_operation BEFORE UPDATE OF state ON execution_attempts
WHEN OLD.attempt_type = 'investigation' AND NEW.state IN ('Succeeded','Failed','Cancelled','Interrupted')
  AND EXISTS (
    SELECT 1 FROM browser_operations o WHERE o.owner_attempt_id = OLD.id AND o.kind = 'exploration'
      AND o.state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect'))
BEGIN SELECT RAISE(ABORT, 'investigation attempt may terminate only after its exploration operation and mandatory trace are closed'); END;
CREATE TRIGGER trg_execution_attempts_journey_requires_closed_operation BEFORE UPDATE OF state ON execution_attempts
WHEN OLD.attempt_type = 'inspection_collection' AND NEW.state IN ('Succeeded','Failed','Cancelled','Interrupted')
  AND EXISTS (
    SELECT 1 FROM browser_operations o WHERE o.owner_attempt_id = OLD.id AND o.kind = 'journey'
      AND o.state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect'))
BEGIN SELECT RAISE(ABORT, 'inspection collection attempt may terminate only after its journey operation and mandatory trace are closed'); END;
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
    (OLD.attempt_type = 'browser_exploration' AND OLD.requested_by_tool_call_id IS NOT NULL)
    OR (OLD.attempt_type = 'inspection_collection' AND (
      EXISTS (SELECT 1 FROM inspection_check_results r WHERE r.attempt_id = OLD.id AND r.result_digest IS NOT NULL)
      OR EXISTS (SELECT 1 FROM config_verification_run_check_results r WHERE r.attempt_id = OLD.id AND r.result_digest IS NOT NULL)
      OR EXISTS (SELECT 1 FROM observed_refresh_log l WHERE l.attempt_id = OLD.id AND l.result_digest IS NOT NULL)
    ))
  ))
  OR (OLD.state = 'Assigned' AND NEW.state IN ('Running','Failed','Cancelled','Interrupted'))
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
CREATE TRIGGER trg_task_change_log_browser_insert AFTER INSERT ON browser_operations
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('browser_operation', NEW.id, 'created', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_browser_state AFTER UPDATE OF state ON browser_operations
WHEN NEW.state <> OLD.state
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('browser_operation', NEW.id, 'state_changed', NEW.row_version);
END;

-- 12.30 Runtime 注册/凭据状态机与 Artifact 上传 ledger（DATA-RUNTIME-001/002、DATA-ARTIFACT-006）
-- runtime_slots 持久状态转换：unregistered -> registered（注册成功）、registered -> revoked（替换）、
-- revoked -> registered（替换后注册）。current 是首选 token，pending 是尚未提升的新 token，retiring 是
-- 已被新 current 替代但等待 Admin 显式退休的旧 token；认证只接受 current 或 retiring。
CREATE TRIGGER trg_runtime_slots_state_transition BEFORE UPDATE OF state ON runtime_slots
WHEN OLD.state <> NEW.state AND NOT (
  (OLD.state = 'unregistered' AND NEW.state IN ('registered','revoked'))
  OR (OLD.state = 'registered' AND NEW.state = 'revoked')
  OR (OLD.state = 'revoked' AND NEW.state = 'registered')
)
BEGIN SELECT RAISE(ABORT, 'runtime_slot state transition only unregistered->registered/revoked, registered->revoked, revoked->registered'); END;

CREATE TRIGGER trg_runtime_slots_current_owner_insert AFTER INSERT ON runtime_slots
WHEN NEW.current_credential_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM runtime_credentials c WHERE c.id = NEW.current_credential_id AND c.slot = NEW.slot
     AND c.confirmed_at IS NOT NULL AND c.retired_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'runtime current must reference a confirmed unretired credential of the same slot'); END;
CREATE TRIGGER trg_runtime_slots_current_owner_update AFTER UPDATE OF current_credential_id ON runtime_slots
WHEN NEW.current_credential_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM runtime_credentials c WHERE c.id = NEW.current_credential_id AND c.slot = NEW.slot
     AND c.confirmed_at IS NOT NULL AND c.retired_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'runtime current must reference a confirmed unretired credential of the same slot'); END;
CREATE TRIGGER trg_runtime_slots_pending_owner_insert AFTER INSERT ON runtime_slots
WHEN NEW.pending_credential_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM runtime_credentials c WHERE c.id = NEW.pending_credential_id AND c.slot = NEW.slot AND c.retired_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'runtime pending must reference an unretired credential of the same slot'); END;
CREATE TRIGGER trg_runtime_slots_pending_owner_update AFTER UPDATE OF pending_credential_id ON runtime_slots
WHEN NEW.pending_credential_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM runtime_credentials c WHERE c.id = NEW.pending_credential_id AND c.slot = NEW.slot AND c.retired_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'runtime pending must reference an unretired credential of the same slot'); END;
CREATE TRIGGER trg_runtime_slots_retiring_owner_insert AFTER INSERT ON runtime_slots
WHEN NEW.retiring_credential_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM runtime_credentials c WHERE c.id = NEW.retiring_credential_id AND c.slot = NEW.slot
     AND c.confirmed_at IS NOT NULL AND c.retired_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'runtime retiring must reference a confirmed unretired credential of the same slot'); END;
CREATE TRIGGER trg_runtime_slots_retiring_owner_update AFTER UPDATE OF retiring_credential_id ON runtime_slots
WHEN NEW.retiring_credential_id IS NOT NULL AND NOT EXISTS
  (SELECT 1 FROM runtime_credentials c WHERE c.id = NEW.retiring_credential_id AND c.slot = NEW.slot
     AND c.confirmed_at IS NOT NULL AND c.retired_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'runtime retiring must reference a confirmed unretired credential of the same slot'); END;

-- 已有待确认或待退休 token 时不得开始另一轮；pending 中止先清空，AFTER 触发器退休孤儿。
CREATE TRIGGER trg_runtime_slots_pending_no_direct_swap BEFORE UPDATE OF pending_credential_id ON runtime_slots
WHEN OLD.pending_credential_id IS NOT NULL AND NEW.pending_credential_id IS NOT NULL
  AND NEW.pending_credential_id IS NOT OLD.pending_credential_id
BEGIN SELECT RAISE(ABORT, 'runtime pending must be cleared before another rotation'); END;
CREATE TRIGGER trg_runtime_slots_no_new_pending_while_retiring BEFORE UPDATE OF pending_credential_id ON runtime_slots
WHEN OLD.pending_credential_id IS NULL AND NEW.pending_credential_id IS NOT NULL AND OLD.retiring_credential_id IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'retiring runtime credential must be explicitly retired before another rotation'); END;
CREATE TRIGGER trg_runtime_slots_retiring_no_direct_swap BEFORE UPDATE OF retiring_credential_id ON runtime_slots
WHEN OLD.retiring_credential_id IS NOT NULL AND NEW.retiring_credential_id IS NOT NULL
  AND NEW.retiring_credential_id IS NOT OLD.retiring_credential_id
BEGIN SELECT RAISE(ABORT, 'runtime retiring credential must be cleared before another value'); END;
CREATE TRIGGER trg_runtime_slots_retiring_only_from_promotion BEFORE UPDATE OF retiring_credential_id ON runtime_slots
WHEN OLD.retiring_credential_id IS NULL AND NEW.retiring_credential_id IS NOT NULL AND NOT (
  OLD.state = 'registered' AND NEW.state = 'registered'
  AND OLD.pending_credential_id IS NOT NULL
  AND NEW.retiring_credential_id IS OLD.current_credential_id
  AND NEW.current_credential_id IS OLD.pending_credential_id
  AND NEW.pending_credential_id IS NULL)
BEGIN SELECT RAISE(ABORT, 'runtime retiring credential can only be created by atomic pending promotion'); END;

-- registered slot 更换 current 只能在一条 UPDATE 中提升旧 pending，并把旧 current 移入 retiring；
-- 旧 token 不自动退休。已有 retiring 时禁止提升，从而最多一个显式退休窗口。
CREATE TRIGGER trg_runtime_slots_promote_requires_pending AFTER UPDATE OF current_credential_id ON runtime_slots
WHEN OLD.state = 'registered' AND NEW.state = 'registered'
  AND NEW.current_credential_id IS NOT OLD.current_credential_id
  AND (
    OLD.pending_credential_id IS NULL
    OR OLD.retiring_credential_id IS NOT NULL
    OR NEW.current_credential_id IS NOT OLD.pending_credential_id
    OR NEW.pending_credential_id IS NOT NULL
    OR NEW.retiring_credential_id IS NOT OLD.current_credential_id
  )
BEGIN SELECT RAISE(ABORT, 'runtime promotion must atomically set current=old pending, pending=NULL, retiring=old current'); END;

-- pending 中止退休该孤儿；替换退休全部；retiring 只有 Admin 显式清指针后退休。
CREATE TRIGGER trg_runtime_slots_abort_retire_pending AFTER UPDATE OF pending_credential_id ON runtime_slots
WHEN OLD.pending_credential_id IS NOT NULL AND NEW.pending_credential_id IS NULL
  AND NEW.current_credential_id IS OLD.current_credential_id
  AND NEW.retiring_credential_id IS OLD.retiring_credential_id
BEGIN
  UPDATE runtime_credentials SET retired_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'), row_version = row_version + 1
  WHERE id = OLD.pending_credential_id AND retired_at IS NULL;
END;
CREATE TRIGGER trg_runtime_slots_retiring_requires_new_auth BEFORE UPDATE OF retiring_credential_id ON runtime_slots
WHEN OLD.retiring_credential_id IS NOT NULL AND NEW.retiring_credential_id IS NULL
  AND OLD.state = 'registered' AND NEW.state = 'registered'
  AND NOT EXISTS (SELECT 1 FROM runtime_credentials c WHERE c.id = NEW.current_credential_id AND c.first_authenticated_at IS NOT NULL)
BEGIN SELECT RAISE(ABORT, 'runtime old credential cannot retire before the new current token authenticates successfully'); END;
CREATE TRIGGER trg_runtime_slots_retire_old_explicit AFTER UPDATE OF retiring_credential_id ON runtime_slots
WHEN OLD.retiring_credential_id IS NOT NULL AND NEW.retiring_credential_id IS NULL
BEGIN
  UPDATE runtime_credentials SET retired_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'), row_version = row_version + 1
  WHERE id = OLD.retiring_credential_id AND retired_at IS NULL;
END;
CREATE TRIGGER trg_runtime_slots_replace_retire_all AFTER UPDATE OF state ON runtime_slots
WHEN NEW.state = 'revoked' AND OLD.state <> 'revoked'
BEGIN
  UPDATE runtime_credentials SET retired_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'), row_version = row_version + 1
  WHERE slot = NEW.slot AND retired_at IS NULL;
END;

-- Runtime credential 来源与历史不可改写；confirmed/first_authenticated/retired 是各自一次性事实。
CREATE TRIGGER trg_runtime_credentials_origin_immutable BEFORE UPDATE OF
  slot, generation, token_digest, created_at ON runtime_credentials
BEGIN SELECT RAISE(ABORT, 'runtime credential origin is immutable'); END;
CREATE TRIGGER trg_runtime_credentials_no_delete BEFORE DELETE ON runtime_credentials
BEGIN SELECT RAISE(ABORT, 'runtime credentials history is not deletable'); END;
CREATE TRIGGER trg_runtime_credentials_confirmed_once BEFORE UPDATE OF confirmed_at ON runtime_credentials
WHEN OLD.confirmed_at IS NOT NULL OR NEW.confirmed_at IS NULL
BEGIN SELECT RAISE(ABORT, 'runtime confirmed_at may only advance once from NULL'); END;
CREATE TRIGGER trg_runtime_credentials_first_auth_once BEFORE UPDATE OF first_authenticated_at ON runtime_credentials
WHEN OLD.first_authenticated_at IS NOT NULL OR NEW.first_authenticated_at IS NULL
BEGIN SELECT RAISE(ABORT, 'runtime first_authenticated_at may only advance once from NULL'); END;
CREATE TRIGGER trg_runtime_credentials_first_auth_current BEFORE UPDATE OF first_authenticated_at ON runtime_credentials
WHEN OLD.first_authenticated_at IS NULL AND NEW.first_authenticated_at IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM runtime_slots s WHERE s.slot = NEW.slot AND s.current_credential_id = NEW.id AND s.state = 'registered')
BEGIN SELECT RAISE(ABORT, 'only the registered current runtime credential may record first authentication'); END;
CREATE TRIGGER trg_runtime_credentials_retired_once BEFORE UPDATE OF retired_at ON runtime_credentials
WHEN OLD.retired_at IS NOT NULL OR NEW.retired_at IS NULL
BEGIN SELECT RAISE(ABORT, 'runtime retired_at may only advance once from NULL'); END;
CREATE TRIGGER trg_runtime_credentials_no_retire_while_referenced BEFORE UPDATE OF retired_at ON runtime_credentials
WHEN NEW.retired_at IS NOT NULL AND OLD.retired_at IS NULL AND EXISTS (
  SELECT 1 FROM runtime_slots s WHERE s.slot = OLD.slot
    AND (s.current_credential_id = OLD.id OR s.pending_credential_id = OLD.id OR s.retiring_credential_id = OLD.id))
BEGIN SELECT RAISE(ABORT, 'runtime credential cannot retire while referenced by current, pending, or retiring'); END;
CREATE TRIGGER trg_runtime_credentials_no_confirm_after_retire BEFORE UPDATE OF confirmed_at ON runtime_credentials
WHEN NEW.confirmed_at IS NOT NULL AND OLD.confirmed_at IS NULL AND (OLD.retired_at IS NOT NULL OR NEW.retired_at IS NOT NULL)
BEGIN SELECT RAISE(ABORT, 'runtime credential cannot be confirmed after retirement'); END;
CREATE TRIGGER trg_runtime_credentials_insert_clean BEFORE INSERT ON runtime_credentials
WHEN NEW.retired_at IS NOT NULL OR NEW.first_authenticated_at IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'runtime credential must be created unretired and not yet authenticated'); END;
CREATE TRIGGER trg_runtime_credentials_confirm_requires_pending BEFORE UPDATE OF confirmed_at ON runtime_credentials
WHEN NEW.confirmed_at IS NOT NULL AND OLD.confirmed_at IS NULL AND NOT EXISTS (
  SELECT 1 FROM runtime_slots s WHERE s.slot = NEW.slot AND s.pending_credential_id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'runtime credential confirmation requires the slot pending pointer'); END;
CREATE TRIGGER trg_runtime_credentials_insert_confirmed_registration_only BEFORE INSERT ON runtime_credentials
WHEN NEW.confirmed_at IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM runtime_slots s WHERE s.slot = NEW.slot
    AND s.state IN ('unregistered','revoked')
    AND s.current_credential_id IS NULL AND s.pending_credential_id IS NULL AND s.retiring_credential_id IS NULL)
BEGIN SELECT RAISE(ABORT, 'confirmed runtime credential may only be inserted in an empty registration window'); END;
-- runtime_artifact_uploads：来源字段不可改写（含 boot_id）；只能以 uploading 创建，状态转换仅
-- uploading->committed/rejected 且终态不可变；committed 必须满足 NULL-safe 正向条件：所引 Attempt
-- 普通上传必须绑定 state='Running' Attempt 且 runtime_slot/boot_id/connection_epoch 精确一致；
-- Session/Journey 连续 trace 可改由 active browser_operation 的冻结 Lintel boot/epoch 授权而不伪造一个 Tool Call Attempt。
-- 旧 Attempt、错误 operation 绑定或 ABA/epoch 不符一律拒绝 commit，只能 rejected；
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
    AND (
      EXISTS (
        SELECT 1 FROM execution_attempts a WHERE a.id = NEW.attempt_id
          AND a.state = 'Running' AND a.runtime_slot IS NOT NULL
          AND a.boot_id = NEW.boot_id AND a.connection_epoch = NEW.connection_epoch)
      OR (NEW.attempt_id IS NULL AND NEW.kind = 'trace' AND NEW.owner_type = 'browser_operation'
        AND EXISTS (
          SELECT 1 FROM browser_operations o WHERE o.id = NEW.owner_id
            AND o.kind IN ('journey','exploration') AND o.state = 'Running'
            AND o.lintel_boot_id = NEW.boot_id AND o.lintel_connection_epoch = NEW.connection_epoch))
    )
)
BEGIN SELECT RAISE(ABORT, 'runtime_artifact_upload commit requires a matching Running Attempt or active browser operation Lintel binding and an exactly matching artifact'); END;
CREATE TRIGGER trg_runtime_artifact_uploads_tool_result_owner BEFORE INSERT ON runtime_artifact_uploads
WHEN NEW.kind = 'tool_result' AND (
  NEW.owner_type <> 'tool_call' OR NEW.retention_kind <> 'generated' OR NOT EXISTS (
    SELECT 1 FROM tool_calls t WHERE t.id = NEW.owner_id AND t.attempt_id = NEW.attempt_id))
BEGIN SELECT RAISE(ABORT, 'tool_result upload must be generated and owned by a Tool Call of the same Attempt'); END;
CREATE TRIGGER trg_runtime_artifact_uploads_browser_owner BEFORE INSERT ON runtime_artifact_uploads
WHEN NEW.kind IN ('trace','screenshot') AND (
  NEW.owner_type <> 'browser_operation' OR NOT EXISTS (
    SELECT 1 FROM browser_operations o WHERE o.id = NEW.owner_id AND (
      (NEW.attempt_id IS NULL AND NEW.kind = 'trace' AND o.kind IN ('journey','exploration'))
      OR o.owner_attempt_id = NEW.attempt_id OR EXISTS (
        SELECT 1 FROM browser_exploration_actions x
        WHERE x.operation_id = o.id AND x.child_attempt_id = NEW.attempt_id)
    )
  )
)
BEGIN SELECT RAISE(ABORT, 'browser upload must be owned by the operation itself for continuous trace or linked to its Journey/exploration child Attempt'); END;
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

-- 替换 fence（Q237「有 active task 时必须先等待完成或明确取消才能替换」）：
-- Execution Attempt 可由 Plinth 或 Lintel 承载（browser_exploration 与巡检 Journey 的子 Attempt
-- 绑定 Lintel，CONTEXT「执行尝试」）；Browser Operation 全部由 Lintel 承载。任意 slot 置 revoked 前
-- 必须没有绑定该 slot 的 active Attempt（Assigned/Running/Cancelling）；此外 lintel 还必须没有
-- active Browser Operation（领域状态非终态，或虽已终态但尚无 stop_confirmed_at 物理停止事实；直接检查，
-- 不通过 Attempt 推断——manual_login、Queued/未派发 Attempt 或已终态 Attempt 上仍存活的浏览器进程同样拦截）。
-- 应用事务先检查并拒绝（409 active_conflict），本触发器是机械兑底；正常替换流程中不存在 active。
CREATE TRIGGER trg_runtime_slots_no_replace_with_active BEFORE UPDATE OF state ON runtime_slots
WHEN NEW.state = 'revoked' AND (
  EXISTS (SELECT 1 FROM execution_attempts a WHERE a.runtime_slot = NEW.slot AND a.state IN ('Assigned','Running','Cancelling'))
  OR (NEW.slot = 'lintel' AND EXISTS (
    SELECT 1 FROM browser_operations bo
    WHERE bo.state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect') OR bo.stop_confirmed_at IS NULL))
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
    AND parent.runtime_slot = 'plinth' AND parent.state = 'Running'
    AND parent.attempt_type = 'investigation' AND parent.scope_type = 'investigation'
    AND NEW.scope_type = 'investigation' AND NEW.scope_id = parent.scope_id
    AND tc.execution_mode = 'quoin_browser' AND tc.status = 'running'
)
BEGIN SELECT RAISE(ABORT, 'cross-runtime sub-execution requestor must be a tool call of a plinth-dispatched parent attempt'); END;
CREATE TRIGGER trg_execution_attempts_requestor_immutable BEFORE UPDATE OF requested_by_tool_call_id ON execution_attempts
WHEN NOT (OLD.requested_by_tool_call_id IS NEW.requested_by_tool_call_id)
BEGIN SELECT RAISE(ABORT, 'execution_attempt requestor tool call is immutable once set'); END;

-- 12.36
-- 终态写 fence：只允许状态变化 UPDATE；evidence_at 必须随进入 Running 首次写入；
-- 终态后禁止任何 UPDATE、子 Attempt、check result。check result 只能在 Running 插入。
-- Passed 必须覆盖绑定配置版本全部 check 且每项 ok+Evidence。
CREATE TRIGGER trg_config_verification_runs_no_origin_update BEFORE UPDATE OF
  purpose, business_system_id, config_version_id, label_contract_version_id, verification_manifest_item_id,
  created_by, created_at ON config_verification_runs
BEGIN SELECT RAISE(ABORT, 'config_verification_run origin is immutable'); END;
CREATE TRIGGER trg_config_verification_runs_no_delete BEFORE DELETE ON config_verification_runs
BEGIN SELECT RAISE(ABORT, 'config_verification_run history is not deletable'); END;
CREATE TRIGGER trg_config_verification_runs_row_version_increment BEFORE UPDATE ON config_verification_runs
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'config_verification_run row_version must increment by exactly 1'); END;
-- 闭合：prepublish 只测未发布草稿；deployment_acceptance 只测 manifest 冻结时 current published 指针。
CREATE TRIGGER trg_config_verification_runs_closure BEFORE INSERT ON config_verification_runs
WHEN NOT EXISTS (
  SELECT 1 FROM business_system_config_versions v
  WHERE v.id = NEW.config_version_id
    AND v.business_system_id = NEW.business_system_id
    AND v.label_contract_version_id = NEW.label_contract_version_id
    AND (
      (NEW.purpose = 'prepublish' AND v.state = 'draft' AND v.published_at IS NULL)
      OR (NEW.purpose = 'deployment_acceptance' AND v.state = 'published' AND v.published_at IS NOT NULL
        AND EXISTS (SELECT 1 FROM business_systems b WHERE b.id = NEW.business_system_id AND b.current_config_version_id = v.id)
        AND EXISTS (SELECT 1 FROM label_contract_state s WHERE s.id = 1 AND s.current_contract_id = NEW.label_contract_version_id)
        AND EXISTS (
          SELECT 1 FROM verification_invocation_items i
          JOIN verification_invocation_manifests m ON m.id = i.invocation_id
          JOIN verification_config_item_locators l ON l.item_id = i.id
          WHERE i.id = NEW.verification_manifest_item_id AND i.object_kind = 'config'
            AND l.business_system_id = NEW.business_system_id
            AND l.config_version_id = NEW.config_version_id
            AND l.label_contract_version_id = NEW.label_contract_version_id
            AND julianday(NEW.created_at) <= julianday(m.deadline_at)
            AND NOT EXISTS (SELECT 1 FROM verification_finalization_receipts fr WHERE fr.invocation_id = m.id))))
)
BEGIN SELECT RAISE(ABORT, 'config_verification_run purpose must bind the corresponding draft or manifest-frozen current published config'); END;
-- 只能以 Queued 创建；显式前向状态机。
CREATE TRIGGER trg_config_verification_runs_insert_state BEFORE INSERT ON config_verification_runs
WHEN NEW.state <> 'Queued' OR NEW.evidence_at IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'config_verification_run must be created as Queued without evidence_at'); END;
CREATE TRIGGER trg_config_verification_runs_state_transition BEFORE UPDATE OF state ON config_verification_runs
WHEN NEW.state <> OLD.state AND NOT (
  (OLD.state = 'Queued' AND NEW.state IN ('Running','Failed','Cancelled','Interrupted'))
  OR (OLD.state = 'Running' AND NEW.state IN ('Passed','Failed','Cancelled','Interrupted'))
)
BEGIN SELECT RAISE(ABORT, 'illegal config_verification_run state transition'); END;
-- 进入 Running 时必须同时写入 evidence_at（同一条 UPDATE）。
CREATE TRIGGER trg_config_verification_runs_running_requires_evidence_at BEFORE UPDATE OF state ON config_verification_runs
WHEN OLD.state <> 'Running' AND NEW.state = 'Running' AND NEW.evidence_at IS NULL
BEGIN SELECT RAISE(ABORT, 'config_verification_run evidence_at must be set when entering Running'); END;
-- Passed 证据：绑定配置版本的全部 check 都有 ok+Evidence 结果行（且无多余/非 ok 行）。
CREATE TRIGGER trg_config_verification_runs_passed_requires_full_ok BEFORE UPDATE OF state ON config_verification_runs
WHEN NEW.state = 'Passed' AND OLD.state <> 'Passed' AND (
  EXISTS (SELECT 1 FROM config_checks c JOIN config_plans p ON p.id = c.plan_id
          WHERE p.config_version_id = OLD.config_version_id
            AND NOT EXISTS (SELECT 1 FROM config_verification_run_check_results r WHERE r.verification_run_id = OLD.id AND r.plan_key = p.plan_key AND r.check_key = c.check_key))
  OR EXISTS (SELECT 1 FROM config_verification_run_check_results r WHERE r.verification_run_id = OLD.id AND (r.status <> 'ok' OR r.evidence_id IS NULL))
)
BEGIN SELECT RAISE(ABORT, 'config_verification_run can only Pass when every check of the bound config version has an ok result with evidence'); END;
-- 父 Config Verification Run 进入终态前必须在同一事务 fence 子 Attempt；Cancelled 允许已进入 Cancelling 的运行子 Attempt。
CREATE TRIGGER trg_config_verification_runs_terminal_children_fenced BEFORE UPDATE OF state ON config_verification_runs
WHEN NEW.state <> OLD.state AND NEW.state IN ('Passed','Failed','Cancelled','Interrupted') AND EXISTS (
  SELECT 1 FROM execution_attempts a
  WHERE a.scope_type = 'config_verification_run' AND a.scope_id = NEW.id
    AND (a.state IN ('Queued','Assigned','Running') OR (NEW.state <> 'Cancelled' AND a.state = 'Cancelling'))
)
BEGIN SELECT RAISE(ABORT, 'config_verification_run cannot become terminal before child attempts are fenced'); END;
-- evidence_at/result_detail 是一次性事实。
CREATE TRIGGER trg_config_verification_runs_evidence_at_once BEFORE UPDATE OF evidence_at ON config_verification_runs
WHEN (OLD.evidence_at IS NOT NULL AND NEW.evidence_at IS NOT OLD.evidence_at)
  OR (OLD.evidence_at IS NULL AND NEW.evidence_at IS NOT NULL AND NEW.state <> 'Running')
BEGIN SELECT RAISE(ABORT, 'config_verification_run evidence_at is a derived one-time fact written only while entering Running'); END;
CREATE TRIGGER trg_config_verification_runs_result_detail_once BEFORE UPDATE OF result_detail ON config_verification_runs
WHEN OLD.result_detail IS NOT NULL AND NEW.result_detail IS NOT OLD.result_detail
BEGIN SELECT RAISE(ABORT, 'config_verification_run result_detail is a one-time fact'); END;
-- 终态写 fence：终态后禁止任何 UPDATE。
CREATE TRIGGER trg_config_verification_runs_terminal_no_update BEFORE UPDATE ON config_verification_runs
WHEN OLD.state IN ('Passed','Failed','Cancelled','Interrupted')
BEGIN SELECT RAISE(ABORT, 'config_verification_run is terminal; no updates allowed'); END;
-- 结果行 append-only：不可 UPDATE/不可 DELETE。
CREATE TRIGGER trg_config_verification_run_check_results_no_update BEFORE UPDATE ON config_verification_run_check_results
BEGIN SELECT RAISE(ABORT, 'config_verification_run_check_results is append-only'); END;
CREATE TRIGGER trg_config_verification_run_check_results_no_delete BEFORE DELETE ON config_verification_run_check_results
BEGIN SELECT RAISE(ABORT, 'config_verification_run_check_results is append-only'); END;

-- Resource Refresh Run：仅当前已发布配置能创建；输入来源、状态和完成事实均不可回写。
CREATE TRIGGER trg_resource_refresh_runs_no_origin_update BEFORE UPDATE OF
  business_system_id, config_version_id, label_contract_version_id, trigger_kind, scheduled_for, created_by, created_at ON resource_refresh_runs
BEGIN SELECT RAISE(ABORT, 'resource_refresh_run origin is immutable'); END;
CREATE TRIGGER trg_resource_refresh_runs_no_delete BEFORE DELETE ON resource_refresh_runs
BEGIN SELECT RAISE(ABORT, 'resource_refresh_run history is not deletable'); END;
CREATE TRIGGER trg_resource_refresh_runs_row_version_increment BEFORE UPDATE ON resource_refresh_runs
WHEN NEW.row_version <> OLD.row_version + 1
BEGIN SELECT RAISE(ABORT, 'resource_refresh_run row_version must increment by exactly 1'); END;
CREATE TRIGGER trg_resource_refresh_runs_closure BEFORE INSERT ON resource_refresh_runs
WHEN NOT EXISTS (
  SELECT 1 FROM business_system_config_versions v
  JOIN business_systems b ON b.id = v.business_system_id
  JOIN label_contract_state l ON l.id = 1
  WHERE v.id = NEW.config_version_id AND v.business_system_id = NEW.business_system_id
    AND v.label_contract_version_id = NEW.label_contract_version_id
    AND v.state = 'published' AND b.current_config_version_id = v.id
    AND l.current_contract_id = NEW.label_contract_version_id
)
BEGIN SELECT RAISE(ABORT, 'resource_refresh_run must freeze the business system current published config and label contract'); END;
CREATE TRIGGER trg_resource_refresh_runs_insert_state BEFORE INSERT ON resource_refresh_runs
WHEN NEW.state <> 'Queued' OR NEW.evidence_at IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'resource_refresh_run must be created as Queued without evidence_at'); END;
CREATE TRIGGER trg_resource_refresh_runs_state_transition BEFORE UPDATE OF state ON resource_refresh_runs
WHEN NEW.state <> OLD.state AND NOT (
  (OLD.state = 'Queued' AND NEW.state IN ('Running','Failed','Cancelled','Interrupted'))
  OR (OLD.state = 'Running' AND NEW.state IN ('Completed','CompletedWithWarnings','Failed','Cancelled','Interrupted'))
)
BEGIN SELECT RAISE(ABORT, 'illegal resource_refresh_run state transition'); END;
CREATE TRIGGER trg_resource_refresh_runs_running_requires_evidence_at BEFORE UPDATE OF state ON resource_refresh_runs
WHEN OLD.state <> 'Running' AND NEW.state = 'Running' AND NEW.evidence_at IS NULL
BEGIN SELECT RAISE(ABORT, 'resource_refresh_run evidence_at must be set when entering Running'); END;
CREATE TRIGGER trg_resource_refresh_runs_evidence_at_once BEFORE UPDATE OF evidence_at ON resource_refresh_runs
WHEN (OLD.evidence_at IS NOT NULL AND NEW.evidence_at IS NOT OLD.evidence_at)
  OR (OLD.evidence_at IS NULL AND NEW.evidence_at IS NOT NULL AND NEW.state <> 'Running')
BEGIN SELECT RAISE(ABORT, 'resource_refresh_run evidence_at is a one-time fact written only while entering Running'); END;
CREATE TRIGGER trg_resource_refresh_runs_result_detail_once BEFORE UPDATE OF result_detail ON resource_refresh_runs
WHEN OLD.result_detail IS NOT NULL AND NEW.result_detail IS NOT OLD.result_detail
BEGIN SELECT RAISE(ABORT, 'resource_refresh_run result_detail is a one-time fact'); END;
CREATE TRIGGER trg_resource_refresh_runs_terminal_children_fenced BEFORE UPDATE OF state ON resource_refresh_runs
WHEN NEW.state <> OLD.state AND NEW.state IN ('Completed','CompletedWithWarnings','Failed','Cancelled','Interrupted') AND EXISTS (
  SELECT 1 FROM execution_attempts a
  WHERE a.scope_type = 'resource_refresh_run' AND a.scope_id = NEW.id
    AND (a.state IN ('Queued','Assigned','Running') OR (NEW.state <> 'Cancelled' AND a.state = 'Cancelling'))
)
BEGIN SELECT RAISE(ABORT, 'resource_refresh_run cannot become terminal before child attempts are fenced'); END;
CREATE TRIGGER trg_resource_refresh_runs_terminal_no_update BEFORE UPDATE ON resource_refresh_runs
WHEN OLD.state IN ('Completed','CompletedWithWarnings','Failed','Cancelled','Interrupted')
BEGIN SELECT RAISE(ABORT, 'resource_refresh_run is terminal; no updates allowed'); END;
-- check result 只能在 Config Verification Run 处于 Running 时插入。
CREATE TRIGGER trg_config_verification_run_check_results_running_only BEFORE INSERT ON config_verification_run_check_results
WHEN NOT EXISTS (SELECT 1 FROM config_verification_runs t WHERE t.id = NEW.verification_run_id AND t.state = 'Running')
BEGIN SELECT RAISE(ABORT, 'config_verification_run check results can only be inserted while the config verification run is Running'); END;
-- check result closure：plan_key+check_key 必须存在于绑定配置版本。PromQL ok 与 Journey success
-- 引用唯一完整 Evidence；Journey 业务 gap 和技术 gap 不制造空 Evidence。Journey ledger/check result
-- 以 result_digest 闭合；operation 创建前的 identity_busy 只允许未派发 Queued Attempt。
CREATE TRIGGER trg_config_verification_run_check_results_closure BEFORE INSERT ON config_verification_run_check_results
WHEN NOT EXISTS (
  SELECT 1 FROM config_verification_runs t
  JOIN config_plans p ON p.config_version_id = t.config_version_id AND p.plan_key = NEW.plan_key
  JOIN config_checks c ON c.plan_id = p.id AND c.check_key = NEW.check_key
  WHERE t.id = NEW.verification_run_id AND t.state = 'Running' AND (
    (c.kind = 'promql' AND NEW.attempt_id IS NOT NULL AND NEW.result_digest IS NOT NULL AND EXISTS (
      SELECT 1 FROM execution_attempts a WHERE a.id=NEW.attempt_id
        AND a.attempt_type='inspection_collection' AND a.scope_type='config_verification_run'
        AND a.scope_id=NEW.verification_run_id AND a.plan_key=NEW.plan_key AND a.check_key=NEW.check_key
    ) AND (
      (NEW.status = 'ok' AND EXISTS (
        SELECT 1 FROM evidence e WHERE e.id = NEW.evidence_id AND e.attempt_id = NEW.attempt_id
          AND e.target_type = 'config_verification_run' AND e.target_id = NEW.verification_run_id AND e.integrity = 'complete'
          AND json_extract(e.params_json, '$.planKey') = NEW.plan_key
          AND json_extract(e.params_json, '$.checkKey') = NEW.check_key))
      OR (NEW.status IN ('error','gap') AND (NEW.evidence_id IS NULL OR EXISTS (
        SELECT 1 FROM evidence e WHERE e.id=NEW.evidence_id AND e.attempt_id=NEW.attempt_id
          AND e.target_type='config_verification_run' AND e.target_id=NEW.verification_run_id
      )))))
    OR (c.kind = 'promql' AND NEW.attempt_id IS NOT NULL AND NEW.result_digest IS NULL
      AND NEW.evidence_id IS NULL AND NEW.status IN ('error','gap') AND EXISTS (
        SELECT 1 FROM execution_attempts a WHERE a.id=NEW.attempt_id
          AND a.attempt_type='inspection_collection' AND a.scope_type='config_verification_run'
          AND a.scope_id=NEW.verification_run_id AND a.plan_key=NEW.plan_key AND a.check_key=NEW.check_key
          AND a.state IN ('Failed','Cancelled','Interrupted')
      ))
    OR (c.kind = 'browser' AND NEW.attempt_id IS NOT NULL AND EXISTS (
      SELECT 1 FROM execution_attempts a
      WHERE a.id = NEW.attempt_id AND a.attempt_type = 'inspection_collection'
        AND a.scope_type = 'config_verification_run' AND a.scope_id = NEW.verification_run_id
        AND a.plan_key = NEW.plan_key AND a.check_key = NEW.check_key
        AND (
          (NEW.result_digest IS NOT NULL AND (
            EXISTS (SELECT 1 FROM browser_journey_results j
              WHERE j.attempt_id = a.id AND j.result_digest = NEW.result_digest
                AND ((j.outcome = 'success' AND NEW.status = 'ok' AND NEW.gap_reason IS NULL
                      AND NEW.evidence_id = j.primary_evidence_id AND EXISTS (
                        SELECT 1 FROM evidence e WHERE e.id = j.primary_evidence_id AND e.attempt_id = a.id
                          AND e.target_type = 'config_verification_run' AND e.target_id = NEW.verification_run_id AND e.integrity = 'complete'
                          AND e.result_json IS NOT NULL AND e.artifact_id IS NULL
                          AND json_extract(e.params_json, '$.plan_key') = NEW.plan_key
                          AND json_extract(e.params_json, '$.check_key') = NEW.check_key))
                  OR (j.outcome = 'gap' AND NEW.status = 'gap' AND NEW.gap_reason = j.gap_code
                      AND NEW.evidence_id IS NULL AND j.primary_evidence_id IS NULL)))
            OR (a.state = 'Queued' AND a.runtime_slot IS NULL AND NEW.status = 'gap'
              AND NEW.gap_reason = 'identity_busy' AND NEW.evidence_id IS NULL
              AND NOT EXISTS (SELECT 1 FROM browser_operations o WHERE o.owner_attempt_id = a.id)
              AND EXISTS (
                SELECT 1 FROM config_verification_runs tr
                JOIN browser_identities bi ON bi.business_system_id = tr.business_system_id
                JOIN browser_operations busy ON busy.identity_id = bi.id AND busy.stop_confirmed_at IS NULL
                WHERE tr.id = a.scope_id))
          ))
          OR (NEW.result_digest IS NULL AND NEW.evidence_id IS NULL AND NEW.status IN ('error','gap')
            AND a.state IN ('Failed','Cancelled','Interrupted'))
        )
    ))
  )
)
OR (NEW.evidence_id IS NOT NULL AND EXISTS (
  SELECT 1 FROM config_verification_run_check_results r WHERE r.evidence_id = NEW.evidence_id))
BEGIN SELECT RAISE(ABORT, 'config_verification_run result must be one exact PromQL result, an atomically committed Journey ResultProposal, or a terminal technical gap'); END;
CREATE TRIGGER trg_config_verification_run_local_journey_result AFTER INSERT ON config_verification_run_check_results
WHEN NEW.result_digest IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM browser_journey_results j WHERE j.attempt_id = NEW.attempt_id)
BEGIN
  UPDATE execution_attempts
  SET state = 'Succeeded', ended_at = NEW.created_at, row_version = row_version + 1
  WHERE id = NEW.attempt_id AND state = 'Queued' AND runtime_slot IS NULL;
END;
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
  -- 6) 逐项闭合：config 属于该系统、未发布、以被激活契约为目标；Config Verification Run Passed；并发前提匹配。
  --    使用 json_each 遍历 items_json 中的每个 item 进行重验。
  SELECT RAISE(ABORT, 'activation item validation failed')
  WHERE EXISTS (
    SELECT 1 FROM json_each(NEW.items_json) je
    WHERE NOT EXISTS (
      SELECT 1
      FROM business_systems bs
      JOIN business_system_config_versions v ON v.id = CAST(je.value ->> '$.config_version_id' AS INTEGER)
      JOIN config_verification_runs t ON t.id = CAST(je.value ->> '$.verification_run_id' AS INTEGER)
      WHERE bs.id = CAST(je.value ->> '$.business_system_id' AS INTEGER)
        AND v.business_system_id = bs.id
        AND v.published_at IS NULL
        AND v.label_contract_version_id = NEW.contract_id
        AND t.business_system_id = bs.id
        AND t.config_version_id = v.id
        AND t.label_contract_version_id = NEW.contract_id
        AND t.purpose = 'prepublish' AND t.verification_manifest_item_id IS NULL
        AND t.state = 'Passed'
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
    timezone = (SELECT timezone FROM business_system_config_versions v WHERE v.id = CAST(je.value ->> '$.config_version_id' AS INTEGER)),
    resource_refresh_interval_seconds = (SELECT resource_refresh_interval_seconds FROM business_system_config_versions v WHERE v.id = CAST(je.value ->> '$.config_version_id' AS INTEGER))
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
-- 12.39 巡检运行闭合：正式 Run 只消费该启用系统的当前已发布配置与当前 Label Contract。
CREATE TRIGGER trg_inspection_runs_closure BEFORE INSERT ON inspection_runs
WHEN NOT EXISTS (
  SELECT 1 FROM business_systems b
  JOIN business_system_config_versions v ON v.id = b.current_config_version_id AND v.business_system_id = b.id
  JOIN config_plans p ON p.config_version_id = v.id AND p.plan_key = NEW.plan_key
  JOIN label_contract_state s ON s.id = 1 AND s.current_contract_id = NEW.label_contract_version_id
  WHERE b.id = NEW.business_system_id AND b.enabled = 1
    AND v.id = NEW.config_version_id AND v.state = 'published' AND v.published_at IS NOT NULL
    AND v.label_contract_version_id = NEW.label_contract_version_id
)
BEGIN SELECT RAISE(ABORT, 'inspection_run must bind the enabled business system current published config, configured plan, and current label contract'); END;

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
   OR (NEW.scope_type = 'config_verification_run' AND NOT EXISTS (
        SELECT 1 FROM config_verification_runs t JOIN config_plans p ON p.config_version_id = t.config_version_id AND p.plan_key = NEW.plan_key
        JOIN config_checks c ON c.plan_id = p.id
        WHERE t.id = NEW.scope_id AND t.state = 'Running'
          AND c.check_key = NEW.check_key AND c.kind = 'promql'
          AND NEW.plan_key IS NOT NULL AND NEW.check_key IS NOT NULL))
   OR (NEW.scope_type = 'resource_refresh_run' AND NOT EXISTS (
        SELECT 1 FROM resource_refresh_runs r JOIN config_discoveries d ON d.config_version_id=r.config_version_id AND d.discovery_key=NEW.discovery_key
        WHERE r.id=NEW.scope_id AND r.state IN ('Queued','Running') AND NEW.discovery_key IS NOT NULL))
   OR (NEW.scope_type = 'run_check' AND NOT EXISTS (
        SELECT 1 FROM inspection_runs r JOIN config_plans p ON p.config_version_id = r.config_version_id AND p.plan_key = r.plan_key
        JOIN config_checks c ON c.plan_id = p.id
        WHERE r.id = NEW.scope_id AND r.state = 'Running'
          AND c.check_key = NEW.check_key AND c.kind = 'browser' AND NEW.check_key IS NOT NULL))
BEGIN SELECT RAISE(ABORT, 'execution_attempt scope must reference the active object required by its fixed work mode'); END;

CREATE TRIGGER trg_browser_exploration_parent_tool BEFORE INSERT ON execution_attempts
WHEN NEW.attempt_type = 'browser_exploration' AND NOT EXISTS (
  SELECT 1 FROM tool_calls t JOIN execution_attempts parent ON parent.id = t.attempt_id
  WHERE t.id = NEW.requested_by_tool_call_id
    AND t.execution_mode = 'quoin_browser'
    AND parent.attempt_type = 'investigation'
    AND parent.scope_type = 'investigation' AND parent.scope_id = NEW.scope_id
)
BEGIN SELECT RAISE(ABORT, 'browser exploration must be requested by a quoin_browser Tool Call in the same investigation'); END;

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
        WHEN 'config_verification_run' THEN 'config_verification_execution_v1'
        WHEN 'resource_refresh_run' THEN 'resource_refresh_execution_v1'
        ELSE 'inspection_collection_v1'
      END
      WHEN 'browser_exploration' THEN 'browser_exploration_v1'
      WHEN 'connection_probe' THEN 'connection_probe_v1'
    END)
BEGIN SELECT RAISE(ABORT, 'attempt input snapshot schema_kind must match the versioned schema of the same Queued Attempt type'); END;
CREATE TRIGGER trg_attempt_input_item_closure BEFORE INSERT ON attempt_input_items
WHEN NOT EXISTS (
  SELECT 1 FROM attempt_input_snapshots s JOIN execution_attempts a ON a.id = s.attempt_id
  WHERE s.id = NEW.snapshot_id AND a.state = 'Queued'
    AND (NEW.connection_revision_id IS NULL OR (a.attempt_type = 'connection_probe' AND EXISTS (
      SELECT 1 FROM connection_revisions r WHERE r.id = NEW.connection_revision_id AND r.connection_id = a.scope_id))))
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
          OR (c.type = 'thanos'
            AND EXISTS (SELECT 1 FROM attempt_connection_grants g WHERE g.attempt_id = NEW.id AND g.connection_id = c.id AND g.purpose = 'thanos_probe'))
          OR (c.type = 'kubernetes'
            AND EXISTS (SELECT 1 FROM attempt_connection_grants g WHERE g.attempt_id = NEW.id AND g.connection_id = c.id AND g.purpose = 'kubernetes_probe'))
        )
      )))
  OR (NEW.runtime_slot = 'lintel' AND NEW.agent_version IS NOT NULL)
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
          WHEN 'thanos' THEN 'thanos_probe'
          WHEN 'kubernetes' THEN 'kubernetes_probe'
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
        OR (p.connection_type = 'thanos' AND EXISTS (
              SELECT 1 FROM thanos_connection_probe_results x WHERE x.probe_result_id = p.id))
        OR (p.connection_type = 'kubernetes' AND EXISTS (
              SELECT 1 FROM kubernetes_connection_probe_results x WHERE x.probe_result_id = p.id)))
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
WHEN NOT EXISTS (SELECT 1 FROM connection_probe_results p WHERE p.id = NEW.probe_result_id AND p.connection_type = 'thanos')
BEGIN SELECT RAISE(ABORT, 'Thanos probe child must match a Thanos probe header'); END;
CREATE TRIGGER trg_kubernetes_connection_probe_results_closure BEFORE INSERT ON kubernetes_connection_probe_results
WHEN NOT EXISTS (SELECT 1 FROM connection_probe_results p WHERE p.id = NEW.probe_result_id AND p.connection_type = 'kubernetes')
BEGIN SELECT RAISE(ABORT, 'Kubernetes probe child must match a Kubernetes probe header'); END;
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
  JOIN model_provider_connection_probe_results m ON m.probe_result_id = p.id
  JOIN credential_generations g ON g.id = p.credential_generation_id
  JOIN root_key_state k ON k.id = 1 AND k.binding_revision = g.key_binding_revision
  WHERE c.id = NEW.connection_id AND c.type = 'model_provider' AND c.enabled = 0
    AND NEW.enabled_row_version = c.row_version + 1
    AND p.connection_revision_id = c.current_revision_id
    AND p.credential_generation_id = c.current_credential_generation_id
    AND p.root_binding_revision = k.binding_revision AND p.outcome = 'passed'
    AND m.streaming_supported = 1 AND m.native_tool_calling_supported = 1
    AND m.cancellation_observed = 1 AND m.usage_observed = 1 AND m.embedding_supported = 1)
BEGIN SELECT RAISE(ABORT, 'enable qualification must select a passed probe for the exact current model-provider binding'); END;
CREATE TRIGGER trg_connections_enable_requires_probe BEFORE UPDATE OF enabled, current_revision_id, current_credential_generation_id ON connections
WHEN NEW.type = 'model_provider' AND NEW.enabled = 1 AND (
  OLD.enabled <> 0 OR NEW.current_revision_id IS NULL OR NEW.current_credential_generation_id IS NULL OR NOT EXISTS (
    SELECT 1 FROM connection_enable_qualifications q
    WHERE q.connection_id = NEW.id AND q.enabled_row_version = NEW.row_version))
BEGIN SELECT RAISE(ABORT, 'model_provider enable must atomically append an explicit immutable qualification event'); END;
CREATE TRIGGER trg_business_system_kubernetes_connection_type BEFORE INSERT ON business_system_kubernetes_connections
WHEN NOT EXISTS (SELECT 1 FROM connections c WHERE c.id = NEW.connection_id AND c.type = 'kubernetes')
BEGIN SELECT RAISE(ABORT, 'business system binding requires a kubernetes connection'); END;
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
          OR (c.type = 'thanos' AND NEW.purpose = 'thanos_probe')
          OR (c.type = 'kubernetes' AND NEW.purpose = 'kubernetes_probe')))
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
        OR (NEW.purpose = 'thanos_query' AND c.type = 'thanos' AND NEW.qualified_probe_result_id IS NULL)
        OR (NEW.purpose = 'config_thanos_query' AND c.type = 'thanos' AND NEW.qualified_probe_result_id IS NULL
            AND a.attempt_type = 'inspection_collection' AND a.scope_type IN ('config_verification_run','resource_refresh_run'))
        OR (NEW.purpose = 'kubernetes_read' AND c.type = 'kubernetes' AND NEW.qualified_probe_result_id IS NULL
          AND EXISTS (SELECT 1 FROM business_system_kubernetes_connections map
                      WHERE map.business_system_id = NEW.business_system_id AND map.connection_id = c.id AND map.state = 'Active'))
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
    AND ((t.tool_name = 'thanos_query' AND g.purpose = 'thanos_query')
      OR (t.tool_name IN ('kubernetes_get','kubernetes_list','kubernetes_logs','kubernetes_events') AND g.purpose = 'kubernetes_read'))
)
BEGIN SELECT RAISE(ABORT, 'tool call connection grant must match the same Attempt and typed external tool'); END;

-- 成功终态不得掩盖尚未闭合的 Model/Tool/Browser 子执行；失败/取消/中断终态必须在同一
-- Attempt UPDATE 中取消它们，否则父 Attempt 已终态而调用或子执行仍可产生迟到有效结果。
CREATE TRIGGER trg_execution_attempts_close_browser_action_before_cancel
BEFORE UPDATE OF state ON execution_attempts
WHEN OLD.attempt_type = 'browser_exploration' AND NEW.state IN ('Cancelled','Interrupted')
BEGIN
  UPDATE browser_exploration_actions
  SET outcome = 'session_closed',
      error_code = CASE WHEN NEW.state = 'Cancelled' THEN 'Cancelled' ELSE 'ParentTerminated' END,
      ended_at = COALESCE(NEW.ended_at, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
  WHERE child_attempt_id = OLD.id AND outcome IS NULL;
END;
CREATE TRIGGER trg_execution_attempts_failed_browser_action_closed BEFORE UPDATE OF state ON execution_attempts
WHEN OLD.attempt_type = 'browser_exploration' AND NEW.state = 'Failed' AND NOT EXISTS (
  SELECT 1 FROM browser_exploration_actions a JOIN browser_operations o ON o.id = a.operation_id
  WHERE a.child_attempt_id = OLD.id AND a.outcome = 'session_closed'
    AND o.state IN ('Failed','Cancelled','Interrupted'))
BEGIN SELECT RAISE(ABORT, 'failed browser action attempt requires a fatal action result and terminal exploration session'); END;
-- quoin_browser Tool Call 是浏览器子 Attempt 的提交入口：父 Tool Result 先写入，AFTER trigger 在同一 statement
-- 原子推进子 Attempt，避免“子 Attempt 已终态但父 Tool Call 仍 Running”的可提交分裂状态。
CREATE TRIGGER trg_tool_calls_close_browser_child AFTER UPDATE OF status ON tool_calls
WHEN OLD.status <> NEW.status AND NEW.execution_mode = 'quoin_browser'
  AND NEW.status IN ('succeeded','failed','cancelled')
BEGIN
  UPDATE execution_attempts
  SET state = CASE
        WHEN NEW.status = 'succeeded' THEN 'Succeeded'
        WHEN NEW.status = 'failed' THEN 'Failed'
        ELSE CASE WHEN state IN ('Running','Cancelling') THEN 'Cancelling' ELSE 'Cancelled' END
      END,
      termination_reason = CASE
        WHEN NEW.status = 'failed' THEN 'tool_error'
        WHEN NEW.status = 'cancelled' AND state IN ('Queued','Assigned') THEN 'cancelled'
        ELSE termination_reason
      END,
      ended_at = CASE
        WHEN NEW.status IN ('succeeded','failed') OR state IN ('Queued','Assigned') THEN NEW.ended_at
        ELSE ended_at
      END,
      row_version = row_version + 1
  WHERE requested_by_tool_call_id = NEW.id
    AND state IN ('Queued','Assigned','Running','Cancelling');
END;
CREATE TRIGGER trg_execution_attempts_success_requires_closed_calls BEFORE UPDATE OF state ON execution_attempts
WHEN NEW.state = 'Succeeded' AND OLD.state <> 'Succeeded' AND (
  EXISTS (SELECT 1 FROM model_calls mc WHERE mc.attempt_id = NEW.id AND mc.status = 'running')
  OR EXISTS (SELECT 1 FROM tool_calls tc WHERE tc.attempt_id = NEW.id AND tc.status IN ('pending','running'))
  OR EXISTS (
    SELECT 1 FROM execution_attempts child
    JOIN tool_calls tc ON tc.id = child.requested_by_tool_call_id
    WHERE tc.attempt_id = NEW.id AND child.state IN ('Queued','Assigned','Running','Cancelling'))
  OR (NEW.attempt_type = 'investigation' AND EXISTS (
    SELECT 1 FROM browser_operations bo WHERE bo.owner_attempt_id = NEW.id
      AND bo.kind = 'exploration' AND bo.state IN ('Queued','WaitingForCapacity','Starting','Running')))
  OR (NEW.attempt_type = 'browser_exploration' AND NOT EXISTS (
      SELECT 1 FROM tool_calls tc
      WHERE tc.id = NEW.requested_by_tool_call_id AND tc.execution_mode = 'quoin_browser' AND tc.status = 'succeeded'))
  OR (NEW.attempt_type = 'browser_exploration'
    AND NOT (OLD.state = 'Queued' AND OLD.requested_by_tool_call_id IS NOT NULL AND OLD.runtime_slot IS NULL)
    AND NOT EXISTS (
      SELECT 1 FROM browser_exploration_actions ba WHERE ba.child_attempt_id = NEW.id AND ba.outcome IS NOT NULL))
  OR (NEW.attempt_type = 'browser_exploration' AND EXISTS (
    SELECT 1 FROM browser_exploration_actions ba JOIN browser_operations bo ON bo.id = ba.operation_id
    WHERE ba.child_attempt_id = NEW.id AND ba.action_kind = 'close_session'
      AND NOT (ba.outcome = 'success' AND bo.state = 'Succeeded')))
  OR (NEW.attempt_type = 'inspection_collection' AND NEW.scope_type IN ('run_check','config_verification_run') AND NOT EXISTS (
    SELECT 1 FROM inspection_check_results r
    WHERE NEW.scope_type = 'run_check' AND r.run_id = NEW.scope_id AND r.check_key = NEW.check_key
      AND r.attempt_id = NEW.id AND r.result_digest IS NOT NULL
    UNION ALL
    SELECT 1 FROM config_verification_run_check_results r
    WHERE NEW.scope_type = 'config_verification_run' AND r.verification_run_id = NEW.scope_id
      AND r.plan_key = NEW.plan_key AND r.check_key = NEW.check_key
      AND r.attempt_id = NEW.id AND r.result_digest IS NOT NULL))
  OR (NEW.attempt_type = 'inspection_collection' AND NEW.scope_type = 'resource_refresh_run' AND NOT EXISTS (
    SELECT 1 FROM observed_refresh_log l
    WHERE l.attempt_id = NEW.id AND l.result_digest IS NOT NULL))
)
BEGIN SELECT RAISE(ABORT, 'Succeeded Attempt requires all Model Calls, Tool Calls, and browser child Attempts terminal'); END;
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
  UPDATE execution_attempts
  SET state = CASE WHEN state IN ('Running','Cancelling') THEN 'Cancelling' ELSE 'Cancelled' END,
      row_version = row_version + 1,
      termination_reason = CASE WHEN state IN ('Queued','Assigned') THEN 'cancelled' ELSE termination_reason END,
      ended_at = CASE WHEN state IN ('Queued','Assigned') THEN COALESCE(NEW.ended_at, strftime('%Y-%m-%dT%H:%M:%fZ','now')) ELSE ended_at END
  WHERE requested_by_tool_call_id IN (SELECT id FROM tool_calls WHERE attempt_id = NEW.id)
    AND state IN ('Queued','Assigned','Running');
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
  OR (NEW.attempt_type = 'connection_probe' AND NOT EXISTS (
      SELECT 1 FROM connection_probe_results p
      WHERE p.attempt_id = NEW.id AND p.connection_id = NEW.scope_id
        AND ((p.connection_type = 'model_provider' AND EXISTS (SELECT 1 FROM model_provider_connection_probe_results m WHERE m.probe_result_id = p.id))
          OR (p.connection_type = 'thanos' AND EXISTS (SELECT 1 FROM thanos_connection_probe_results t WHERE t.probe_result_id = p.id))
          OR (p.connection_type = 'kubernetes' AND EXISTS (SELECT 1 FROM kubernetes_connection_probe_results k WHERE k.probe_result_id = p.id)))))
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
-- 普通巡检结果只在 Running 阶段追加并闭合到精确 check/Evidence 来源。
-- 普通巡检结果只在 Running 阶段追加并闭合到精确 check/Evidence 来源。PromQL ok 与 Journey
-- success 引用唯一完整 Evidence；Journey 业务 gap 和技术 gap 不制造空 Evidence。
CREATE TRIGGER trg_inspection_check_results_closure BEFORE INSERT ON inspection_check_results
WHEN NOT EXISTS (
  SELECT 1 FROM inspection_runs r
  JOIN config_plans p ON p.config_version_id = r.config_version_id AND p.plan_key = r.plan_key
  JOIN config_checks c ON c.plan_id = p.id AND c.check_key = NEW.check_key
  WHERE r.id = NEW.run_id AND r.state = 'Running' AND (
    (c.kind = 'promql' AND NEW.attempt_id IS NULL AND NEW.result_digest IS NULL AND (
      (NEW.status = 'ok' AND EXISTS (
        SELECT 1 FROM evidence e WHERE e.id = NEW.evidence_id AND e.attempt_id IS NULL
          AND e.target_type = 'inspection_run' AND e.target_id = NEW.run_id AND e.integrity = 'complete'
          AND json_extract(e.params_json, '$.check_key') = NEW.check_key))
      OR (NEW.status IN ('error','gap') AND NEW.evidence_id IS NULL)))
    OR (c.kind = 'browser' AND NEW.attempt_id IS NOT NULL AND EXISTS (
      SELECT 1 FROM execution_attempts a
      WHERE a.id = NEW.attempt_id AND a.attempt_type = 'inspection_collection'
        AND a.scope_type = 'run_check' AND a.scope_id = NEW.run_id AND a.check_key = NEW.check_key
        AND (
          (NEW.result_digest IS NOT NULL AND (
            EXISTS (SELECT 1 FROM browser_journey_results j
              WHERE j.attempt_id = a.id AND j.result_digest = NEW.result_digest
                AND ((j.outcome = 'success' AND NEW.status = 'ok' AND NEW.gap_reason IS NULL
                      AND NEW.evidence_id = j.primary_evidence_id AND EXISTS (
                        SELECT 1 FROM evidence e WHERE e.id = j.primary_evidence_id AND e.attempt_id = a.id
                          AND e.target_type = 'inspection_run' AND e.target_id = NEW.run_id AND e.integrity = 'complete'
                          AND e.result_json IS NOT NULL AND e.artifact_id IS NULL
                          AND json_extract(e.params_json, '$.check_key') = NEW.check_key))
                  OR (j.outcome = 'gap' AND NEW.status = 'gap' AND NEW.gap_reason = j.gap_code
                      AND NEW.evidence_id IS NULL AND j.primary_evidence_id IS NULL)))
            OR (a.state = 'Queued' AND a.runtime_slot IS NULL AND NEW.status = 'gap'
              AND NEW.gap_reason = 'identity_busy' AND NEW.evidence_id IS NULL
              AND NOT EXISTS (SELECT 1 FROM browser_operations o WHERE o.owner_attempt_id = a.id)
              AND EXISTS (
                SELECT 1 FROM inspection_runs ir
                JOIN browser_identities bi ON bi.business_system_id = ir.business_system_id
                JOIN browser_operations busy ON busy.identity_id = bi.id AND busy.stop_confirmed_at IS NULL
                WHERE ir.id = a.scope_id))
          ))
          OR (NEW.result_digest IS NULL AND NEW.evidence_id IS NULL AND NEW.status IN ('error','gap')
            AND a.state IN ('Failed','Cancelled','Interrupted'))
        )
    ))
  )
)
OR (NEW.evidence_id IS NOT NULL AND EXISTS (
  SELECT 1 FROM inspection_check_results r WHERE r.evidence_id = NEW.evidence_id))
BEGIN SELECT RAISE(ABORT, 'inspection result must be one exact PromQL result, an atomically committed Journey ResultProposal, or a terminal technical gap'); END;
CREATE TRIGGER trg_inspection_local_journey_result AFTER INSERT ON inspection_check_results
WHEN NEW.result_digest IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM browser_journey_results j WHERE j.attempt_id = NEW.attempt_id)
BEGIN
  UPDATE execution_attempts
  SET state = 'Succeeded', ended_at = NEW.created_at, row_version = row_version + 1
  WHERE id = NEW.attempt_id AND state = 'Queued' AND runtime_slot IS NULL;
END;
CREATE TRIGGER trg_config_discoveries_parent_frozen BEFORE INSERT ON config_discoveries
WHEN NOT EXISTS (
  SELECT 1 FROM business_system_config_versions v
  WHERE v.id = NEW.config_version_id AND v.state = 'draft' AND v.published_at IS NULL
    AND NOT EXISTS (SELECT 1 FROM config_verification_runs t WHERE t.config_version_id = NEW.config_version_id)
    AND NOT EXISTS (SELECT 1 FROM inspection_runs r WHERE r.config_version_id = NEW.config_version_id)
)
BEGIN SELECT RAISE(ABORT, 'config_discoveries can only be inserted while parent config is draft with no config verification runs, no publications, and no inspection runs'); END;
CREATE TRIGGER trg_config_plans_parent_frozen BEFORE INSERT ON config_plans
WHEN NOT EXISTS (
  SELECT 1 FROM business_system_config_versions v
  WHERE v.id = NEW.config_version_id AND v.state = 'draft' AND v.published_at IS NULL
    AND NOT EXISTS (SELECT 1 FROM config_verification_runs t WHERE t.config_version_id = NEW.config_version_id)
    AND NOT EXISTS (SELECT 1 FROM inspection_runs r WHERE r.config_version_id = NEW.config_version_id)
)
BEGIN SELECT RAISE(ABORT, 'config_plans can only be inserted while parent config is draft with no config verification runs, no publications, and no inspection runs'); END;
CREATE TRIGGER trg_config_checks_parent_frozen BEFORE INSERT ON config_checks
WHEN NOT EXISTS (
  SELECT 1 FROM config_plans p JOIN business_system_config_versions v ON v.id = p.config_version_id
  WHERE p.id = NEW.plan_id AND v.state = 'draft' AND v.published_at IS NULL
    AND NOT EXISTS (SELECT 1 FROM config_verification_runs t WHERE t.config_version_id = v.id)
    AND NOT EXISTS (SELECT 1 FROM inspection_runs r WHERE r.config_version_id = v.id)
)
BEGIN SELECT RAISE(ABORT, 'config_checks can only be inserted while parent config is draft with no config verification runs, no publications, and no inspection runs'); END;
-- check_key 只在其 plan 父作用域内唯一；config_verification_run 以 plan_key+check_key 复合定位，
-- 因而不同 plan 可合法复用同一 check_key（DATA-CONFIG-004）。
CREATE TRIGGER trg_config_discoveries_identity_labels_unique BEFORE INSERT ON config_discoveries
WHEN (SELECT COUNT(*) FROM json_each(NEW.identity_labels_json)) <> (SELECT COUNT(DISTINCT value) FROM json_each(NEW.identity_labels_json))
BEGIN SELECT RAISE(ABORT, 'identity_labels must not contain duplicates'); END;
-- 12.42 配置验证/资源刷新 Run 纳入任务变更日志（DATA-SSE-004）：与权威状态同一事务派生。
CREATE TRIGGER trg_task_change_log_config_verification_run_insert AFTER INSERT ON config_verification_runs
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('config_verification_run', NEW.id, 'created', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_config_verification_run_state AFTER UPDATE OF state ON config_verification_runs
WHEN NEW.state <> OLD.state
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('config_verification_run', NEW.id, 'state_changed', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_resource_refresh_run_insert AFTER INSERT ON resource_refresh_runs
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('resource_refresh_run', NEW.id, 'created', NEW.row_version);
END;
CREATE TRIGGER trg_task_change_log_resource_refresh_run_state AFTER UPDATE OF state ON resource_refresh_runs
WHEN NEW.state <> OLD.state
BEGIN
  INSERT INTO task_change_log (object_type, object_id, change_type, row_version)
  VALUES ('resource_refresh_run', NEW.id, 'state_changed', NEW.row_version);
END;


-- 12.43 Deployment Acceptance 不可变闭包与 finalize receipt。
CREATE TRIGGER trg_verification_manifest_admin_session BEFORE INSERT ON verification_invocation_manifests
WHEN NOT EXISTS (
  SELECT 1 FROM sessions s JOIN users u ON u.id = s.user_id
  WHERE s.id = NEW.admin_session_id AND s.user_id = NEW.principal_user_id
    AND s.revoked_at IS NULL AND u.role = 'admin' AND u.enabled = 1
    AND s.auth_revision_at_issue = u.auth_revision
    AND julianday(NEW.created_at) < julianday(s.idle_expires_at)
    AND julianday(NEW.created_at) < julianday(s.absolute_expires_at))
BEGIN SELECT RAISE(ABORT, 'verification manifest requires the initiating active Admin Session'); END;

CREATE TRIGGER trg_verification_items_manifest_open BEFORE INSERT ON verification_invocation_items
WHEN EXISTS (SELECT 1 FROM verification_finalization_receipts r WHERE r.invocation_id = NEW.invocation_id)
  OR NOT EXISTS (
    SELECT 1 FROM verification_invocation_manifests m
    WHERE m.id = NEW.invocation_id AND NEW.item_seq <= m.item_count
      AND (SELECT COUNT(*) FROM verification_invocation_items i WHERE i.invocation_id = m.id) < m.item_count)
BEGIN SELECT RAISE(ABORT, 'verification manifest item set is closed by its immutable item_count'); END;
CREATE TRIGGER trg_verification_results_manifest_open BEFORE INSERT ON verification_item_results
WHEN EXISTS (
  SELECT 1 FROM verification_invocation_items i
  JOIN verification_finalization_receipts r ON r.invocation_id = i.invocation_id
  WHERE i.id = NEW.item_id)
  OR NOT EXISTS (
    SELECT 1 FROM verification_invocation_items i
    JOIN verification_invocation_manifests m ON m.id = i.invocation_id
    WHERE i.id = NEW.item_id
      AND (SELECT COUNT(*) FROM verification_invocation_items all_items WHERE all_items.invocation_id = m.id) = m.item_count)
BEGIN SELECT RAISE(ABORT, 'verification results require the complete immutable manifest item set and no final receipt'); END;
CREATE TRIGGER trg_verification_result_input_closure BEFORE INSERT ON verification_item_results
WHEN NOT EXISTS (SELECT 1 FROM verification_invocation_items i WHERE i.id = NEW.item_id AND i.input_digest = NEW.input_digest)
BEGIN SELECT RAISE(ABORT, 'verification result input digest must match its frozen manifest item'); END;
CREATE TRIGGER trg_verification_result_artifact_closure BEFORE INSERT ON verification_item_results
WHEN NEW.artifact_id IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM verification_invocation_items i
  JOIN artifacts a ON a.id = NEW.artifact_id AND a.kind = 'verification_attachment'
    AND a.owner_type = 'verification_invocation' AND a.owner_id = i.invocation_id
    AND a.retention_kind = 'long_term' AND a.body_expired = 0
  JOIN artifact_blobs b ON b.id = a.blob_id AND b.sha256 = NEW.result_digest
  WHERE i.id = NEW.item_id)
BEGIN SELECT RAISE(ABORT, 'verification result artifact must be its long-term canonical Test Result under the same invocation'); END;
CREATE TRIGGER trg_verification_result_deadline_closure BEFORE INSERT ON verification_item_results
WHEN NOT EXISTS (
  SELECT 1 FROM verification_invocation_items i
  JOIN verification_invocation_manifests m ON m.id = i.invocation_id
  WHERE i.id = NEW.item_id
    AND julianday(NEW.observed_at) >= julianday(m.started_at)
    AND julianday(NEW.observed_at) <= julianday(m.deadline_at)
    AND julianday(NEW.committed_at) >= julianday(m.started_at)
    AND julianday(NEW.committed_at) <= julianday(m.deadline_at))
BEGIN SELECT RAISE(ABORT, 'verification result is outside the fixed eight-hour point-in-time closure'); END;
CREATE TRIGGER trg_verification_result_conflict_record AFTER INSERT ON verification_item_results
BEGIN
  INSERT OR IGNORE INTO verification_result_conflicts (item_id, first_result_id, conflicting_result_id, created_at)
  SELECT NEW.item_id, prior.id, NEW.id, NEW.committed_at
  FROM verification_item_results prior
  WHERE prior.item_id = NEW.item_id AND prior.id < NEW.id AND prior.result_digest <> NEW.result_digest;
END;
CREATE TRIGGER trg_verification_result_conflict_closure BEFORE INSERT ON verification_result_conflicts
WHEN NOT EXISTS (
  SELECT 1 FROM verification_item_results a
  JOIN verification_item_results b ON b.id = NEW.conflicting_result_id
  JOIN verification_invocation_items i ON i.id = NEW.item_id
  WHERE a.id = NEW.first_result_id AND a.item_id = NEW.item_id AND b.item_id = NEW.item_id
    AND a.result_digest <> b.result_digest
    AND NOT EXISTS (SELECT 1 FROM verification_finalization_receipts r WHERE r.invocation_id = i.invocation_id))
BEGIN SELECT RAISE(ABORT, 'verification conflict must bind two different results of the same open invocation item'); END;

CREATE TRIGGER trg_verification_helper_import_closure BEFORE INSERT ON verification_helper_imports
WHEN NOT EXISTS (
  SELECT 1 FROM verification_invocation_manifests m
  JOIN artifacts a ON a.id = NEW.artifact_id AND a.kind = 'verification_attachment'
    AND a.owner_type = 'verification_invocation' AND a.owner_id = m.id
    AND a.retention_kind = 'long_term' AND a.body_expired = 0
  JOIN artifact_blobs b ON b.id = a.blob_id AND b.sha256 = NEW.report_digest
  WHERE m.id = NEW.invocation_id AND m.canonical_input_digest = NEW.request_digest
    AND julianday(NEW.received_at) >= julianday(m.started_at)
    AND julianday(NEW.received_at) <= julianday(m.deadline_at)
    AND NOT EXISTS (SELECT 1 FROM verification_finalization_receipts r WHERE r.invocation_id = m.id))
BEGIN SELECT RAISE(ABORT, 'helper import must bind the open manifest request and its exact long-term report artifact'); END;

CREATE TRIGGER trg_verification_subject_drift_closure BEFORE INSERT ON verification_subject_drifts
WHEN NOT EXISTS (
  SELECT 1 FROM verification_invocation_items i
  JOIN verification_invocation_manifests m ON m.id = i.invocation_id
  WHERE i.id = NEW.item_id AND i.invocation_id = NEW.invocation_id AND i.object_kind = NEW.object_kind
    AND julianday(NEW.observed_at) >= julianday(m.started_at)
    AND julianday(NEW.observed_at) <= julianday(m.deadline_at)
    AND NOT EXISTS (SELECT 1 FROM verification_finalization_receipts r WHERE r.invocation_id = m.id))
BEGIN SELECT RAISE(ABORT, 'subject drift must bind one matching item in an open invocation observation window'); END;

CREATE TRIGGER trg_verification_deployment_locator_kind BEFORE INSERT ON verification_deployment_item_locators
WHEN NOT EXISTS (SELECT 1 FROM verification_invocation_items i WHERE i.id = NEW.item_id AND i.object_kind = 'deployment')
BEGIN SELECT RAISE(ABORT, 'deployment locator requires a deployment item'); END;
CREATE TRIGGER trg_verification_connection_locator_kind BEFORE INSERT ON verification_connection_item_locators
WHEN NOT EXISTS (
  SELECT 1 FROM verification_invocation_items i
  JOIN connection_revisions r ON r.id = NEW.connection_revision_id AND r.connection_id = NEW.connection_id
  JOIN credential_generations g ON g.id = NEW.credential_generation_id AND g.connection_id = NEW.connection_id
  WHERE i.id = NEW.item_id AND i.object_kind = 'connection' AND g.key_binding_revision = NEW.root_binding_revision)
BEGIN SELECT RAISE(ABORT, 'connection locator requires one exact connection binding'); END;
CREATE TRIGGER trg_verification_config_locator_kind BEFORE INSERT ON verification_config_item_locators
WHEN NOT EXISTS (
  SELECT 1 FROM verification_invocation_items i
  JOIN business_system_config_versions v ON v.id = NEW.config_version_id
  WHERE i.id = NEW.item_id AND i.object_kind = 'config' AND v.business_system_id = NEW.business_system_id
    AND v.label_contract_version_id = NEW.label_contract_version_id)
BEGIN SELECT RAISE(ABORT, 'config locator requires one exact config binding'); END;
CREATE TRIGGER trg_verification_browser_locator_kind BEFORE INSERT ON verification_browser_identity_item_locators
WHEN NOT EXISTS (
  SELECT 1 FROM verification_invocation_items i
  JOIN browser_identities b ON b.id = NEW.browser_identity_id
  JOIN browser_identity_revisions r ON r.id = NEW.identity_revision_id AND r.business_system_id = b.business_system_id
  JOIN browser_profile_generations g ON g.id = NEW.profile_generation_id AND g.identity_id = b.id
    AND g.identity_revision_id = r.id
  WHERE i.id = NEW.item_id AND i.object_kind = 'browser_identity')
BEGIN SELECT RAISE(ABORT, 'browser locator requires one exact identity revision/profile generation'); END;
CREATE TRIGGER trg_verification_observation_locator_kind BEFORE INSERT ON verification_ui_observation_item_locators
WHEN NOT EXISTS (SELECT 1 FROM verification_invocation_items i WHERE i.id = NEW.item_id AND i.object_kind = 'ui_observation')
BEGIN SELECT RAISE(ABORT, 'observation locator requires a ui_observation item'); END;

CREATE TRIGGER trg_verification_typed_observation_closure BEFORE INSERT ON verification_typed_observations
WHEN NOT EXISTS (
  SELECT 1 FROM verification_item_results r
  JOIN verification_invocation_items i ON i.id = r.item_id
  JOIN verification_invocation_manifests m ON m.id = i.invocation_id
  JOIN sessions s ON s.id = NEW.admin_session_id AND s.revoked_at IS NULL
  JOIN users u ON u.id = s.user_id AND u.enabled = 1 AND u.role = 'admin' AND s.auth_revision_at_issue = u.auth_revision
  WHERE r.id = NEW.result_id AND i.object_kind = 'ui_observation'
    AND r.producer_type = 'admin_observation' AND m.admin_session_id = NEW.admin_session_id
    AND julianday(NEW.submitted_at) < julianday(s.idle_expires_at)
    AND julianday(NEW.submitted_at) < julianday(s.absolute_expires_at)
    AND julianday(NEW.submitted_at) >= julianday(m.started_at)
    AND julianday(NEW.submitted_at) <= julianday(m.deadline_at)
    AND NOT EXISTS (SELECT 1 FROM verification_finalization_receipts fr WHERE fr.invocation_id = m.id)
    AND ((NEW.visual_result = 'passed' AND NEW.motion_result = 'passed' AND NEW.focus_occlusion_result = 'passed' AND r.outcome = 'passed')
      OR ((NEW.visual_result = 'failed' OR NEW.motion_result = 'failed' OR NEW.focus_occlusion_result = 'failed')
        AND r.outcome = 'failed' AND r.category = 'functional_assertion_failed')))
BEGIN SELECT RAISE(ABORT, 'typed observation must be submitted by the initiating Admin Session and match its result'); END;

CREATE TRIGGER trg_verification_finalization_closure BEFORE INSERT ON verification_finalization_receipts
WHEN NOT EXISTS (
  SELECT 1 FROM verification_invocation_manifests m
  JOIN artifacts a ON a.id = NEW.canonical_artifact_id AND a.kind = 'verification_bundle'
    AND a.owner_type = 'verification_invocation' AND a.owner_id = m.id
    AND a.retention_kind = 'long_term' AND a.body_expired = 0
  JOIN artifact_blobs b ON b.id = a.blob_id AND b.sha256 = NEW.final_result_digest
  WHERE m.id = NEW.invocation_id
    AND julianday(NEW.snapshot_at) >= julianday(m.started_at)
    AND julianday(NEW.snapshot_at) <= julianday(m.deadline_at)
    AND julianday(NEW.finalized_at) >= julianday(NEW.snapshot_at)
    AND julianday(NEW.finalized_at) <= julianday(m.deadline_at)
    AND julianday(a.created_at) <= julianday(NEW.finalized_at)
    AND ((NEW.finalized_by_type = 'initiating_admin_session' AND m.admin_session_id = NEW.finalized_by_session_id
        AND EXISTS (SELECT 1 FROM sessions s JOIN users u ON u.id = s.user_id
          WHERE s.id = NEW.finalized_by_session_id AND s.revoked_at IS NULL
            AND u.enabled = 1 AND u.role = 'admin' AND s.auth_revision_at_issue = u.auth_revision
            AND julianday(NEW.finalized_at) < julianday(s.idle_expires_at)
            AND julianday(NEW.finalized_at) < julianday(s.absolute_expires_at)))
      OR (NEW.finalized_by_type = 'system_deadline' AND NEW.finalized_by_session_id IS NULL
        AND julianday(NEW.finalized_at) <= julianday(m.deadline_at)))
    AND m.applicable_set_digest = NEW.applicable_set_digest
    AND m.manifest_digest = NEW.manifest_digest
    AND m.item_set_digest = NEW.item_set_digest
    AND NOT EXISTS (
      SELECT 1 FROM verification_invocation_items i
      WHERE i.invocation_id = m.id AND (
        NOT EXISTS (SELECT 1 FROM verification_item_results r WHERE r.item_id = i.id)
        OR (i.object_kind = 'deployment' AND NOT EXISTS (SELECT 1 FROM verification_deployment_item_locators l WHERE l.item_id = i.id))
        OR (i.object_kind = 'connection' AND NOT EXISTS (SELECT 1 FROM verification_connection_item_locators l WHERE l.item_id = i.id))
        OR (i.object_kind = 'config' AND NOT EXISTS (SELECT 1 FROM verification_config_item_locators l WHERE l.item_id = i.id))
        OR (i.object_kind = 'browser_identity' AND NOT EXISTS (SELECT 1 FROM verification_browser_identity_item_locators l WHERE l.item_id = i.id))
        OR (i.object_kind = 'ui_observation' AND (NOT EXISTS (SELECT 1 FROM verification_ui_observation_item_locators l WHERE l.item_id = i.id)
          OR EXISTS (
            SELECT 1 FROM verification_item_results r
            WHERE r.item_id = i.id AND r.category IN ('passed','functional_assertion_failed')
              AND NOT EXISTS (SELECT 1 FROM verification_typed_observations o WHERE o.result_id = r.id)
          )))
      ))
    AND NOT EXISTS (
      SELECT 1 FROM verification_invocation_items i
      JOIN verification_item_results r ON r.item_id = i.id
      WHERE i.invocation_id = m.id AND julianday(r.observed_at) > julianday(NEW.snapshot_at))
    AND NOT EXISTS (
      SELECT 1 FROM verification_helper_imports h
      WHERE h.invocation_id = m.id AND julianday(h.received_at) > julianday(NEW.snapshot_at))
    AND NOT EXISTS (
      SELECT 1 FROM verification_typed_observations o
      JOIN verification_item_results r ON r.id = o.result_id
      JOIN verification_invocation_items i ON i.id = r.item_id
      WHERE i.invocation_id = m.id AND julianday(o.submitted_at) > julianday(NEW.snapshot_at))
    AND NOT EXISTS (
      SELECT 1 FROM verification_subject_drifts d
      WHERE d.invocation_id = m.id AND julianday(d.observed_at) > julianday(NEW.snapshot_at))
    AND NOT EXISTS (
      SELECT 1 FROM verification_invocation_items i
      JOIN verification_item_results r ON r.item_id = i.id
      WHERE i.invocation_id = m.id AND r.category = 'subject_drift'
        AND NOT EXISTS (SELECT 1 FROM verification_subject_drifts d WHERE d.invocation_id = m.id AND d.item_id = i.id))
    AND (
      (NEW.overall_outcome = 'failed' AND (
        EXISTS (SELECT 1 FROM verification_invocation_items i JOIN verification_item_results r ON r.item_id = i.id WHERE i.invocation_id = m.id AND r.outcome = 'failed')
        OR EXISTS (SELECT 1 FROM verification_invocation_items i JOIN verification_result_conflicts c ON c.item_id = i.id WHERE i.invocation_id = m.id)))
      OR (NEW.overall_outcome = 'warned'
        AND NOT EXISTS (SELECT 1 FROM verification_invocation_items i JOIN verification_item_results r ON r.item_id = i.id WHERE i.invocation_id = m.id AND r.outcome = 'failed')
        AND NOT EXISTS (SELECT 1 FROM verification_invocation_items i JOIN verification_result_conflicts c ON c.item_id = i.id WHERE i.invocation_id = m.id)
        AND (EXISTS (SELECT 1 FROM verification_invocation_items i JOIN verification_item_results r ON r.item_id = i.id WHERE i.invocation_id = m.id AND r.outcome = 'warned')
          OR EXISTS (SELECT 1 FROM verification_subject_drifts d WHERE d.invocation_id = m.id)))
      OR (NEW.overall_outcome = 'passed'
        AND NOT EXISTS (SELECT 1 FROM verification_invocation_items i LEFT JOIN verification_item_results r ON r.item_id = i.id AND r.outcome = 'passed' WHERE i.invocation_id = m.id AND r.id IS NULL)
        AND NOT EXISTS (SELECT 1 FROM verification_invocation_items i JOIN verification_result_conflicts c ON c.item_id = i.id WHERE i.invocation_id = m.id)
        AND NOT EXISTS (SELECT 1 FROM verification_subject_drifts d WHERE d.invocation_id = m.id))
    )
)
BEGIN SELECT RAISE(ABORT, 'verification receipt requires complete typed items and deterministic severity aggregation'); END;

-- Browser deployment verification freezes the current identity generation into a clone and requires explicit cleanup evidence.
CREATE TRIGGER trg_browser_deployment_verification_insert_closure BEFORE INSERT ON browser_operations
WHEN NEW.kind = 'deployment_verification' AND NOT EXISTS (
  SELECT 1 FROM verification_invocation_items i
  JOIN verification_browser_identity_item_locators l ON l.item_id = i.id
  JOIN verification_invocation_manifests m ON m.id = i.invocation_id
  WHERE i.id = NEW.verification_manifest_item_id AND l.browser_identity_id = NEW.identity_id
    AND l.identity_revision_id = NEW.identity_revision_id AND l.profile_generation_id = NEW.profile_generation_id
    AND m.admin_session_id = NEW.actor_session_id
    AND julianday(NEW.requested_at) <= julianday(m.deadline_at)
    AND NOT EXISTS (SELECT 1 FROM verification_finalization_receipts fr WHERE fr.invocation_id = m.id))
BEGIN SELECT RAISE(ABORT, 'deployment browser verification must bind the manifest-frozen identity and initiating Admin Session'); END;
CREATE TRIGGER trg_browser_deployment_result_closure BEFORE INSERT ON browser_deployment_verification_results
WHEN NOT EXISTS (
  SELECT 1 FROM browser_operations o
  JOIN verification_item_results vr ON vr.id = NEW.verification_result_id
  JOIN verification_invocation_items vi ON vi.id = vr.item_id AND vi.id = o.verification_manifest_item_id
  WHERE o.id = NEW.operation_id AND o.kind = 'deployment_verification'
    AND o.clone_identity = NEW.clone_identity AND o.lintel_boot_id = NEW.original_boot_id
    AND ((NEW.functional_outcome IN ('passed','warned') AND o.state = 'Succeeded')
      OR (NEW.functional_outcome = 'failed' AND o.state = 'Failed'))
    AND (
      (NEW.cleanup_outcome = 'clean' AND NEW.cleanup_boot_id = NEW.original_boot_id
        AND o.stop_confirmed_at IS NOT NULL AND o.stop_confirmation_basis = 'same_boot_cleanup_ack'
        AND lower(hex(o.cleanup_state_hash)) = NEW.cleanup_state_hash)
      OR (NEW.cleanup_outcome = 'residue' AND NEW.cleanup_boot_id = NEW.original_boot_id
        AND (o.stop_confirmed_at IS NULL OR o.stop_confirmation_basis <> 'same_boot_cleanup_ack'))
      OR (NEW.cleanup_outcome = 'indeterminate'
        AND (o.stop_confirmed_at IS NULL OR o.stop_confirmation_basis <> 'same_boot_cleanup_ack')))
    AND (
      (NEW.cleanup_outcome = 'residue' AND vr.outcome = 'failed' AND vr.category = 'cleanup_residue')
      OR (NEW.cleanup_outcome <> 'residue' AND NEW.functional_outcome = 'failed'
        AND vr.outcome = 'failed' AND vr.category = 'functional_assertion_failed')
      OR (NEW.cleanup_outcome = 'indeterminate' AND NEW.functional_outcome <> 'failed'
        AND vr.outcome = 'warned' AND vr.category = 'cleanup_indeterminate')
      OR (NEW.cleanup_outcome = 'clean' AND NEW.functional_outcome = 'passed'
        AND vr.outcome = 'passed' AND vr.category = 'passed')
      OR (NEW.cleanup_outcome = 'clean' AND NEW.functional_outcome = 'warned'
        AND vr.outcome = 'warned' AND vr.category IN ('environment_unavailable','infrastructure_interrupted'))))
BEGIN SELECT RAISE(ABORT, 'browser deployment result must bind its manifest item, functional result and same-boot cleanup evidence'); END;
CREATE TRIGGER trg_browser_deployment_result_no_update BEFORE UPDATE ON browser_deployment_verification_results
BEGIN SELECT RAISE(ABORT, 'browser deployment verification result is immutable'); END;
CREATE TRIGGER trg_browser_deployment_result_no_delete BEFORE DELETE ON browser_deployment_verification_results
BEGIN SELECT RAISE(ABORT, 'browser deployment verification result is immutable'); END;

CREATE TRIGGER trg_lintel_recovery_receipt_closure BEFORE INSERT ON lintel_recovery_receipts
WHEN NOT EXISTS (
  SELECT 1 FROM maintenance_state m
  JOIN runtime_credentials oldc ON oldc.id = NEW.old_runtime_credential_id
    AND oldc.slot = 'lintel' AND oldc.generation = NEW.old_token_generation AND oldc.retired_at IS NOT NULL
  JOIN runtime_credentials newc ON newc.id = NEW.replacement_runtime_credential_id
    AND newc.slot = 'lintel' AND newc.generation = NEW.replacement_token_generation
    AND newc.confirmed_at IS NOT NULL AND newc.first_authenticated_at IS NOT NULL AND newc.retired_at IS NULL
  JOIN runtime_slots s ON s.slot = 'lintel' AND s.state = 'registered'
    AND s.current_credential_id = newc.id AND s.pending_credential_id IS NULL AND s.retiring_credential_id IS NULL
  WHERE m.id = 1 AND m.active = 1 AND m.reason = 'LintelRecovery' AND m.row_version = NEW.maintenance_revision)
BEGIN SELECT RAISE(ABORT, 'Lintel recovery receipt requires active maintenance, a retired old credential, and one authenticated replacement current credential'); END;
CREATE TRIGGER trg_lintel_recovery_receipt_no_update BEFORE UPDATE ON lintel_recovery_receipts
BEGIN SELECT RAISE(ABORT, 'Lintel recovery receipt is immutable'); END;
CREATE TRIGGER trg_lintel_recovery_receipt_no_delete BEFORE DELETE ON lintel_recovery_receipts
BEGIN SELECT RAISE(ABORT, 'Lintel recovery receipt is immutable'); END;

-- Deployment Acceptance tables are append-only; only the receipt constitutes finalization.
CREATE TRIGGER trg_verification_manifests_no_update BEFORE UPDATE ON verification_invocation_manifests BEGIN SELECT RAISE(ABORT, 'verification manifests are immutable'); END;
CREATE TRIGGER trg_verification_manifests_no_delete BEFORE DELETE ON verification_invocation_manifests BEGIN SELECT RAISE(ABORT, 'verification manifests are immutable'); END;
CREATE TRIGGER trg_verification_items_no_update BEFORE UPDATE ON verification_invocation_items BEGIN SELECT RAISE(ABORT, 'verification items are immutable'); END;
CREATE TRIGGER trg_verification_items_no_delete BEFORE DELETE ON verification_invocation_items BEGIN SELECT RAISE(ABORT, 'verification items are immutable'); END;
CREATE TRIGGER trg_verification_deployment_locators_no_update BEFORE UPDATE ON verification_deployment_item_locators BEGIN SELECT RAISE(ABORT, 'verification locators are immutable'); END;
CREATE TRIGGER trg_verification_deployment_locators_no_delete BEFORE DELETE ON verification_deployment_item_locators BEGIN SELECT RAISE(ABORT, 'verification locators are immutable'); END;
CREATE TRIGGER trg_verification_connection_locators_no_update BEFORE UPDATE ON verification_connection_item_locators BEGIN SELECT RAISE(ABORT, 'verification locators are immutable'); END;
CREATE TRIGGER trg_verification_connection_locators_no_delete BEFORE DELETE ON verification_connection_item_locators BEGIN SELECT RAISE(ABORT, 'verification locators are immutable'); END;
CREATE TRIGGER trg_verification_config_locators_no_update BEFORE UPDATE ON verification_config_item_locators BEGIN SELECT RAISE(ABORT, 'verification locators are immutable'); END;
CREATE TRIGGER trg_verification_config_locators_no_delete BEFORE DELETE ON verification_config_item_locators BEGIN SELECT RAISE(ABORT, 'verification locators are immutable'); END;
CREATE TRIGGER trg_verification_browser_locators_no_update BEFORE UPDATE ON verification_browser_identity_item_locators BEGIN SELECT RAISE(ABORT, 'verification locators are immutable'); END;
CREATE TRIGGER trg_verification_browser_locators_no_delete BEFORE DELETE ON verification_browser_identity_item_locators BEGIN SELECT RAISE(ABORT, 'verification locators are immutable'); END;
CREATE TRIGGER trg_verification_ui_locators_no_update BEFORE UPDATE ON verification_ui_observation_item_locators BEGIN SELECT RAISE(ABORT, 'verification locators are immutable'); END;
CREATE TRIGGER trg_verification_ui_locators_no_delete BEFORE DELETE ON verification_ui_observation_item_locators BEGIN SELECT RAISE(ABORT, 'verification locators are immutable'); END;
CREATE TRIGGER trg_verification_item_results_no_update BEFORE UPDATE ON verification_item_results BEGIN SELECT RAISE(ABORT, 'verification results are immutable'); END;
CREATE TRIGGER trg_verification_item_results_no_delete BEFORE DELETE ON verification_item_results BEGIN SELECT RAISE(ABORT, 'verification results are immutable'); END;
CREATE TRIGGER trg_verification_conflicts_no_update BEFORE UPDATE ON verification_result_conflicts BEGIN SELECT RAISE(ABORT, 'verification conflicts are immutable'); END;
CREATE TRIGGER trg_verification_conflicts_no_delete BEFORE DELETE ON verification_result_conflicts BEGIN SELECT RAISE(ABORT, 'verification conflicts are immutable'); END;
CREATE TRIGGER trg_verification_helper_imports_no_update BEFORE UPDATE ON verification_helper_imports BEGIN SELECT RAISE(ABORT, 'verification helper imports are immutable'); END;
CREATE TRIGGER trg_verification_helper_imports_no_delete BEFORE DELETE ON verification_helper_imports BEGIN SELECT RAISE(ABORT, 'verification helper imports are immutable'); END;
CREATE TRIGGER trg_verification_observations_no_update BEFORE UPDATE ON verification_typed_observations BEGIN SELECT RAISE(ABORT, 'verification observations are immutable'); END;
CREATE TRIGGER trg_verification_observations_no_delete BEFORE DELETE ON verification_typed_observations BEGIN SELECT RAISE(ABORT, 'verification observations are immutable'); END;
CREATE TRIGGER trg_verification_subject_drifts_no_update BEFORE UPDATE ON verification_subject_drifts BEGIN SELECT RAISE(ABORT, 'verification subject drift is immutable'); END;
CREATE TRIGGER trg_verification_subject_drifts_no_delete BEFORE DELETE ON verification_subject_drifts BEGIN SELECT RAISE(ABORT, 'verification subject drift is immutable'); END;
CREATE TRIGGER trg_verification_receipts_no_update BEFORE UPDATE ON verification_finalization_receipts BEGIN SELECT RAISE(ABORT, 'verification finalization receipt is immutable'); END;
CREATE TRIGGER trg_verification_receipts_no_delete BEFORE DELETE ON verification_finalization_receipts BEGIN SELECT RAISE(ABORT, 'verification finalization receipt is immutable'); END;
