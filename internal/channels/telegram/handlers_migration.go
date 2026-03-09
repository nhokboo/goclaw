package telegram

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mymmrac/telego"
)

// handleGroupMigration processes a Telegram group → supergroup migration event.
// Updates all DB references from oldChatID to newChatID and transfers in-memory state.
func (c *Channel) handleGroupMigration(ctx context.Context, oldChatID, newChatID int64) {
	channelName := c.Name()

	slog.Warn("telegram.group_migration: detected",
		"channel", channelName,
		"old_chat_id", oldChatID,
		"new_chat_id", newChatID,
	)

	// 1. Migrate all DB references atomically
	if c.chatMigration != nil {
		if err := c.chatMigration.MigrateGroupChatID(ctx, channelName, oldChatID, newChatID); err != nil {
			slog.Error("telegram.group_migration: DB migration failed",
				"channel", channelName,
				"old_chat_id", oldChatID,
				"new_chat_id", newChatID,
				"error", err,
			)
			// Continue with in-memory cleanup even if DB migration fails —
			// the old chat ID won't receive new messages anyway.
		}
	} else {
		slog.Warn("telegram.group_migration: no migration store configured, DB references not updated",
			"old_chat_id", oldChatID,
			"new_chat_id", newChatID,
		)
	}

	// 2. Transfer in-memory pairing approval cache
	oldKey := fmt.Sprintf("%d", oldChatID)
	newKey := fmt.Sprintf("%d", newChatID)
	if val, loaded := c.approvedGroups.LoadAndDelete(oldKey); loaded {
		c.approvedGroups.Store(newKey, val)
	}

	// 3. Clear pending group history for the old chat ID
	c.groupHistory.Clear(oldKey)

	slog.Info("telegram.group_migration: completed",
		"channel", channelName,
		"old_chat_id", oldChatID,
		"new_chat_id", newChatID,
	)
}

// handleMyChatMember processes bot membership changes (added/removed/promoted/demoted).
// Logs the event for diagnostics — actual migration is handled via MigrateToChatID messages.
func (c *Channel) handleMyChatMember(update *telego.ChatMemberUpdated) {
	if update == nil {
		return
	}

	oldStatus := "unknown"
	newStatus := "unknown"
	if update.OldChatMember != nil {
		oldStatus = update.OldChatMember.MemberStatus()
	}
	if update.NewChatMember != nil {
		newStatus = update.NewChatMember.MemberStatus()
	}

	slog.Info("telegram.my_chat_member",
		"chat_id", update.Chat.ID,
		"chat_type", update.Chat.Type,
		"chat_title", update.Chat.Title,
		"old_status", oldStatus,
		"new_status", newStatus,
	)
}
