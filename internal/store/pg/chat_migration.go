package pg

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// PGChatMigrationStore implements ChatMigrationStore using a single transaction
// across all affected tables.
type PGChatMigrationStore struct {
	db *sql.DB
}

func NewPGChatMigrationStore(db *sql.DB) *PGChatMigrationStore {
	return &PGChatMigrationStore{db: db}
}

// MigrateGroupChatID atomically migrates all references from oldChatID to newChatID.
//
// Affected data patterns:
//   - session_key contains chat ID as segment: "agent:{id}:{channel}:group:{chatID}"
//   - user_id uses format "group:{channel}:{chatID}" in sessions, cron_jobs, user_context_files, memory_documents
//   - cron_jobs.payload JSONB has "to" field with raw chat ID
//   - group_file_writers.group_id uses "group:{channel}:{chatID}"
//   - handoff_routes.chat_id stores raw chat ID string
//   - pairing_requests/paired_devices store raw chat ID string
//   - pending_messages store raw chat ID string
func (s *PGChatMigrationStore) MigrateGroupChatID(ctx context.Context, channel string, oldChatID, newChatID int64) error {
	oldStr := fmt.Sprintf("%d", oldChatID)
	newStr := fmt.Sprintf("%d", newChatID)
	oldGroupUserID := fmt.Sprintf("group:%s:%s", channel, oldStr)
	newGroupUserID := fmt.Sprintf("group:%s:%s", channel, newStr)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer tx.Rollback()

	// Track rows affected for logging
	var totalRows int64

	// 1. sessions: update session_key and user_id
	// session_key contains chatID as a segment, e.g. "agent:friday:telegram:group:-123456"
	res, err := tx.ExecContext(ctx,
		`UPDATE sessions SET
			session_key = REPLACE(session_key, $1, $2),
			user_id = REPLACE(user_id, $3, $4),
			updated_at = NOW()
		WHERE session_key LIKE '%' || $1 || '%'
		   OR user_id = $3`,
		oldStr, newStr, oldGroupUserID, newGroupUserID)
	if err != nil {
		return fmt.Errorf("migrate sessions: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		totalRows += n
		slog.Info("telegram.migration: sessions updated", "count", n)
	}

	// 2. cron_jobs: update user_id and payload "to" field
	res, err = tx.ExecContext(ctx,
		`UPDATE cron_jobs SET
			user_id = REPLACE(user_id, $1, $2),
			payload = jsonb_set(payload, '{to}', to_jsonb($4::text)),
			updated_at = NOW()
		WHERE user_id = $1
		   OR payload->>'to' = $3`,
		oldGroupUserID, newGroupUserID, oldStr, newStr)
	if err != nil {
		return fmt.Errorf("migrate cron_jobs: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		totalRows += n
		slog.Info("telegram.migration: cron_jobs updated", "count", n)
	}

	// 3. group_file_writers: update group_id
	res, err = tx.ExecContext(ctx,
		`UPDATE group_file_writers SET group_id = $2 WHERE group_id = $1`,
		oldGroupUserID, newGroupUserID)
	if err != nil {
		return fmt.Errorf("migrate group_file_writers: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		totalRows += n
		slog.Info("telegram.migration: group_file_writers updated", "count", n)
	}

	// 4. user_context_files: update user_id
	res, err = tx.ExecContext(ctx,
		`UPDATE user_context_files SET user_id = $2 WHERE user_id = $1`,
		oldGroupUserID, newGroupUserID)
	if err != nil {
		return fmt.Errorf("migrate user_context_files: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		totalRows += n
		slog.Info("telegram.migration: user_context_files updated", "count", n)
	}

	// 5. memory_documents: update user_id
	res, err = tx.ExecContext(ctx,
		`UPDATE memory_documents SET user_id = $2 WHERE user_id = $1`,
		oldGroupUserID, newGroupUserID)
	if err != nil {
		return fmt.Errorf("migrate memory_documents: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		totalRows += n
		slog.Info("telegram.migration: memory_documents updated", "count", n)
	}

	// 6. handoff_routes: update chat_id where channel matches
	res, err = tx.ExecContext(ctx,
		`UPDATE handoff_routes SET chat_id = $2 WHERE channel = $3 AND chat_id = $1`,
		oldStr, newStr, channel)
	if err != nil {
		return fmt.Errorf("migrate handoff_routes: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		totalRows += n
		slog.Info("telegram.migration: handoff_routes updated", "count", n)
	}

	// 7. pairing_requests: update chat_id where channel matches
	res, err = tx.ExecContext(ctx,
		`UPDATE pairing_requests SET chat_id = $2 WHERE channel = $3 AND chat_id = $1`,
		oldStr, newStr, channel)
	if err != nil {
		return fmt.Errorf("migrate pairing_requests: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		totalRows += n
		slog.Info("telegram.migration: pairing_requests updated", "count", n)
	}

	// 8. paired_devices: update chat_id where channel matches
	res, err = tx.ExecContext(ctx,
		`UPDATE paired_devices SET chat_id = $2 WHERE channel = $3 AND chat_id = $1`,
		oldStr, newStr, channel)
	if err != nil {
		return fmt.Errorf("migrate paired_devices: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		totalRows += n
		slog.Info("telegram.migration: paired_devices updated", "count", n)
	}

	// 9. pending_messages: update chat_id where channel matches
	res, err = tx.ExecContext(ctx,
		`UPDATE pending_messages SET chat_id = $2 WHERE channel = $3 AND chat_id = $1`,
		oldStr, newStr, channel)
	if err != nil {
		return fmt.Errorf("migrate pending_messages: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		totalRows += n
		slog.Info("telegram.migration: pending_messages updated", "count", n)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}

	slog.Info("telegram.migration: completed",
		"channel", channel,
		"old_chat_id", oldChatID,
		"new_chat_id", newChatID,
		"total_rows", totalRows,
	)

	return nil
}
