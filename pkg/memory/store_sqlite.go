package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// SQLiteStore is the canonical persistent memory storage.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates/opens the memory database at path.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create memory db dir: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}
	// Single-process memory service. Use one shared connection to avoid
	// writer lock contention with SQLite under concurrent goroutines.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &SQLiteStore{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *SQLiteStore) init() error {
	stmts := []string{
		`PRAGMA journal_mode=WAL;`,
		`PRAGMA synchronous=NORMAL;`,
		`PRAGMA temp_store=MEMORY;`,
		`PRAGMA busy_timeout=5000;`,
		`CREATE TABLE IF NOT EXISTS sessions (
			session_key TEXT PRIMARY KEY,
			channel TEXT NOT NULL DEFAULT '',
			chat_id TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL DEFAULT '',
			created_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL,
			message_count INTEGER NOT NULL DEFAULT 0,
			summary TEXT NOT NULL DEFAULT '',
			last_consolidated_ms INTEGER NOT NULL DEFAULT 0
		);`,
		`CREATE TABLE IF NOT EXISTS session_provider_state (
			session_key TEXT PRIMARY KEY,
			state_id TEXT NOT NULL DEFAULT '',
			updated_at_ms INTEGER NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS events (
			id TEXT PRIMARY KEY,
			session_key TEXT NOT NULL,
			turn_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			tool_call_id TEXT NOT NULL DEFAULT '',
			tool_name TEXT NOT NULL DEFAULT '',
			metadata_json TEXT NOT NULL DEFAULT '{}',
			created_at_ms INTEGER NOT NULL,
			archived INTEGER NOT NULL DEFAULT 0
		);`,
		`CREATE INDEX IF NOT EXISTS events_session_active_idx ON events(session_key, archived, created_at_ms DESC, seq DESC);`,
		`CREATE INDEX IF NOT EXISTS events_session_turn_idx ON events(session_key, turn_id, seq);`,
		`CREATE TABLE IF NOT EXISTS session_compactions (
			id TEXT PRIMARY KEY,
			session_key TEXT NOT NULL,
			started_at_ms INTEGER NOT NULL,
			completed_at_ms INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL,
			source_event_count INTEGER NOT NULL,
			retained_event_count INTEGER NOT NULL,
			summary TEXT NOT NULL DEFAULT '',
			checkpoint_json TEXT NOT NULL DEFAULT '{}',
			error TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE INDEX IF NOT EXISTS compaction_session_idx ON session_compactions(session_key, started_at_ms DESC);`,
		`CREATE TABLE IF NOT EXISTS session_snapshots (
			session_key TEXT NOT NULL,
			revision INTEGER NOT NULL,
			created_at_ms INTEGER NOT NULL,
			facts_json TEXT NOT NULL DEFAULT '[]',
			preferences_json TEXT NOT NULL DEFAULT '[]',
			tasks_json TEXT NOT NULL DEFAULT '[]',
			open_loops_json TEXT NOT NULL DEFAULT '[]',
			constraints_json TEXT NOT NULL DEFAULT '[]',
			summary TEXT NOT NULL DEFAULT '',
			compaction_id TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(session_key, revision)
		);`,
		`CREATE INDEX IF NOT EXISTS session_snapshots_latest_idx ON session_snapshots(session_key, revision DESC, created_at_ms DESC);`,
		`CREATE TABLE IF NOT EXISTS memory_items (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL DEFAULT '',
			scope_type TEXT NOT NULL DEFAULT 'session',
			scope_id TEXT NOT NULL DEFAULT '',
			session_key TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL,
			item_key TEXT NOT NULL,
			content TEXT NOT NULL,
			confidence REAL NOT NULL DEFAULT 0,
			weight REAL NOT NULL DEFAULT 1,
			source_event_id TEXT NOT NULL DEFAULT '',
			first_seen_at_ms INTEGER NOT NULL,
			last_seen_at_ms INTEGER NOT NULL,
			expires_at_ms INTEGER NOT NULL DEFAULT 0,
			deleted_at_ms INTEGER NOT NULL DEFAULT 0,
			metadata_json TEXT NOT NULL DEFAULT '{}'
		);`,
		`CREATE INDEX IF NOT EXISTS memory_items_legacy_scope_idx ON memory_items(user_id, agent_id, session_key, deleted_at_ms, expires_at_ms, last_seen_at_ms DESC);`,
		`CREATE TABLE IF NOT EXISTS memory_observations (
			id TEXT PRIMARY KEY,
			item_id TEXT NOT NULL,
			session_key TEXT NOT NULL DEFAULT '',
			event_id TEXT NOT NULL DEFAULT '',
			observed_at_ms INTEGER NOT NULL,
			confidence REAL NOT NULL DEFAULT 0,
			content TEXT NOT NULL DEFAULT '',
			extractor TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL DEFAULT 'upsert',
			metadata_json TEXT NOT NULL DEFAULT '{}'
		);`,
		`CREATE INDEX IF NOT EXISTS memory_obs_item_idx ON memory_observations(item_id, observed_at_ms DESC);`,
		`CREATE INDEX IF NOT EXISTS memory_obs_event_idx ON memory_observations(event_id, observed_at_ms DESC);`,
		`CREATE TABLE IF NOT EXISTS memory_links (
			id TEXT PRIMARY KEY,
			from_item_id TEXT NOT NULL,
			to_item_id TEXT NOT NULL,
			relation TEXT NOT NULL,
			weight REAL NOT NULL DEFAULT 1,
			created_at_ms INTEGER NOT NULL
		);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS memory_links_unique ON memory_links(from_item_id, to_item_id, relation);`,
		`CREATE INDEX IF NOT EXISTS memory_links_from_idx ON memory_links(from_item_id, created_at_ms DESC);`,
		`CREATE TABLE IF NOT EXISTS memory_embeddings (
			item_id TEXT PRIMARY KEY,
			model TEXT NOT NULL,
			vector_json TEXT NOT NULL,
			norm REAL NOT NULL DEFAULT 0,
			updated_at_ms INTEGER NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS retrieval_cache (
			cache_key TEXT PRIMARY KEY,
			result_json TEXT NOT NULL,
			created_at_ms INTEGER NOT NULL,
			expires_at_ms INTEGER NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS retrieval_cache_exp_idx ON retrieval_cache(expires_at_ms);`,
		`CREATE TABLE IF NOT EXISTS memory_jobs (
			id TEXT PRIMARY KEY,
			job_type TEXT NOT NULL,
			session_key TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			priority INTEGER NOT NULL DEFAULT 100,
			payload_json TEXT NOT NULL DEFAULT '{}',
			error TEXT NOT NULL DEFAULT '',
			run_after_ms INTEGER NOT NULL,
			lease_until_ms INTEGER NOT NULL DEFAULT 0,
			created_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL,
			completed_at_ms INTEGER NOT NULL DEFAULT 0
		);`,
		`CREATE INDEX IF NOT EXISTS memory_jobs_claim_idx ON memory_jobs(status, run_after_ms, lease_until_ms, priority, created_at_ms);`,
		`CREATE TABLE IF NOT EXISTS memory_metrics (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			metric TEXT NOT NULL,
			value REAL NOT NULL,
			labels_json TEXT NOT NULL DEFAULT '{}',
			created_at_ms INTEGER NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS memory_metrics_metric_idx ON memory_metrics(metric, created_at_ms DESC);`,
		`CREATE TABLE IF NOT EXISTS memory_audit_log (
			id TEXT PRIMARY KEY,
			action TEXT NOT NULL,
			entity TEXT NOT NULL,
			entity_id TEXT NOT NULL DEFAULT '',
			session_key TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL DEFAULT '',
			reason TEXT NOT NULL DEFAULT '',
			payload_json TEXT NOT NULL DEFAULT '{}',
			created_at_ms INTEGER NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS memory_audit_created_idx ON memory_audit_log(created_at_ms DESC);`,
		`CREATE TABLE IF NOT EXISTS persona_profiles (
			user_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			profile_json TEXT NOT NULL,
			revision INTEGER NOT NULL DEFAULT 1,
			updated_at_ms INTEGER NOT NULL,
			PRIMARY KEY(user_id, agent_id)
		);`,
		`CREATE TABLE IF NOT EXISTS persona_candidates (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			session_key TEXT NOT NULL DEFAULT '',
			turn_id TEXT NOT NULL DEFAULT '',
			source_event_id TEXT NOT NULL DEFAULT '',
			field_path TEXT NOT NULL,
			operation TEXT NOT NULL,
			value TEXT NOT NULL DEFAULT '',
			confidence REAL NOT NULL DEFAULT 0,
			evidence TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			rejected_reason TEXT NOT NULL DEFAULT '',
			applied_revision_id TEXT NOT NULL DEFAULT '',
			created_at_ms INTEGER NOT NULL,
			applied_at_ms INTEGER NOT NULL DEFAULT 0
		);`,
		`CREATE INDEX IF NOT EXISTS persona_candidates_status_idx ON persona_candidates(user_id, agent_id, status, created_at_ms DESC);`,
		`CREATE INDEX IF NOT EXISTS persona_candidates_turn_idx ON persona_candidates(user_id, agent_id, session_key, turn_id, status, created_at_ms DESC);`,
		`DELETE FROM persona_candidates
WHERE rowid NOT IN (
	SELECT MAX(rowid)
	FROM persona_candidates
	GROUP BY user_id, agent_id, session_key, turn_id, field_path, operation, value
);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS persona_candidates_unique_key ON persona_candidates(user_id, agent_id, session_key, turn_id, field_path, operation, value);`,
		`CREATE TABLE IF NOT EXISTS persona_revisions (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			session_key TEXT NOT NULL DEFAULT '',
			turn_id TEXT NOT NULL DEFAULT '',
			candidate_id TEXT NOT NULL DEFAULT '',
			field_path TEXT NOT NULL,
			operation TEXT NOT NULL,
			old_value TEXT NOT NULL DEFAULT '',
			new_value TEXT NOT NULL DEFAULT '',
			confidence REAL NOT NULL DEFAULT 0,
			evidence TEXT NOT NULL DEFAULT '',
			reason TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '',
			profile_before_json TEXT NOT NULL DEFAULT '{}',
			profile_after_json TEXT NOT NULL DEFAULT '{}',
			created_at_ms INTEGER NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS persona_revisions_profile_idx ON persona_revisions(user_id, agent_id, created_at_ms DESC);`,
		`CREATE TABLE IF NOT EXISTS persona_signals (
			user_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			field_path TEXT NOT NULL,
			value_hash TEXT NOT NULL,
			hits INTEGER NOT NULL DEFAULT 0,
			last_seen_at_ms INTEGER NOT NULL,
			PRIMARY KEY(user_id, agent_id, field_path, value_hash)
		);`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memory_items_fts USING fts5(item_id UNINDEXED, content, tokenize='unicode61 remove_diacritics 2');`,
		`DROP TRIGGER IF EXISTS memory_items_ai;`,
		`DROP TRIGGER IF EXISTS memory_items_au;`,
		`DROP TRIGGER IF EXISTS memory_items_ad;`,
		`CREATE TRIGGER IF NOT EXISTS memory_items_ai AFTER INSERT ON memory_items BEGIN
			INSERT INTO memory_items_fts(item_id, content) VALUES (new.id, new.content);
		END;`,
		`CREATE TRIGGER IF NOT EXISTS memory_items_au AFTER UPDATE OF content ON memory_items BEGIN
			DELETE FROM memory_items_fts WHERE item_id = old.id;
			INSERT INTO memory_items_fts(item_id, content) VALUES(new.id, new.content);
		END;`,
		`CREATE TRIGGER IF NOT EXISTS memory_items_ad AFTER DELETE ON memory_items BEGIN
			DELETE FROM memory_items_fts WHERE item_id = old.id;
		END;`,
	}

	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("init sqlite schema failed on %q: %w", trimSQL(stmt), err)
		}
	}

	// Backward-compatible migrations for older databases.
	if err := ensureColumnExists(s.db, "memory_items", "scope_type", "TEXT NOT NULL DEFAULT 'session'"); err != nil {
		return err
	}
	if err := ensureColumnExists(s.db, "memory_items", "scope_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if _, err := s.db.Exec(`
UPDATE memory_items
SET scope_type = CASE
	WHEN TRIM(scope_type) = '' AND TRIM(session_key) = '' THEN 'global'
	WHEN TRIM(scope_type) = '' THEN 'session'
	ELSE scope_type
END,
scope_id = CASE
	WHEN TRIM(scope_id) = '' AND TRIM(scope_type) = 'session' THEN session_key
	WHEN TRIM(scope_id) = '' AND TRIM(scope_type) = 'user' THEN user_id
	ELSE scope_id
END`); err != nil {
		return fmt.Errorf("migrate memory scope columns: %w", err)
	}
	if _, err := s.db.Exec(`
UPDATE memory_items
SET scope_type = 'global'
WHERE TRIM(scope_type) = 'session' AND TRIM(scope_id) = '' AND TRIM(session_key) = ''`); err != nil {
		return fmt.Errorf("normalize global memory scope: %w", err)
	}
	if _, err := s.db.Exec(`DROP INDEX IF EXISTS memory_items_unique_active`); err != nil {
		return fmt.Errorf("drop legacy memory unique index: %w", err)
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS memory_items_unique_active ON memory_items(user_id, agent_id, scope_type, scope_id, kind, item_key)`); err != nil {
		return fmt.Errorf("create memory unique index: %w", err)
	}
	if _, err := s.db.Exec(`DROP INDEX IF EXISTS memory_items_scope_idx`); err != nil {
		return fmt.Errorf("drop legacy memory scope index: %w", err)
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS memory_items_scope_idx ON memory_items(user_id, agent_id, scope_type, scope_id, deleted_at_ms, expires_at_ms, last_seen_at_ms DESC)`); err != nil {
		return fmt.Errorf("create memory scope index: %w", err)
	}
	if _, err := s.db.Exec(`DROP INDEX IF EXISTS memory_items_legacy_scope_idx`); err != nil {
		return fmt.Errorf("drop legacy memory scope compatibility index: %w", err)
	}

	if _, err := s.db.Exec(`DELETE FROM retrieval_cache WHERE expires_at_ms <= ?`, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("purge retrieval cache: %w", err)
	}

	return nil
}

func trimSQL(sql string) string {
	line := strings.TrimSpace(sql)
	if len(line) > 96 {
		return line[:96] + "..."
	}
	return line
}

func ensureColumnExists(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	defer rows.Close()

	var (
		cid       int
		name      string
		colType   string
		notNull   int
		dfltValue sql.NullString
		pk        int
	)
	for rows.Next() {
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("scan table info(%s): %w", table, err)
		}
		if strings.EqualFold(name, column) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate table info(%s): %w", table, err)
	}

	stmt := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, definition)
	if _, err := db.Exec(stmt); err != nil {
		return fmt.Errorf("alter table add column %s.%s: %w", table, column, err)
	}
	return nil
}

func nowMS() int64 { return time.Now().UnixMilli() }

func invalidateRetrievalCacheTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM retrieval_cache`); err != nil {
		return fmt.Errorf("invalidate retrieval cache: %w", err)
	}
	return nil
}

func (s *SQLiteStore) invalidateRetrievalCache(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM retrieval_cache`); err != nil {
		return fmt.Errorf("invalidate retrieval cache: %w", err)
	}
	return nil
}

func encodeMap(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func encodeStringSlice(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		key := strings.ToLower(v)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, v)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func decodeStringSlice(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := []string{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func insertAuditLogTx(ctx context.Context, tx *sql.Tx, action, entity, entityID, sessionKey, userID, agentID, reason string, payload map[string]string) error {
	if tx == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO memory_audit_log(id, action, entity, entity_id, session_key, user_id, agent_id, reason, payload_json, created_at_ms)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"audit-"+uuid.NewString(),
		action,
		entity,
		entityID,
		sessionKey,
		userID,
		agentID,
		reason,
		encodeMap(payload),
		nowMS(),
	)
	if err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}

func (s *SQLiteStore) insertAuditLog(ctx context.Context, action, entity, entityID, sessionKey, userID, agentID, reason string, payload map[string]string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO memory_audit_log(id, action, entity, entity_id, session_key, user_id, agent_id, reason, payload_json, created_at_ms)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"audit-"+uuid.NewString(),
		action,
		entity,
		entityID,
		sessionKey,
		userID,
		agentID,
		reason,
		encodeMap(payload),
		nowMS(),
	)
	if err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}

func decodeMap(raw string) map[string]string {
	if raw == "" {
		return map[string]string{}
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]string{}
	}
	return out
}

func encodeVector(vec []float32) string {
	if len(vec) == 0 {
		return "[]"
	}
	b, err := json.Marshal(vec)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func decodeVector(raw string) []float32 {
	if raw == "" {
		return nil
	}
	out := []float32{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func (s *SQLiteStore) EnsureSession(ctx context.Context, sessionKey, channel, chatID, userID string) error {
	now := nowMS()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO sessions(session_key, channel, chat_id, user_id, created_at_ms, updated_at_ms, message_count, summary, last_consolidated_ms)
VALUES(?, ?, ?, ?, ?, ?, 0, '', 0)
ON CONFLICT(session_key) DO UPDATE SET
	channel = CASE WHEN excluded.channel <> '' THEN excluded.channel ELSE sessions.channel END,
	chat_id = CASE WHEN excluded.chat_id <> '' THEN excluded.chat_id ELSE sessions.chat_id END,
	user_id = CASE WHEN sessions.user_id = '' THEN excluded.user_id ELSE sessions.user_id END,
	updated_at_ms = excluded.updated_at_ms`,
		sessionKey, channel, chatID, userID, now, now)
	if err != nil {
		return fmt.Errorf("ensure session: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetSession(ctx context.Context, sessionKey string) (Session, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT session_key, channel, chat_id, user_id, created_at_ms, updated_at_ms, message_count, summary, last_consolidated_ms
FROM sessions WHERE session_key = ?`, sessionKey)
	var out Session
	if err := row.Scan(&out.SessionKey, &out.Channel, &out.ChatID, &out.UserID, &out.CreatedAtMS, &out.UpdatedAtMS, &out.MessageCount, &out.Summary, &out.LastConsolidatedMS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, sql.ErrNoRows
		}
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) MarkSessionConsolidated(ctx context.Context, sessionKey string, atMS int64) error {
	if atMS == 0 {
		atMS = nowMS()
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE sessions
SET last_consolidated_ms = ?, updated_at_ms = ?
WHERE session_key = ?`, atMS, atMS, sessionKey)
	if err != nil {
		return fmt.Errorf("mark session consolidated: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetSessionSummary(ctx context.Context, sessionKey string) (string, error) {
	row := s.db.QueryRowContext(ctx, `SELECT summary FROM sessions WHERE session_key = ?`, sessionKey)
	var summary string
	if err := row.Scan(&summary); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get session summary: %w", err)
	}
	return summary, nil
}

func (s *SQLiteStore) SetSessionSummary(ctx context.Context, sessionKey, summary string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET summary = ?, updated_at_ms = ? WHERE session_key = ?`, summary, nowMS(), sessionKey)
	if err != nil {
		return fmt.Errorf("set session summary: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetSessionProviderState(ctx context.Context, sessionKey string) (string, error) {
	row := s.db.QueryRowContext(ctx, `SELECT state_id FROM session_provider_state WHERE session_key = ?`, sessionKey)
	var stateID string
	if err := row.Scan(&stateID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get session provider state: %w", err)
	}
	return stateID, nil
}

func (s *SQLiteStore) SetSessionProviderState(ctx context.Context, sessionKey, stateID string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO session_provider_state(session_key, state_id, updated_at_ms)
VALUES(?, ?, ?)
ON CONFLICT(session_key) DO UPDATE SET
	state_id = excluded.state_id,
	updated_at_ms = excluded.updated_at_ms`,
		sessionKey, strings.TrimSpace(stateID), nowMS(),
	)
	if err != nil {
		return fmt.Errorf("set session provider state: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetLatestSessionSnapshot(ctx context.Context, sessionKey string) (SessionSnapshot, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT session_key, revision, created_at_ms, facts_json, preferences_json, tasks_json, open_loops_json, constraints_json, summary, compaction_id
FROM session_snapshots
WHERE session_key = ?
ORDER BY revision DESC, created_at_ms DESC
LIMIT 1`, sessionKey)
	var snap SessionSnapshot
	var factsRaw, prefRaw, tasksRaw, loopsRaw, constraintsRaw string
	if err := row.Scan(&snap.SessionKey, &snap.Revision, &snap.CreatedAtMS, &factsRaw, &prefRaw, &tasksRaw, &loopsRaw, &constraintsRaw, &snap.Summary, &snap.CompactionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionSnapshot{}, nil
		}
		return SessionSnapshot{}, fmt.Errorf("get latest session snapshot: %w", err)
	}
	snap.Facts = decodeStringSlice(factsRaw)
	snap.Preferences = decodeStringSlice(prefRaw)
	snap.Tasks = decodeStringSlice(tasksRaw)
	snap.OpenLoops = decodeStringSlice(loopsRaw)
	snap.Constraints = decodeStringSlice(constraintsRaw)
	return snap, nil
}

func (s *SQLiteStore) UpsertSessionSnapshot(ctx context.Context, snap SessionSnapshot) error {
	if strings.TrimSpace(snap.SessionKey) == "" {
		return fmt.Errorf("upsert session snapshot: empty session key")
	}
	if snap.CreatedAtMS == 0 {
		snap.CreatedAtMS = nowMS()
	}
	if snap.Revision <= 0 {
		var next int
		row := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision), 0) + 1 FROM session_snapshots WHERE session_key = ?`, snap.SessionKey)
		if err := row.Scan(&next); err != nil {
			return fmt.Errorf("upsert session snapshot: resolve revision: %w", err)
		}
		snap.Revision = next
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO session_snapshots(session_key, revision, created_at_ms, facts_json, preferences_json, tasks_json, open_loops_json, constraints_json, summary, compaction_id)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_key, revision) DO UPDATE SET
	created_at_ms = excluded.created_at_ms,
	facts_json = excluded.facts_json,
	preferences_json = excluded.preferences_json,
	tasks_json = excluded.tasks_json,
	open_loops_json = excluded.open_loops_json,
	constraints_json = excluded.constraints_json,
	summary = excluded.summary,
	compaction_id = excluded.compaction_id`,
		snap.SessionKey,
		snap.Revision,
		snap.CreatedAtMS,
		encodeStringSlice(snap.Facts),
		encodeStringSlice(snap.Preferences),
		encodeStringSlice(snap.Tasks),
		encodeStringSlice(snap.OpenLoops),
		encodeStringSlice(snap.Constraints),
		snap.Summary,
		snap.CompactionID,
	)
	if err != nil {
		return fmt.Errorf("upsert session snapshot: %w", err)
	}
	return nil
}

func (s *SQLiteStore) AppendEvent(ctx context.Context, ev Event) error {
	if strings.TrimSpace(ev.SessionKey) == "" {
		return fmt.Errorf("append event: empty session_key")
	}
	if strings.TrimSpace(ev.Role) == "" {
		return fmt.Errorf("append event: empty role")
	}
	if ev.ID == "" {
		ev.ID = uuid.NewString()
	}
	if ev.TurnID == "" {
		ev.TurnID = "turn-" + uuid.NewString()
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}

	meta := encodeMap(ev.Metadata)
	created := ev.CreatedAt.UnixMilli()
	archived := 0
	if ev.Archived {
		archived = 1
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append event begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := nowMS()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO sessions(session_key, channel, chat_id, user_id, created_at_ms, updated_at_ms, message_count, summary, last_consolidated_ms)
VALUES(?, '', '', '', ?, ?, 0, '', 0)
ON CONFLICT(session_key) DO UPDATE SET updated_at_ms = excluded.updated_at_ms`, ev.SessionKey, now, now); err != nil {
		return fmt.Errorf("append event ensure session: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO events(id, session_key, turn_id, seq, role, content, tool_call_id, tool_name, metadata_json, created_at_ms, archived)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, ev.ID, ev.SessionKey, ev.TurnID, ev.Seq, ev.Role, ev.Content, ev.ToolCallID, ev.ToolName, meta, created, archived); err != nil {
		return fmt.Errorf("append event insert: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE sessions
SET updated_at_ms = ?, message_count = message_count + 1
WHERE session_key = ?`, created, ev.SessionKey); err != nil {
		return fmt.Errorf("append event update session: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("append event commit: %w", err)
	}
	return nil
}

func (s *SQLiteStore) AppendUserEventAndMemories(ctx context.Context, ev Event, userID, agentID string, ops []ConsolidationOp) (int, error) {
	if strings.TrimSpace(ev.SessionKey) == "" {
		return 0, fmt.Errorf("append user event and memories: empty session_key")
	}
	if strings.TrimSpace(ev.Role) == "" {
		ev.Role = "user"
	}
	if ev.ID == "" {
		ev.ID = "evt-" + uuid.NewString()
	}
	if ev.TurnID == "" {
		ev.TurnID = "turn-" + uuid.NewString()
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}
	if strings.TrimSpace(agentID) == "" {
		agentID = "dotagent"
	}

	meta := encodeMap(ev.Metadata)
	created := ev.CreatedAt.UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("append user event and memories begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := nowMS()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO sessions(session_key, channel, chat_id, user_id, created_at_ms, updated_at_ms, message_count, summary, last_consolidated_ms)
VALUES(?, '', '', '', ?, ?, 0, '', 0)
ON CONFLICT(session_key) DO UPDATE SET updated_at_ms = excluded.updated_at_ms`, ev.SessionKey, now, now); err != nil {
		return 0, fmt.Errorf("append user event and memories ensure session: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO events(id, session_key, turn_id, seq, role, content, tool_call_id, tool_name, metadata_json, created_at_ms, archived)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`, ev.ID, ev.SessionKey, ev.TurnID, ev.Seq, ev.Role, ev.Content, ev.ToolCallID, ev.ToolName, meta, created); err != nil {
		return 0, fmt.Errorf("append user event and memories insert event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE sessions
SET updated_at_ms = ?, message_count = message_count + 1
WHERE session_key = ?`, created, ev.SessionKey); err != nil {
		return 0, fmt.Errorf("append user event and memories update session: %w", err)
	}

	inserted := 0
	for _, op := range ops {
		if strings.TrimSpace(op.Key) == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(op.Action)) {
		case "delete":
			scopeType, scopeID := deriveScopeForOp(op.Kind, ev.SessionKey, userID, op.Metadata)
			args := []interface{}{nowMS(), userID, agentID, string(op.Kind), op.Key}
			query := `
UPDATE memory_items
SET deleted_at_ms = ?
WHERE user_id = ? AND agent_id = ? AND kind = ? AND item_key = ?`
			if scopeType != "" {
				query += ` AND scope_type = ?`
				args = append(args, string(scopeType))
			}
			if strings.TrimSpace(scopeID) != "" {
				query += ` AND scope_id = ?`
				args = append(args, scopeID)
			}
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return inserted, fmt.Errorf("append user event and memories delete memory: %w", err)
			}
			_ = insertAuditLogTx(ctx, tx, "memory_delete", "memory_item", op.Key, ev.SessionKey, userID, agentID, "user_forget_request", map[string]string{
				"kind":      string(op.Kind),
				"scope":     string(scopeType),
				"scope_id":  scopeID,
				"source_ev": ev.ID,
			})
		default:
			scopeType, scopeID := deriveScopeForOp(op.Kind, ev.SessionKey, userID, op.Metadata)
			item, mapErr := buildMemoryItemFromOp(op, ev, userID, agentID, scopeType, scopeID)
			if mapErr != nil {
				return inserted, mapErr
			}
			itemID, upsertErr := upsertMemoryItemTx(ctx, tx, item)
			if upsertErr != nil {
				return inserted, fmt.Errorf("append user event and memories upsert memory: %w", upsertErr)
			}
			if embErr := upsertEmbeddingTx(ctx, tx, itemID, currentEmbeddingModel(), embedText(item.Content)); embErr != nil {
				return inserted, embErr
			}
			inserted++
		}
	}

	if err := invalidateRetrievalCacheTx(ctx, tx); err != nil {
		return inserted, err
	}
	if err := tx.Commit(); err != nil {
		return inserted, fmt.Errorf("append user event and memories commit: %w", err)
	}
	return inserted, nil
}

func (s *SQLiteStore) ListRecentEvents(ctx context.Context, sessionKey string, limit int, includeArchived bool) ([]Event, error) {
	if limit <= 0 {
		limit = 1
	}
	archivedFilter := 0
	if includeArchived {
		archivedFilter = 1
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, session_key, turn_id, seq, role, content, tool_call_id, tool_name, metadata_json, created_at_ms, archived
FROM events
WHERE session_key = ?
AND (? = 1 OR archived = 0)
ORDER BY created_at_ms DESC, seq DESC
LIMIT ?`, sessionKey, archivedFilter, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent events: %w", err)
	}
	defer rows.Close()

	out := make([]Event, 0, limit)
	for rows.Next() {
		var ev Event
		var createdMS int64
		var metaRaw string
		var archived int
		if err := rows.Scan(&ev.ID, &ev.SessionKey, &ev.TurnID, &ev.Seq, &ev.Role, &ev.Content, &ev.ToolCallID, &ev.ToolName, &metaRaw, &createdMS, &archived); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		ev.Metadata = decodeMap(metaRaw)
		ev.CreatedAt = time.UnixMilli(createdMS)
		ev.Archived = archived != 0
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}

	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (s *SQLiteStore) ArchiveEventsBefore(ctx context.Context, sessionKey string, keepLatest int) (int, error) {
	if keepLatest < 0 {
		keepLatest = 0
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE events
SET archived = 1
WHERE session_key = ?
AND archived = 0
AND id NOT IN (
	SELECT id FROM events
	WHERE session_key = ? AND archived = 0
	ORDER BY created_at_ms DESC, seq DESC
	LIMIT ?
)`, sessionKey, sessionKey, keepLatest)
	if err != nil {
		return 0, fmt.Errorf("archive events before: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *SQLiteStore) ArchiveEventsExceptTurns(ctx context.Context, sessionKey string, keepTurnIDs []string) (int, error) {
	if strings.TrimSpace(sessionKey) == "" {
		return 0, fmt.Errorf("archive events except turns: empty session_key")
	}
	keep := uniqueStrings(keepTurnIDs)
	if len(keep) == 0 {
		res, err := s.db.ExecContext(ctx, `
UPDATE events
SET archived = 1
WHERE session_key = ?
AND archived = 0`, sessionKey)
		if err != nil {
			return 0, fmt.Errorf("archive events except turns: %w", err)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}

	placeholders := strings.TrimRight(strings.Repeat("?,", len(keep)), ",")
	args := make([]interface{}, 0, 1+len(keep))
	args = append(args, sessionKey)
	for _, turnID := range keep {
		args = append(args, turnID)
	}
	query := fmt.Sprintf(`
UPDATE events
SET archived = 1
WHERE session_key = ?
AND archived = 0
AND turn_id NOT IN (%s)`, placeholders)
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("archive events except turns: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *SQLiteStore) StartCompaction(ctx context.Context, sessionKey string, sourceCount, retainedCount int, checkpoint map[string]string) (string, error) {
	id := "cmp-" + uuid.NewString()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO session_compactions(id, session_key, started_at_ms, completed_at_ms, status, source_event_count, retained_event_count, summary, checkpoint_json, error)
VALUES(?, ?, ?, 0, ?, ?, ?, '', ?, '')`, id, sessionKey, nowMS(), JobRunning, sourceCount, retainedCount, encodeMap(checkpoint))
	if err != nil {
		return "", fmt.Errorf("start compaction: %w", err)
	}
	return id, nil
}

func (s *SQLiteStore) CheckpointCompaction(ctx context.Context, compactionID string, checkpoint map[string]string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE session_compactions
SET checkpoint_json = ?, status = ?, error = ''
WHERE id = ?`, encodeMap(checkpoint), JobRunning, compactionID)
	if err != nil {
		return fmt.Errorf("checkpoint compaction: %w", err)
	}
	return nil
}

func (s *SQLiteStore) CompleteCompaction(ctx context.Context, compactionID, summary string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE session_compactions
SET summary = ?, status = ?, completed_at_ms = ?, error = ''
WHERE id = ?`, summary, JobCompleted, nowMS(), compactionID)
	if err != nil {
		return fmt.Errorf("complete compaction: %w", err)
	}
	return nil
}

func (s *SQLiteStore) FailCompaction(ctx context.Context, compactionID, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE session_compactions
SET status = ?, completed_at_ms = ?, error = ?
WHERE id = ?`, JobFailed, nowMS(), errMsg, compactionID)
	if err != nil {
		return fmt.Errorf("fail compaction: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UpsertMemoryItem(ctx context.Context, item MemoryItem) (MemoryItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MemoryItem{}, fmt.Errorf("upsert memory item begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := upsertMemoryItemTx(ctx, tx, item)
	if err != nil {
		return MemoryItem{}, fmt.Errorf("upsert memory item: %w", err)
	}

	row := tx.QueryRowContext(ctx, `
SELECT id, user_id, agent_id, scope_type, scope_id, session_key, kind, item_key, content, confidence, weight, source_event_id, first_seen_at_ms, last_seen_at_ms, expires_at_ms, deleted_at_ms, metadata_json
FROM memory_items
WHERE id = ?`, id)

	var out MemoryItem
	var kind string
	var scopeType string
	var metadataRaw string
	if err := row.Scan(
		&out.ID,
		&out.UserID,
		&out.AgentID,
		&scopeType,
		&out.ScopeID,
		&out.SessionKey,
		&kind,
		&out.Key,
		&out.Content,
		&out.Confidence,
		&out.Weight,
		&out.SourceEventID,
		&out.FirstSeenAtMS,
		&out.LastSeenAtMS,
		&out.ExpiresAtMS,
		&out.DeletedAtMS,
		&metadataRaw,
	); err != nil {
		return MemoryItem{}, fmt.Errorf("read upserted memory item: %w", err)
	}
	out.ScopeType = MemoryScopeType(scopeType)
	out.Kind = MemoryItemKind(kind)
	out.Metadata = decodeMap(metadataRaw)
	normalizeMemoryScope(&out)
	if err := invalidateRetrievalCacheTx(ctx, tx); err != nil {
		return MemoryItem{}, err
	}
	if err := tx.Commit(); err != nil {
		return MemoryItem{}, fmt.Errorf("upsert memory item commit: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) DeleteMemoryByKey(ctx context.Context, userID, agentID string, kind MemoryItemKind, key string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE memory_items
SET deleted_at_ms = ?
WHERE user_id = ? AND agent_id = ? AND kind = ? AND item_key = ?`, nowMS(), userID, agentID, string(kind), key)
	if err != nil {
		return fmt.Errorf("delete memory by key: %w", err)
	}
	_ = s.insertAuditLog(ctx, "memory_delete", "memory_item", key, "", userID, agentID, "delete_by_key", map[string]string{
		"kind": string(kind),
	})
	if err := s.invalidateRetrievalCache(ctx); err != nil {
		return err
	}
	return nil
}

func (s *SQLiteStore) ListMemoryCandidates(ctx context.Context, userID, agentID, sessionKey string, limit int) ([]MemoryItem, error) {
	_ = sessionKey
	if limit <= 0 {
		limit = 20
	}
	now := nowMS()
	rows, err := s.db.QueryContext(ctx, `
SELECT id, user_id, agent_id, scope_type, scope_id, session_key, kind, item_key, content, confidence, weight, source_event_id, first_seen_at_ms, last_seen_at_ms, expires_at_ms, deleted_at_ms, metadata_json
FROM memory_items
WHERE user_id = ? AND agent_id = ?
AND deleted_at_ms = 0
AND (expires_at_ms = 0 OR expires_at_ms > ?)
ORDER BY last_seen_at_ms DESC
LIMIT ?`, userID, agentID, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list memory candidates: %w", err)
	}
	defer rows.Close()

	return scanMemoryItems(rows)
}

func (s *SQLiteStore) SearchMemoryFTS(ctx context.Context, userID, agentID, sessionKey, query string, limit int) ([]MemoryItem, error) {
	_ = sessionKey
	if limit <= 0 {
		limit = 20
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	now := nowMS()
	rows, err := s.db.QueryContext(ctx, `
SELECT m.id, m.user_id, m.agent_id, m.scope_type, m.scope_id, m.session_key, m.kind, m.item_key, m.content, m.confidence, m.weight, m.source_event_id, m.first_seen_at_ms, m.last_seen_at_ms, m.expires_at_ms, m.deleted_at_ms, m.metadata_json
FROM memory_items_fts f
JOIN memory_items m ON m.id = f.item_id
WHERE f.content MATCH ?
AND m.user_id = ?
AND m.agent_id = ?
AND m.deleted_at_ms = 0
AND (m.expires_at_ms = 0 OR m.expires_at_ms > ?)
ORDER BY bm25(memory_items_fts), m.last_seen_at_ms DESC
LIMIT ?`, query, userID, agentID, now, limit)
	if err != nil {
		return nil, fmt.Errorf("search memory fts: %w", err)
	}
	defer rows.Close()

	return scanMemoryItems(rows)
}

func scanMemoryItems(rows *sql.Rows) ([]MemoryItem, error) {
	out := []MemoryItem{}
	for rows.Next() {
		var it MemoryItem
		var kind string
		var scopeType string
		var metaRaw string
		if err := rows.Scan(&it.ID, &it.UserID, &it.AgentID, &scopeType, &it.ScopeID, &it.SessionKey, &kind, &it.Key, &it.Content, &it.Confidence, &it.Weight, &it.SourceEventID, &it.FirstSeenAtMS, &it.LastSeenAtMS, &it.ExpiresAtMS, &it.DeletedAtMS, &metaRaw); err != nil {
			return nil, fmt.Errorf("scan memory item: %w", err)
		}
		it.ScopeType = MemoryScopeType(scopeType)
		it.Kind = MemoryItemKind(kind)
		it.Metadata = decodeMap(metaRaw)
		normalizeMemoryScope(&it)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memory items: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) UpsertMemoryLink(ctx context.Context, link MemoryLink) error {
	if link.ID == "" {
		link.ID = "lnk-" + uuid.NewString()
	}
	if link.CreatedAtMS == 0 {
		link.CreatedAtMS = nowMS()
	}
	if link.Weight == 0 {
		link.Weight = 1
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO memory_links(id, from_item_id, to_item_id, relation, weight, created_at_ms)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(from_item_id, to_item_id, relation) DO UPDATE SET
	weight = excluded.weight`,
		link.ID, link.FromItemID, link.ToItemID, link.Relation, link.Weight, link.CreatedAtMS)
	if err != nil {
		return fmt.Errorf("upsert memory link: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListMemoryLinks(ctx context.Context, itemID string, limit int) ([]MemoryLink, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, from_item_id, to_item_id, relation, weight, created_at_ms
FROM memory_links
WHERE from_item_id = ?
ORDER BY created_at_ms DESC
LIMIT ?`, itemID, limit)
	if err != nil {
		return nil, fmt.Errorf("list memory links: %w", err)
	}
	defer rows.Close()

	out := []MemoryLink{}
	for rows.Next() {
		var l MemoryLink
		if err := rows.Scan(&l.ID, &l.FromItemID, &l.ToItemID, &l.Relation, &l.Weight, &l.CreatedAtMS); err != nil {
			return nil, fmt.Errorf("scan memory link: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memory links: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) ListMemoryObservations(ctx context.Context, itemID string, limit int) ([]MemoryObservation, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, item_id, session_key, event_id, observed_at_ms, confidence, content, extractor, action, metadata_json
FROM memory_observations
WHERE item_id = ?
ORDER BY observed_at_ms DESC
LIMIT ?`, itemID, limit)
	if err != nil {
		return nil, fmt.Errorf("list memory observations: %w", err)
	}
	defer rows.Close()

	out := make([]MemoryObservation, 0, limit)
	for rows.Next() {
		var obs MemoryObservation
		var rawMeta string
		if err := rows.Scan(&obs.ID, &obs.ItemID, &obs.SessionKey, &obs.EventID, &obs.ObservedAt, &obs.Confidence, &obs.Content, &obs.Extractor, &obs.Action, &rawMeta); err != nil {
			return nil, fmt.Errorf("scan memory observation: %w", err)
		}
		obs.Metadata = decodeMap(rawMeta)
		out = append(out, obs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memory observations: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) UpsertEmbedding(ctx context.Context, itemID, model string, vector []float32) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO memory_embeddings(item_id, model, vector_json, norm, updated_at_ms)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(item_id) DO UPDATE SET
	model = excluded.model,
	vector_json = excluded.vector_json,
	norm = excluded.norm,
	updated_at_ms = excluded.updated_at_ms`, itemID, model, encodeVector(vector), vectorNorm(vector), nowMS())
	if err != nil {
		return fmt.Errorf("upsert embedding: %w", err)
	}
	return nil
}

func upsertEmbeddingTx(ctx context.Context, tx *sql.Tx, itemID, model string, vector []float32) error {
	if strings.TrimSpace(itemID) == "" {
		return fmt.Errorf("upsert embedding tx: empty item_id")
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO memory_embeddings(item_id, model, vector_json, norm, updated_at_ms)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(item_id) DO UPDATE SET
	model = excluded.model,
	vector_json = excluded.vector_json,
	norm = excluded.norm,
	updated_at_ms = excluded.updated_at_ms`, itemID, model, encodeVector(vector), vectorNorm(vector), nowMS())
	if err != nil {
		return fmt.Errorf("upsert embedding tx: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetEmbeddings(ctx context.Context, itemIDs []string) (map[string][]float32, error) {
	if len(itemIDs) == 0 {
		return map[string][]float32{}, nil
	}
	ids := uniqueStrings(itemIDs)
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]interface{}, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	query := fmt.Sprintf(`SELECT item_id, vector_json FROM memory_embeddings WHERE item_id IN (%s)`, placeholders)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get embeddings: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]float32, len(ids))
	for rows.Next() {
		var id string
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("scan embedding: %w", err)
		}
		out[id] = decodeVector(raw)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate embeddings: %w", err)
	}
	return out, nil
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func buildMemoryItemFromOp(op ConsolidationOp, ev Event, userID, agentID string, scopeType MemoryScopeType, scopeID string) (MemoryItem, error) {
	if strings.TrimSpace(op.Content) == "" {
		return MemoryItem{}, fmt.Errorf("memory op has empty content for key %q", op.Key)
	}
	conf := clampConfidence(op.Confidence)
	if conf == 0 {
		conf = 0.55
	}
	item := MemoryItem{
		ID:            "mem-" + uuid.NewString(),
		UserID:        userID,
		AgentID:       agentID,
		ScopeType:     scopeType,
		ScopeID:       scopeID,
		SessionKey:    ev.SessionKey,
		Kind:          op.Kind,
		Key:           strings.TrimSpace(op.Key),
		Content:       strings.TrimSpace(op.Content),
		Confidence:    conf,
		Weight:        1.1,
		SourceEventID: ev.ID,
		FirstSeenAtMS: ev.CreatedAt.UnixMilli(),
		LastSeenAtMS:  ev.CreatedAt.UnixMilli(),
		ExpiresAtMS:   0,
		Metadata:      op.Metadata,
	}
	if item.FirstSeenAtMS == 0 {
		item.FirstSeenAtMS = nowMS()
		item.LastSeenAtMS = item.FirstSeenAtMS
	}
	if op.TTL > 0 {
		item.ExpiresAtMS = time.Now().Add(op.TTL).UnixMilli()
	}
	normalizeMemoryScope(&item)
	return item, nil
}

func deriveScopeForOp(kind MemoryItemKind, sessionKey, userID string, meta map[string]string) (MemoryScopeType, string) {
	scopeType := MemoryScopeSession
	scopeID := sessionKey
	if scopeRaw := strings.ToLower(strings.TrimSpace(meta["scope_type"])); scopeRaw != "" {
		switch MemoryScopeType(scopeRaw) {
		case MemoryScopeSession, MemoryScopeUser, MemoryScopeGlobal:
			scopeType = MemoryScopeType(scopeRaw)
		}
	}
	if scopeRaw := strings.TrimSpace(meta["scope_id"]); scopeRaw != "" {
		scopeID = scopeRaw
	}

	switch scopeType {
	case MemoryScopeSession:
		if strings.TrimSpace(scopeID) == "" {
			scopeID = sessionKey
		}
	case MemoryScopeUser:
		if strings.TrimSpace(scopeID) == "" {
			scopeID = userID
		}
	case MemoryScopeGlobal:
		scopeID = ""
	default:
		scopeType = MemoryScopeSession
		scopeID = sessionKey
	}

	if strings.TrimSpace(meta["scope_type"]) == "" {
		switch kind {
		case MemorySemanticFact, MemoryUserPreference, MemoryProcedural:
			scopeType = MemoryScopeUser
			scopeID = userID
		case MemoryTaskState, MemoryEpisodic:
			scopeType = MemoryScopeSession
			scopeID = sessionKey
		}
	}
	if scopeType == MemoryScopeUser && strings.TrimSpace(scopeID) == "" {
		scopeType = MemoryScopeSession
		scopeID = sessionKey
	}
	return scopeType, scopeID
}

func (s *SQLiteStore) GetRetrievalCache(ctx context.Context, key string, nowMS int64) (string, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT result_json, expires_at_ms FROM retrieval_cache WHERE cache_key = ?`, key)
	var payload string
	var expires int64
	if err := row.Scan(&payload, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get retrieval cache: %w", err)
	}
	if expires <= nowMS {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM retrieval_cache WHERE cache_key = ?`, key)
		return "", false, nil
	}
	return payload, true, nil
}

func (s *SQLiteStore) PutRetrievalCache(ctx context.Context, key, value string, expiresAtMS int64) error {
	now := nowMS()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO retrieval_cache(cache_key, result_json, created_at_ms, expires_at_ms)
VALUES(?, ?, ?, ?)
ON CONFLICT(cache_key) DO UPDATE SET
	result_json = excluded.result_json,
	created_at_ms = excluded.created_at_ms,
	expires_at_ms = excluded.expires_at_ms`, key, value, now, expiresAtMS)
	if err != nil {
		return fmt.Errorf("put retrieval cache: %w", err)
	}
	return nil
}

func (s *SQLiteStore) EnqueueJob(ctx context.Context, job Job) error {
	now := nowMS()
	if job.ID == "" {
		job.ID = "job-" + uuid.NewString()
	}
	if job.Status == "" {
		job.Status = JobPending
	}
	if job.Priority == 0 {
		job.Priority = 100
	}
	if job.RunAfterMS == 0 {
		job.RunAfterMS = now
	}
	if job.CreatedAtMS == 0 {
		job.CreatedAtMS = now
	}
	if job.UpdatedAtMS == 0 {
		job.UpdatedAtMS = now
	}

	_, err := s.db.ExecContext(ctx, `
INSERT INTO memory_jobs(id, job_type, session_key, status, priority, payload_json, error, run_after_ms, lease_until_ms, created_at_ms, updated_at_ms, completed_at_ms)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	status = excluded.status,
	priority = excluded.priority,
	payload_json = excluded.payload_json,
	error = excluded.error,
	run_after_ms = excluded.run_after_ms,
	lease_until_ms = excluded.lease_until_ms,
	updated_at_ms = excluded.updated_at_ms,
	completed_at_ms = excluded.completed_at_ms`,
		job.ID,
		job.JobType,
		job.SessionKey,
		job.Status,
		job.Priority,
		encodeMap(job.Payload),
		job.Error,
		job.RunAfterMS,
		job.LeaseUntilMS,
		job.CreatedAtMS,
		job.UpdatedAtMS,
		job.CompletedAtMS,
	)
	if err != nil {
		return fmt.Errorf("enqueue job: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ClaimNextJob(ctx context.Context, nowMS, leaseForMS int64) (Job, bool, error) {
	if leaseForMS <= 0 {
		leaseForMS = 60_000
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, fmt.Errorf("claim next job begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
SELECT id, job_type, session_key, status, priority, payload_json, error, run_after_ms, lease_until_ms, created_at_ms, updated_at_ms, completed_at_ms
FROM memory_jobs
WHERE run_after_ms <= ?
AND (status = ? OR (status = ? AND lease_until_ms <= ?))
ORDER BY priority ASC, created_at_ms ASC
LIMIT 1`, nowMS, JobPending, JobRunning, nowMS)

	var job Job
	var payloadRaw string
	if err := row.Scan(&job.ID, &job.JobType, &job.SessionKey, &job.Status, &job.Priority, &payloadRaw, &job.Error, &job.RunAfterMS, &job.LeaseUntilMS, &job.CreatedAtMS, &job.UpdatedAtMS, &job.CompletedAtMS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, false, nil
		}
		return Job{}, false, fmt.Errorf("claim next job select: %w", err)
	}

	leaseUntil := nowMS + leaseForMS
	res, err := tx.ExecContext(ctx, `
UPDATE memory_jobs
SET status = ?, lease_until_ms = ?, updated_at_ms = ?, error = ''
WHERE id = ? AND (status = ? OR (status = ? AND lease_until_ms <= ?))`, JobRunning, leaseUntil, nowMS, job.ID, JobPending, JobRunning, nowMS)
	if err != nil {
		return Job{}, false, fmt.Errorf("claim next job update: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return Job{}, false, nil
	}

	if err := tx.Commit(); err != nil {
		return Job{}, false, fmt.Errorf("claim next job commit: %w", err)
	}

	job.Status = JobRunning
	job.LeaseUntilMS = leaseUntil
	job.UpdatedAtMS = nowMS
	job.Payload = decodeMap(payloadRaw)
	return job, true, nil
}

func (s *SQLiteStore) CompleteJob(ctx context.Context, id string) error {
	now := nowMS()
	_, err := s.db.ExecContext(ctx, `
UPDATE memory_jobs
SET status = ?, completed_at_ms = ?, updated_at_ms = ?, lease_until_ms = 0
WHERE id = ?`, JobCompleted, now, now, id)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	return nil
}

func (s *SQLiteStore) FailJob(ctx context.Context, id, errMsg string) error {
	now := nowMS()
	_, err := s.db.ExecContext(ctx, `
UPDATE memory_jobs
SET status = ?, error = ?, updated_at_ms = ?, lease_until_ms = 0
WHERE id = ?`, JobFailed, errMsg, now, id)
	if err != nil {
		return fmt.Errorf("fail job: %w", err)
	}
	return nil
}

func (s *SQLiteStore) RequeueExpiredJobs(ctx context.Context, nowMS int64) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE memory_jobs
SET status = ?, updated_at_ms = ?, error = ''
WHERE status = ? AND lease_until_ms > 0 AND lease_until_ms <= ?`, JobPending, nowMS, JobRunning, nowMS)
	if err != nil {
		return fmt.Errorf("requeue expired jobs: %w", err)
	}
	return nil
}

func (s *SQLiteStore) SweepRetention(ctx context.Context, nowMS, eventRetentionMS, auditRetentionMS int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sweep retention begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if eventRetentionMS > 0 {
		cutoff := nowMS - eventRetentionMS
		if _, err := tx.ExecContext(ctx, `
DELETE FROM events
WHERE archived = 1 AND created_at_ms <= ?`, cutoff); err != nil {
			return fmt.Errorf("sweep retention events: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM memory_items
WHERE deleted_at_ms > 0 AND deleted_at_ms <= ?`, nowMS); err != nil {
		return fmt.Errorf("sweep retention deleted memory: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM memory_items
WHERE expires_at_ms > 0 AND expires_at_ms <= ?`, nowMS); err != nil {
		return fmt.Errorf("sweep retention expired memory: %w", err)
	}
	if auditRetentionMS > 0 {
		cutoff := nowMS - auditRetentionMS
		if _, err := tx.ExecContext(ctx, `
DELETE FROM memory_audit_log
WHERE created_at_ms <= ?`, cutoff); err != nil {
			return fmt.Errorf("sweep retention audit log: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM retrieval_cache WHERE expires_at_ms <= ?`, nowMS); err != nil {
		return fmt.Errorf("sweep retention retrieval cache: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sweep retention commit: %w", err)
	}
	return nil
}

func (s *SQLiteStore) AddMetric(ctx context.Context, metric string, value float64, labels map[string]string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO memory_metrics(metric, value, labels_json, created_at_ms)
VALUES(?, ?, ?, ?)`, metric, value, encodeMap(labels), nowMS())
	if err != nil {
		return fmt.Errorf("add metric: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetPersonaProfile(ctx context.Context, userID, agentID string) (PersonaProfile, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT profile_json
FROM persona_profiles
WHERE user_id = ? AND agent_id = ?`, userID, agentID)
	var raw string
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return defaultPersonaProfile(userID, agentID), nil
		}
		return PersonaProfile{}, fmt.Errorf("get persona profile: %w", err)
	}
	return profileFromJSON(raw, userID, agentID), nil
}

func (s *SQLiteStore) UpsertPersonaProfile(ctx context.Context, profile PersonaProfile) error {
	if strings.TrimSpace(profile.UserID) == "" || strings.TrimSpace(profile.AgentID) == "" {
		return fmt.Errorf("upsert persona profile: missing user_id/agent_id")
	}
	if profile.Revision <= 0 {
		profile.Revision = 1
	}
	if profile.UpdatedAtMS <= 0 {
		profile.UpdatedAtMS = nowMS()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO persona_profiles(user_id, agent_id, profile_json, revision, updated_at_ms)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(user_id, agent_id) DO UPDATE SET
	profile_json = excluded.profile_json,
	revision = excluded.revision,
	updated_at_ms = excluded.updated_at_ms`,
		profile.UserID,
		profile.AgentID,
		profileToJSON(profile),
		profile.Revision,
		profile.UpdatedAtMS,
	)
	if err != nil {
		return fmt.Errorf("upsert persona profile: %w", err)
	}
	return nil
}

func (s *SQLiteStore) InsertPersonaCandidates(ctx context.Context, candidates []PersonaUpdateCandidate) error {
	if len(candidates) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("insert persona candidates begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO persona_candidates(
	id, user_id, agent_id, session_key, turn_id, source_event_id,
	field_path, operation, value, confidence, evidence, source, status,
	rejected_reason, applied_revision_id, created_at_ms, applied_at_ms
)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(user_id, agent_id, session_key, turn_id, field_path, operation, value) DO UPDATE SET
	confidence = CASE WHEN excluded.confidence > persona_candidates.confidence THEN excluded.confidence ELSE persona_candidates.confidence END,
	evidence = CASE WHEN excluded.evidence <> '' THEN excluded.evidence ELSE persona_candidates.evidence END,
	source_event_id = CASE WHEN excluded.source_event_id <> '' THEN excluded.source_event_id ELSE persona_candidates.source_event_id END,
	source = CASE WHEN excluded.source <> '' THEN excluded.source ELSE persona_candidates.source END,
	status = CASE WHEN persona_candidates.status = ? THEN persona_candidates.status ELSE excluded.status END`)
	if err != nil {
		return fmt.Errorf("insert persona candidates prepare: %w", err)
	}
	defer stmt.Close()

	for _, c := range candidates {
		if c.ID == "" {
			c.ID = "pcd-" + uuid.NewString()
		}
		if c.Status == "" {
			c.Status = personaCandidatePending
		}
		if c.CreatedAtMS == 0 {
			c.CreatedAtMS = nowMS()
		}
		if _, err := stmt.ExecContext(
			ctx,
			c.ID,
			c.UserID,
			c.AgentID,
			c.SessionKey,
			c.TurnID,
			c.SourceEventID,
			c.FieldPath,
			c.Operation,
			c.Value,
			c.Confidence,
			c.Evidence,
			c.Source,
			c.Status,
			c.RejectedReason,
			c.AppliedRevisionID,
			c.CreatedAtMS,
			c.AppliedAtMS,
			personaCandidateApplied,
		); err != nil {
			return fmt.Errorf("insert persona candidate: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("insert persona candidates commit: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListPersonaCandidates(ctx context.Context, userID, agentID, sessionKey, turnID, status string, limit int) ([]PersonaUpdateCandidate, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, user_id, agent_id, session_key, turn_id, source_event_id,
	field_path, operation, value, confidence, evidence, source, status,
	rejected_reason, applied_revision_id, created_at_ms, applied_at_ms
FROM persona_candidates
WHERE user_id = ? AND agent_id = ?
AND (? = '' OR session_key = ?)
AND (? = '' OR turn_id = ?)
AND (? = '' OR status = ?)
ORDER BY created_at_ms ASC
LIMIT ?`,
		userID, agentID,
		sessionKey, sessionKey,
		turnID, turnID,
		status, status,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list persona candidates: %w", err)
	}
	defer rows.Close()

	out := make([]PersonaUpdateCandidate, 0, limit)
	for rows.Next() {
		var c PersonaUpdateCandidate
		if err := rows.Scan(
			&c.ID,
			&c.UserID,
			&c.AgentID,
			&c.SessionKey,
			&c.TurnID,
			&c.SourceEventID,
			&c.FieldPath,
			&c.Operation,
			&c.Value,
			&c.Confidence,
			&c.Evidence,
			&c.Source,
			&c.Status,
			&c.RejectedReason,
			&c.AppliedRevisionID,
			&c.CreatedAtMS,
			&c.AppliedAtMS,
		); err != nil {
			return nil, fmt.Errorf("scan persona candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate persona candidates: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) UpdatePersonaCandidateStatus(ctx context.Context, id, status, reason, revisionID string, appliedAtMS int64) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("update persona candidate status: empty id")
	}
	if strings.TrimSpace(status) == "" {
		status = personaCandidateRejected
	}
	if appliedAtMS < 0 {
		appliedAtMS = 0
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE persona_candidates
SET status = ?, rejected_reason = ?, applied_revision_id = ?, applied_at_ms = ?
WHERE id = ?`,
		status, reason, revisionID, appliedAtMS, id,
	)
	if err != nil {
		return fmt.Errorf("update persona candidate status: %w", err)
	}
	return nil
}

func (s *SQLiteStore) BumpPersonaSignal(ctx context.Context, userID, agentID, fieldPath, valueHash string, atMS int64) (int, error) {
	if atMS == 0 {
		atMS = nowMS()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("bump persona signal begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
INSERT INTO persona_signals(user_id, agent_id, field_path, value_hash, hits, last_seen_at_ms)
VALUES(?, ?, ?, ?, 1, ?)
ON CONFLICT(user_id, agent_id, field_path, value_hash) DO UPDATE SET
	hits = persona_signals.hits + 1,
	last_seen_at_ms = excluded.last_seen_at_ms`,
		userID, agentID, fieldPath, valueHash, atMS,
	)
	if err != nil {
		return 0, fmt.Errorf("bump persona signal upsert: %w", err)
	}
	row := tx.QueryRowContext(ctx, `
SELECT hits
FROM persona_signals
WHERE user_id = ? AND agent_id = ? AND field_path = ? AND value_hash = ?`,
		userID, agentID, fieldPath, valueHash,
	)
	var hits int
	if err := row.Scan(&hits); err != nil {
		return 0, fmt.Errorf("bump persona signal read: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("bump persona signal commit: %w", err)
	}
	return hits, nil
}

func (s *SQLiteStore) InsertPersonaRevision(ctx context.Context, rev PersonaRevision) error {
	if rev.ID == "" {
		rev.ID = "prv-" + uuid.NewString()
	}
	if rev.CreatedAtMS == 0 {
		rev.CreatedAtMS = nowMS()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO persona_revisions(
	id, user_id, agent_id, session_key, turn_id, candidate_id, field_path, operation,
	old_value, new_value, confidence, evidence, reason, source,
	profile_before_json, profile_after_json, created_at_ms
)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rev.ID,
		rev.UserID,
		rev.AgentID,
		rev.SessionKey,
		rev.TurnID,
		rev.CandidateID,
		rev.FieldPath,
		rev.Operation,
		rev.OldValue,
		rev.NewValue,
		rev.Confidence,
		rev.Evidence,
		rev.Reason,
		rev.Source,
		rev.ProfileBeforeJSON,
		rev.ProfileAfterJSON,
		rev.CreatedAtMS,
	)
	if err != nil {
		return fmt.Errorf("insert persona revision: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListPersonaRevisions(ctx context.Context, userID, agentID string, limit int) ([]PersonaRevision, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, user_id, agent_id, session_key, turn_id, candidate_id, field_path, operation,
	old_value, new_value, confidence, evidence, reason, source,
	profile_before_json, profile_after_json, created_at_ms
FROM persona_revisions
WHERE user_id = ? AND agent_id = ?
ORDER BY created_at_ms DESC
LIMIT ?`, userID, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("list persona revisions: %w", err)
	}
	defer rows.Close()

	out := make([]PersonaRevision, 0, limit)
	for rows.Next() {
		var rev PersonaRevision
		if err := rows.Scan(
			&rev.ID,
			&rev.UserID,
			&rev.AgentID,
			&rev.SessionKey,
			&rev.TurnID,
			&rev.CandidateID,
			&rev.FieldPath,
			&rev.Operation,
			&rev.OldValue,
			&rev.NewValue,
			&rev.Confidence,
			&rev.Evidence,
			&rev.Reason,
			&rev.Source,
			&rev.ProfileBeforeJSON,
			&rev.ProfileAfterJSON,
			&rev.CreatedAtMS,
		); err != nil {
			return nil, fmt.Errorf("scan persona revision: %w", err)
		}
		out = append(out, rev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate persona revisions: %w", err)
	}
	return out, nil
}

// ApplyPersonaMutation atomically writes profile + revision + candidate status + memory links.
func (s *SQLiteStore) ApplyPersonaMutation(ctx context.Context, profile PersonaProfile, candidate PersonaUpdateCandidate, revision PersonaRevision, memoryOps []ConsolidationOp) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("apply persona mutation begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if profile.Revision <= 0 {
		profile.Revision = 1
	}
	if profile.UpdatedAtMS <= 0 {
		profile.UpdatedAtMS = nowMS()
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO persona_profiles(user_id, agent_id, profile_json, revision, updated_at_ms)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(user_id, agent_id) DO UPDATE SET
	profile_json = excluded.profile_json,
	revision = excluded.revision,
	updated_at_ms = excluded.updated_at_ms`,
		profile.UserID,
		profile.AgentID,
		profileToJSON(profile),
		profile.Revision,
		profile.UpdatedAtMS,
	); err != nil {
		return fmt.Errorf("apply persona mutation profile: %w", err)
	}

	if revision.ID == "" {
		revision.ID = "prv-" + uuid.NewString()
	}
	if revision.CreatedAtMS == 0 {
		revision.CreatedAtMS = nowMS()
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO persona_revisions(
	id, user_id, agent_id, session_key, turn_id, candidate_id, field_path, operation,
	old_value, new_value, confidence, evidence, reason, source,
	profile_before_json, profile_after_json, created_at_ms
)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		revision.ID,
		revision.UserID,
		revision.AgentID,
		revision.SessionKey,
		revision.TurnID,
		revision.CandidateID,
		revision.FieldPath,
		revision.Operation,
		revision.OldValue,
		revision.NewValue,
		revision.Confidence,
		revision.Evidence,
		revision.Reason,
		revision.Source,
		revision.ProfileBeforeJSON,
		revision.ProfileAfterJSON,
		revision.CreatedAtMS,
	); err != nil {
		return fmt.Errorf("apply persona mutation revision: %w", err)
	}

	if candidate.ID != "" {
		appliedAt := nowMS()
		if _, err := tx.ExecContext(ctx, `
UPDATE persona_candidates
SET status = ?, rejected_reason = '', applied_revision_id = ?, applied_at_ms = ?
WHERE id = ?`, personaCandidateApplied, revision.ID, appliedAt, candidate.ID); err != nil {
			return fmt.Errorf("apply persona mutation candidate status: %w", err)
		}
	}

	if len(memoryOps) > 0 {
		rootKey := "persona/profile"
		rootContent := "Persona profile revision: " + fmt.Sprintf("%d", profile.Revision)
		rootID, err := upsertMemoryItemTx(ctx, tx, MemoryItem{
			ID:            "mem-" + uuid.NewString(),
			UserID:        profile.UserID,
			AgentID:       profile.AgentID,
			ScopeType:     MemoryScopeUser,
			ScopeID:       profile.UserID,
			SessionKey:    candidate.SessionKey,
			Kind:          MemoryProcedural,
			Key:           rootKey,
			Content:       rootContent,
			Confidence:    0.9,
			Weight:        1.0,
			SourceEventID: candidate.SourceEventID,
			FirstSeenAtMS: nowMS(),
			LastSeenAtMS:  nowMS(),
			Metadata:      map[string]string{"source": "persona"},
		})
		if err != nil {
			return fmt.Errorf("apply persona mutation root memory item: %w", err)
		}

		for _, op := range memoryOps {
			if op.Action == "delete" {
				if _, err := tx.ExecContext(ctx, `
UPDATE memory_items
SET deleted_at_ms = ?
WHERE user_id = ? AND agent_id = ? AND kind = ?
AND (item_key = ? OR item_key LIKE ?)`,
					nowMS(), profile.UserID, profile.AgentID, string(op.Kind), op.Key, op.Key+"/%"); err != nil {
					return fmt.Errorf("apply persona mutation delete memory: %w", err)
				}
				continue
			}

			memID, err := upsertMemoryItemTx(ctx, tx, MemoryItem{
				ID:            "mem-" + uuid.NewString(),
				UserID:        profile.UserID,
				AgentID:       profile.AgentID,
				ScopeType:     MemoryScopeUser,
				ScopeID:       profile.UserID,
				SessionKey:    candidate.SessionKey,
				Kind:          op.Kind,
				Key:           op.Key,
				Content:       op.Content,
				Confidence:    op.Confidence,
				Weight:        1.0,
				SourceEventID: candidate.SourceEventID,
				FirstSeenAtMS: nowMS(),
				LastSeenAtMS:  nowMS(),
				Metadata:      op.Metadata,
			})
			if err != nil {
				return fmt.Errorf("apply persona mutation memory item: %w", err)
			}

			if rootID != "" && rootID != memID {
				linkID := "lnk-" + uuid.NewString()
				if _, err := tx.ExecContext(ctx, `
INSERT INTO memory_links(id, from_item_id, to_item_id, relation, weight, created_at_ms)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(from_item_id, to_item_id, relation) DO UPDATE SET
	weight = excluded.weight`,
					linkID, rootID, memID, "persona_field", 1.0, nowMS(),
				); err != nil {
					return fmt.Errorf("apply persona mutation memory link: %w", err)
				}
			}
		}
	}
	if err := invalidateRetrievalCacheTx(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("apply persona mutation commit: %w", err)
	}
	return nil
}

func upsertMemoryItemTx(ctx context.Context, tx *sql.Tx, item MemoryItem) (string, error) {
	if item.ID == "" {
		item.ID = "mem-" + uuid.NewString()
	}
	if item.AgentID == "" {
		item.AgentID = "dotagent"
	}
	if item.Key == "" {
		item.Key = strings.ToLower(strings.TrimSpace(item.Content))
	}
	if item.FirstSeenAtMS == 0 {
		item.FirstSeenAtMS = nowMS()
	}
	if item.LastSeenAtMS == 0 {
		item.LastSeenAtMS = nowMS()
	}
	normalizeMemoryScope(&item)
	if item.ScopeType == MemoryScopeSession && strings.TrimSpace(item.SessionKey) == "" {
		item.SessionKey = item.ScopeID
	}
	item.Confidence = clampConfidence(item.Confidence)
	if item.Weight <= 0 {
		item.Weight = 1
	}

	var existingID string
	var existingContent string
	var existingConfidence float64
	var existingWeight float64
	var existingSource string
	var existingSession string
	var existingMetaMap map[string]string
	var existingMeta string
	row := tx.QueryRowContext(ctx, `
SELECT id, content, confidence, weight, source_event_id, session_key, metadata_json
FROM memory_items
WHERE user_id = ? AND agent_id = ? AND scope_type = ? AND scope_id = ? AND kind = ? AND item_key = ?`,
		item.UserID, item.AgentID, string(item.ScopeType), item.ScopeID, string(item.Kind), item.Key,
	)
	switch err := row.Scan(&existingID, &existingContent, &existingConfidence, &existingWeight, &existingSource, &existingSession, &existingMeta); {
	case err == nil:
		confidence := existingConfidence
		if item.Confidence > confidence {
			confidence = item.Confidence
		}
		existingMetaMap = decodeMap(existingMeta)
		weight := existingWeight
		if weight == 0 {
			weight = item.Weight
		} else if item.Weight > 0 {
			weight = (existingWeight + item.Weight) / 2.0
		}
		source := existingSource
		if strings.TrimSpace(item.SourceEventID) != "" {
			source = item.SourceEventID
		}
		session := existingSession
		if strings.TrimSpace(item.SessionKey) != "" {
			session = item.SessionKey
		}
		metaMap := existingMetaMap
		if metaMap == nil {
			metaMap = map[string]string{}
		}
		if len(item.Metadata) > 0 {
			for k, v := range item.Metadata {
				metaMap[k] = v
			}
		}
		content, changed := chooseMemoryContent(existingContent, item.Content, existingConfidence, item.Confidence, item.Kind, item.Key)
		if changed {
			metaMap["content_changed_at_ms"] = fmt.Sprintf("%d", nowMS())
		}
		meta := encodeMap(metaMap)
		if _, err := tx.ExecContext(ctx, `
UPDATE memory_items
SET content = ?, session_key = ?, confidence = ?, weight = ?, source_event_id = ?, last_seen_at_ms = ?, expires_at_ms = ?, deleted_at_ms = 0, metadata_json = ?
WHERE id = ?`,
			content,
			session,
			confidence,
			weight,
			source,
			item.LastSeenAtMS,
			item.ExpiresAtMS,
			meta,
			existingID,
		); err != nil {
			return "", fmt.Errorf("update memory_items existing id=%s key=%s scope=%s/%s: %w", existingID, item.Key, item.ScopeType, item.ScopeID, err)
		}
		if obsErr := insertMemoryObservationTx(ctx, tx, existingID, item, "upsert"); obsErr != nil {
			return "", obsErr
		}
		_ = insertAuditLogTx(ctx, tx, "memory_upsert", "memory_item", existingID, item.SessionKey, item.UserID, item.AgentID, "update", map[string]string{
			"kind":      string(item.Kind),
			"item_key":  item.Key,
			"scope":     string(item.ScopeType),
			"scope_id":  item.ScopeID,
			"event_id":  item.SourceEventID,
			"extractor": item.Metadata["extractor"],
		})
		return existingID, nil
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
INSERT INTO memory_items(id, user_id, agent_id, scope_type, scope_id, session_key, kind, item_key, content, confidence, weight, source_event_id, first_seen_at_ms, last_seen_at_ms, expires_at_ms, deleted_at_ms, metadata_json)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
			item.ID,
			item.UserID,
			item.AgentID,
			string(item.ScopeType),
			item.ScopeID,
			item.SessionKey,
			string(item.Kind),
			item.Key,
			item.Content,
			item.Confidence,
			item.Weight,
			item.SourceEventID,
			item.FirstSeenAtMS,
			item.LastSeenAtMS,
			item.ExpiresAtMS,
			encodeMap(item.Metadata),
		); err != nil {
			return "", fmt.Errorf("insert memory_items id=%s key=%s scope=%s/%s: %w", item.ID, item.Key, item.ScopeType, item.ScopeID, err)
		}
		if obsErr := insertMemoryObservationTx(ctx, tx, item.ID, item, "insert"); obsErr != nil {
			return "", obsErr
		}
		_ = insertAuditLogTx(ctx, tx, "memory_upsert", "memory_item", item.ID, item.SessionKey, item.UserID, item.AgentID, "insert", map[string]string{
			"kind":      string(item.Kind),
			"item_key":  item.Key,
			"scope":     string(item.ScopeType),
			"scope_id":  item.ScopeID,
			"event_id":  item.SourceEventID,
			"extractor": item.Metadata["extractor"],
		})
		return item.ID, nil
	default:
		return "", err
	}
}

func clampConfidence(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func normalizeMemoryScope(item *MemoryItem) {
	if item == nil {
		return
	}
	if item.ScopeType == "" {
		if strings.TrimSpace(item.SessionKey) == "" {
			item.ScopeType = MemoryScopeGlobal
		} else {
			item.ScopeType = MemoryScopeSession
		}
	}
	switch item.ScopeType {
	case MemoryScopeSession:
		if strings.TrimSpace(item.ScopeID) == "" {
			item.ScopeID = item.SessionKey
		}
		if strings.TrimSpace(item.SessionKey) == "" {
			item.SessionKey = item.ScopeID
		}
	case MemoryScopeUser:
		if strings.TrimSpace(item.ScopeID) == "" {
			item.ScopeID = item.UserID
		}
		if strings.TrimSpace(item.ScopeID) == "" && strings.TrimSpace(item.SessionKey) != "" {
			item.ScopeID = item.SessionKey
		}
	case MemoryScopeGlobal:
		item.ScopeID = ""
	}
}

func chooseMemoryContent(existing, incoming string, existingConfidence, incomingConfidence float64, kind MemoryItemKind, key string) (string, bool) {
	existing = strings.TrimSpace(existing)
	incoming = strings.TrimSpace(incoming)
	if existing == "" {
		return incoming, incoming != ""
	}
	if incoming == "" {
		return existing, false
	}
	if strings.EqualFold(existing, incoming) {
		return incoming, !strings.EqualFold(existing, incoming)
	}

	singletonKey := strings.HasPrefix(key, "identity/") || strings.HasPrefix(key, "profile/")
	if singletonKey || kind == MemoryTaskState {
		if incomingConfidence+0.05 >= existingConfidence {
			return incoming, true
		}
		return existing, false
	}

	overlap := textTokenJaccard(existing, incoming)
	if overlap < 0.2 && existingConfidence > incomingConfidence+0.15 {
		return existing, false
	}
	if incomingConfidence > existingConfidence+0.05 {
		return incoming, true
	}
	if len(incoming) > len(existing)+8 {
		return incoming, true
	}
	return existing, false
}

func textTokenJaccard(a, b string) float64 {
	aSet := tokenSet(a)
	bSet := tokenSet(b)
	if len(aSet) == 0 || len(bSet) == 0 {
		return 0
	}
	inter := 0
	union := len(aSet)
	for tok := range bSet {
		if _, ok := aSet[tok]; ok {
			inter++
		} else {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func tokenSet(in string) map[string]struct{} {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(in)))
	out := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		f = strings.Trim(f, ".,!?;:\"'()[]{}")
		if len(f) < 2 {
			continue
		}
		out[f] = struct{}{}
	}
	return out
}

func insertMemoryObservationTx(ctx context.Context, tx *sql.Tx, itemID string, item MemoryItem, action string) error {
	content := strings.TrimSpace(item.Content)
	if strings.TrimSpace(item.SourceEventID) == "" && content == "" {
		return nil
	}
	meta := map[string]string{}
	for k, v := range item.Metadata {
		meta[k] = v
	}
	extractor := strings.TrimSpace(meta["extractor"])
	if extractor == "" {
		extractor = "unknown"
	}
	obs := MemoryObservation{
		ID:         "obs-" + uuid.NewString(),
		ItemID:     itemID,
		SessionKey: item.SessionKey,
		EventID:    item.SourceEventID,
		ObservedAt: item.LastSeenAtMS,
		Confidence: clampConfidence(item.Confidence),
		Content:    truncateForMetadata(content, 400),
		Metadata:   meta,
		Extractor:  extractor,
		Action:     nonEmptyString(action, "upsert"),
	}
	if obs.ObservedAt == 0 {
		obs.ObservedAt = nowMS()
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO memory_observations(id, item_id, session_key, event_id, observed_at_ms, confidence, content, extractor, action, metadata_json)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		obs.ID, obs.ItemID, obs.SessionKey, obs.EventID, obs.ObservedAt, obs.Confidence, obs.Content, obs.Extractor, obs.Action, encodeMap(obs.Metadata),
	); err != nil {
		return fmt.Errorf("insert memory observation: %w", err)
	}
	return nil
}

func nonEmptyString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func (s *SQLiteStore) RollbackPersonaToRevision(ctx context.Context, userID, agentID, revisionID string) (PersonaProfile, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT profile_before_json
FROM persona_revisions
WHERE id = ? AND user_id = ? AND agent_id = ?`, revisionID, userID, agentID)
	var beforeRaw string
	if err := row.Scan(&beforeRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PersonaProfile{}, fmt.Errorf("rollback persona: revision not found")
		}
		return PersonaProfile{}, fmt.Errorf("rollback persona: %w", err)
	}
	profile := profileFromJSON(beforeRaw, userID, agentID)
	profile.Revision++
	profile.UpdatedAtMS = nowMS()
	if err := s.UpsertPersonaProfile(ctx, profile); err != nil {
		return PersonaProfile{}, err
	}
	return profile, nil
}
