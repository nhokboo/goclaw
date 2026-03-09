package store

import "context"

// ChatMigrationStore handles chat ID migration when platforms (e.g. Telegram)
// change group IDs (group → supergroup migration).
type ChatMigrationStore interface {
	// MigrateGroupChatID atomically updates all references from oldChatID to newChatID
	// for the given channel. This includes sessions, cron jobs, group file writers,
	// memory, context files, handoff routes, pairing, and pending messages.
	MigrateGroupChatID(ctx context.Context, channel string, oldChatID, newChatID int64) error
}
