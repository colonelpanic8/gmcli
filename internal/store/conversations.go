package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Conversation is the storage shape for a chat thread. participants_json
// holds the libgm Participant array as JSON to avoid a join table at this
// stage — Phase 3 may normalize if query patterns demand it.
type Conversation struct {
	ID                string    `json:"conversation_id"`
	SourcePlatform    string    `json:"source_platform"`
	Name              string    `json:"name"`
	IsGroup           bool      `json:"is_group"`
	ParticipantsJSON  string    `json:"participants_json"`
	LastMessageTimeMS int64     `json:"last_message_time_ms"`
	Unread            bool      `json:"unread"`
	Pinned            bool      `json:"pinned"`
	Archived          bool      `json:"archived"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// UpsertConversation inserts or updates a conversation row by ID.
func (s *Store) UpsertConversation(ctx context.Context, c Conversation) error {
	if c.ID == "" {
		return fmt.Errorf("conversation id is required")
	}
	platform := c.SourcePlatform
	if platform == "" {
		platform = "gm"
	}
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin conversation upsert: %w", err)
	}
	defer tx.Rollback()

	existing, err := scanConversation(tx.QueryRowContext(ctx, `
		SELECT conversation_id, source_platform, name, is_group, participants_json,
		       last_message_ts, unread, pinned, archived, updated_at
		  FROM conversations
		 WHERE conversation_id = ?`, c.ID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read existing conversation %s: %w", c.ID, err)
	}
	if err == nil && conversationIdentityChanged(existing, c) {
		var messages int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, c.ID).Scan(&messages); err != nil {
			return fmt.Errorf("count messages for conversation %s: %w", c.ID, err)
		}
		if messages > 0 {
			if err := preserveReusedConversationID(ctx, tx, existing); err != nil {
				return err
			}
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO conversations (
			conversation_id, source_platform, name, is_group, participants_json,
			last_message_ts, unread, pinned, archived, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(conversation_id) DO UPDATE SET
			source_platform   = excluded.source_platform,
			name              = excluded.name,
			is_group          = excluded.is_group,
			participants_json = excluded.participants_json,
			last_message_ts   = MAX(conversations.last_message_ts, excluded.last_message_ts),
			unread            = excluded.unread,
			pinned            = excluded.pinned,
			archived          = excluded.archived,
			updated_at        = excluded.updated_at
	`,
		c.ID, platform, c.Name, boolToInt(c.IsGroup), nullableJSON(c.ParticipantsJSON),
		c.LastMessageTimeMS, boolToInt(c.Unread), boolToInt(c.Pinned), boolToInt(c.Archived), now,
	)
	if err != nil {
		return fmt.Errorf("upsert conversation %s: %w", c.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit conversation %s: %w", c.ID, err)
	}
	return nil
}

type storedParticipant struct {
	E164 string `json:"e164"`
	IsMe bool   `json:"is_me"`
}

func conversationIdentityChanged(existing, incoming Conversation) bool {
	if existing.SourcePlatform != "gm" && existing.SourcePlatform != "" {
		return false
	}
	oldNumbers := participantNumbers(existing.ParticipantsJSON)
	newNumbers := participantNumbers(incoming.ParticipantsJSON)
	if len(oldNumbers) == 0 || len(newNumbers) == 0 {
		return false
	}
	if existing.IsGroup != incoming.IsGroup {
		return true
	}
	if !existing.IsGroup {
		return strings.Join(oldNumbers, "\x00") != strings.Join(newNumbers, "\x00")
	}
	oldSet := make(map[string]bool, len(oldNumbers))
	for _, number := range oldNumbers {
		oldSet[number] = true
	}
	for _, number := range newNumbers {
		if oldSet[number] {
			return false
		}
	}
	return true
}

func participantNumbers(raw string) []string {
	var participants []storedParticipant
	if json.Unmarshal([]byte(raw), &participants) != nil {
		return nil
	}
	seen := make(map[string]bool)
	numbers := make([]string, 0, len(participants))
	for _, participant := range participants {
		number := strings.TrimSpace(participant.E164)
		if participant.IsMe || number == "" || seen[number] {
			continue
		}
		seen[number] = true
		numbers = append(numbers, number)
	}
	sort.Strings(numbers)
	return numbers
}

func preserveReusedConversationID(ctx context.Context, tx *sql.Tx, existing Conversation) error {
	digest := sha256.Sum256([]byte(existing.ID + "\x00" + existing.ParticipantsJSON))
	legacyID := "legacy:" + existing.ID + ":" + hex.EncodeToString(digest[:8])
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO conversations (
			conversation_id, source_platform, name, is_group, participants_json,
			last_message_ts, unread, pinned, archived, updated_at
		) SELECT ?, 'gm-legacy', name, is_group, participants_json,
		         last_message_ts, unread, pinned, archived, updated_at
		    FROM conversations WHERE conversation_id = ?`, legacyID, existing.ID); err != nil {
		return fmt.Errorf("preserve reused conversation %s as %s: %w", existing.ID, legacyID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET conversation_id = ? WHERE conversation_id = ?`, legacyID, existing.ID); err != nil {
		return fmt.Errorf("move messages for reused conversation %s: %w", existing.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO conversation_coverage (
			conversation_id, status, history_start_ms, last_attempt_ts, last_success_ts,
			exhausted_at, terminal_reason, last_error, last_requests,
			last_records_fetched, updated_at
		) SELECT ?, status, history_start_ms, last_attempt_ts, last_success_ts,
		         exhausted_at, terminal_reason, last_error, last_requests,
		         last_records_fetched, updated_at
		    FROM conversation_coverage WHERE conversation_id = ?`, legacyID, existing.ID); err != nil {
		return fmt.Errorf("preserve coverage for reused conversation %s: %w", existing.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO conversation_coverage_segments (conversation_id, start_ms, end_ms, verified_at)
		SELECT ?, start_ms, end_ms, verified_at
		  FROM conversation_coverage_segments WHERE conversation_id = ?`, legacyID, existing.ID); err != nil {
		return fmt.Errorf("preserve coverage segments for reused conversation %s: %w", existing.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_coverage_segments WHERE conversation_id = ?`, existing.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_coverage WHERE conversation_id = ?`, existing.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE aliases SET target_id = ? WHERE target_type = 'conversation' AND target_id = ?`, legacyID, existing.ID); err != nil {
		return fmt.Errorf("preserve alias for reused conversation %s: %w", existing.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE conversation_id = ?`, existing.ID); err != nil {
		return fmt.Errorf("replace reused conversation %s: %w", existing.ID, err)
	}
	return nil
}

// GetConversation fetches a single row. Returns sql.ErrNoRows on miss.
func (s *Store) GetConversation(ctx context.Context, id string) (Conversation, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT conversation_id, source_platform, name, is_group, participants_json,
		       last_message_ts, unread, pinned, archived, updated_at
		  FROM conversations
		 WHERE conversation_id = ?
	`, id)
	return scanConversation(row)
}

// CountConversations returns the total number of stored conversations.
func (s *Store) CountConversations(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations`).Scan(&n)
	return n, err
}

// ListConversationOpts filters and paginates ListConversations.
type ListConversationOpts struct {
	Limit      int  // max rows; <=0 means 50
	UnreadOnly bool // only conversations with unread=1
	Pinned     bool // only pinned threads
}

// ListConversations returns conversations ordered by last_message_ts DESC.
func (s *Store) ListConversations(ctx context.Context, opts ListConversationOpts) ([]Conversation, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	q := `
		SELECT conversation_id, source_platform, name, is_group, participants_json,
		       last_message_ts, unread, pinned, archived, updated_at
		  FROM conversations`
	var clauses []string
	if opts.UnreadOnly {
		clauses = append(clauses, "unread = 1")
	}
	if opts.Pinned {
		clauses = append(clauses, "pinned = 1")
	}
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY last_message_ts DESC, updated_at DESC LIMIT ?"
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()
	out := make([]Conversation, 0)
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListConversationsByID returns active phone conversations in stable ID order.
// Preserved fragments from a previously paired phone remain exportable but
// cannot be requested from the current phone by their synthetic local IDs.
func (s *Store) ListConversationsByID(ctx context.Context) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT conversation_id, source_platform, name, is_group, participants_json,
		       last_message_ts, unread, pinned, archived, updated_at
		  FROM conversations
		 WHERE source_platform = 'gm'
		 ORDER BY conversation_id`)
	if err != nil {
		return nil, fmt.Errorf("list conversations by ID: %w", err)
	}
	defer rows.Close()
	out := make([]Conversation, 0)
	for rows.Next() {
		conversation, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, conversation)
	}
	return out, rows.Err()
}

// scanConversation reads a single row in the canonical column order. Used by
// both GetConversation and ListConversations.
func scanConversation(r interface {
	Scan(...any) error
}) (Conversation, error) {
	var c Conversation
	var lastMsg, updated, isGroup, unread, pinned, archived int64
	if err := r.Scan(
		&c.ID, &c.SourcePlatform, &c.Name, &isGroup, &c.ParticipantsJSON,
		&lastMsg, &unread, &pinned, &archived, &updated,
	); err != nil {
		return Conversation{}, err
	}
	c.IsGroup = isGroup != 0
	c.Unread = unread != 0
	c.Pinned = pinned != 0
	c.Archived = archived != 0
	c.LastMessageTimeMS = lastMsg
	c.UpdatedAt = time.UnixMilli(updated)
	return c, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullableJSON(s string) any {
	if s == "" {
		return "[]"
	}
	return s
}
