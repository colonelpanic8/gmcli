package viewer

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrInvalidCursor = errors.New("invalid cursor")
)

// Source is the renderer-independent query surface shared by CLI, TUI, and
// future viewer clients.
type Source interface {
	Metadata(context.Context) (Meta, error)
	ListConversations(context.Context, ConversationQuery) (ConversationPage, error)
	GetConversation(context.Context, string) (Conversation, error)
	ListMessages(context.Context, string, MessageQuery) (MessagePage, error)
	SearchMessages(context.Context, SearchQuery) (SearchPage, error)
	MessageContext(context.Context, string, string, ContextQuery) (MessageContext, error)
}

type ConversationQuery struct {
	Query         string
	Sort          ConversationSort
	Offset, Limit int
}

// ConversationSort controls conversation-list ordering.
type ConversationSort string

const (
	// ConversationSortRecent orders by newest message activity first.
	ConversationSortRecent ConversationSort = "recent"
	// ConversationSortMessages orders by message count first, then recency.
	ConversationSortMessages ConversationSort = "messages"
)

type ConversationPage struct {
	Conversations []Conversation   `json:"conversations"`
	Total         int              `json:"total"`
	Offset        int              `json:"offset"`
	Limit         int              `json:"limit"`
	Sort          ConversationSort `json:"sort"`
}
type Cursor string
type MessageQuery struct {
	Before, After Cursor
	Limit         int
}
type MessagePage struct {
	Conversation Conversation `json:"conversation"`
	Messages     []Message    `json:"messages"`
	HasOlder     bool         `json:"has_older"`
	HasNewer     bool         `json:"has_newer"`
	BeforeCursor Cursor       `json:"before_cursor,omitempty"`
	AfterCursor  Cursor       `json:"after_cursor,omitempty"`
}
type SearchQuery struct {
	Query, ConversationID string
	Offset, Limit         int
}
type SearchHit struct {
	ConversationID   string  `json:"conversation_id"`
	ConversationName string  `json:"conversation_name"`
	Message          Message `json:"message"`
}
type SearchPage struct {
	Query  string      `json:"query"`
	Hits   []SearchHit `json:"hits"`
	Total  int         `json:"total"`
	Offset int         `json:"offset"`
	Limit  int         `json:"limit"`
}
type ContextQuery struct{ Before, After int }
type MessageContext struct {
	Conversation Conversation `json:"conversation"`
	TargetID     string       `json:"target_message_id"`
	TargetIndex  int          `json:"target_index"`
	Messages     []Message    `json:"messages"`
}

const conversationColumns = `
	c.conversation_id, c.source_platform, c.name, c.is_group, c.participants_json,
	c.last_message_ts, c.unread, c.pinned, c.archived, c.updated_at,
	(SELECT COUNT(*) FROM messages m WHERE m.conversation_id = c.conversation_id) AS message_count,
	COALESCE((SELECT COALESCE(m.body, '') FROM messages m WHERE m.conversation_id = c.conversation_id ORDER BY m.timestamp_ms DESC, m.message_id DESC LIMIT 1), '') AS preview`

func (a *Archive) Metadata(ctx context.Context) (Meta, error) {
	var conversations, messages int
	if err := a.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations`).Scan(&conversations); err != nil {
		return Meta{}, err
	}
	if err := a.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&messages); err != nil {
		return Meta{}, err
	}
	return Meta{FormatVersion: a.formatVersion, ExportedAt: a.exportedAt, Conversations: conversations, Messages: messages, CachePath: a.cachePath}, nil
}

func (a *Archive) ListConversations(ctx context.Context, query ConversationQuery) (ConversationPage, error) {
	limit, offset := bounded(query.Limit, 100, 1, 500), bounded(query.Offset, 0, 0, 1_000_000)
	sortOrder := query.Sort
	if sortOrder == "" {
		sortOrder = ConversationSortRecent
	}
	orderBy := `c.last_message_ts DESC, c.conversation_id`
	switch sortOrder {
	case ConversationSortRecent:
	case ConversationSortMessages:
		orderBy = `message_count DESC, c.last_message_ts DESC, c.conversation_id`
	default:
		return ConversationPage{}, fmt.Errorf("unsupported conversation sort %q (want recent or messages)", sortOrder)
	}
	where := ""
	var args []any
	if needle := strings.TrimSpace(query.Query); needle != "" {
		where = ` WHERE LOWER(c.name || ' ' || c.conversation_id || ' ' || COALESCE(c.participants_json, '') || ' ' || COALESCE((SELECT m.body FROM messages m WHERE m.conversation_id = c.conversation_id ORDER BY m.timestamp_ms DESC, m.message_id DESC LIMIT 1), '')) LIKE ?`
		args = append(args, "%"+strings.ToLower(needle)+"%")
	}
	var total int
	if err := a.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations c`+where, args...).Scan(&total); err != nil {
		return ConversationPage{}, err
	}
	rows, err := a.store.DB().QueryContext(ctx, `SELECT `+conversationColumns+` FROM conversations c`+where+` ORDER BY `+orderBy+` LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return ConversationPage{}, err
	}
	defer rows.Close()
	values := make([]Conversation, 0, limit)
	for rows.Next() {
		conversation, err := scanConversation(rows)
		if err != nil {
			return ConversationPage{}, err
		}
		values = append(values, conversation)
	}
	return ConversationPage{Conversations: values, Total: total, Offset: offset, Limit: limit, Sort: sortOrder}, rows.Err()
}

func (a *Archive) GetConversation(ctx context.Context, id string) (Conversation, error) {
	conversation, err := scanConversation(a.store.DB().QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations c WHERE c.conversation_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, fmt.Errorf("%w: conversation %q", ErrNotFound, id)
	}
	return conversation, err
}

func (a *Archive) ListMessages(ctx context.Context, conversationID string, query MessageQuery) (MessagePage, error) {
	conversation, err := a.GetConversation(ctx, conversationID)
	if err != nil {
		return MessagePage{}, err
	}
	if query.Before != "" && query.After != "" {
		return MessagePage{}, errors.New("before and after cursors are mutually exclusive")
	}
	limit := bounded(query.Limit, 200, 1, 1000)
	where, order := `conversation_id = ?`, `timestamp_ms DESC, message_id DESC`
	args := []any{conversationID}
	if query.Before != "" {
		cursor, err := decodeMessageCursor(query.Before)
		if err != nil {
			return MessagePage{}, err
		}
		where += ` AND (timestamp_ms < ? OR (timestamp_ms = ? AND message_id < ?))`
		args = append(args, cursor.TimestampMS, cursor.TimestampMS, cursor.MessageID)
	}
	if query.After != "" {
		cursor, err := decodeMessageCursor(query.After)
		if err != nil {
			return MessagePage{}, err
		}
		where += ` AND (timestamp_ms > ? OR (timestamp_ms = ? AND message_id > ?))`
		args = append(args, cursor.TimestampMS, cursor.TimestampMS, cursor.MessageID)
		order = `timestamp_ms ASC, message_id ASC`
	}
	args = append(args, limit)
	rows, err := a.store.DB().QueryContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE `+where+` ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		return MessagePage{}, err
	}
	messages, err := scanMessages(rows)
	if err != nil {
		return MessagePage{}, err
	}
	if query.After == "" {
		reverseMessages(messages)
	}
	enrichSenders(messages, conversation)
	result := MessagePage{Conversation: conversation, Messages: messages}
	if len(messages) > 0 {
		result.BeforeCursor, result.AfterCursor = encodeMessageCursor(messages[0]), encodeMessageCursor(messages[len(messages)-1])
		result.HasOlder, err = a.messageExists(ctx, conversationID, messages[0], true)
		if err != nil {
			return MessagePage{}, err
		}
		result.HasNewer, err = a.messageExists(ctx, conversationID, messages[len(messages)-1], false)
		if err != nil {
			return MessagePage{}, err
		}
	}
	return result, nil
}

func (a *Archive) SearchMessages(ctx context.Context, query SearchQuery) (SearchPage, error) {
	limit, offset := bounded(query.Limit, 100, 1, 500), bounded(query.Offset, 0, 0, 1_000_000)
	needle := strings.TrimSpace(query.Query)
	if needle == "" {
		return SearchPage{Query: needle, Hits: []SearchHit{}, Offset: offset, Limit: limit}, nil
	}
	where := `messages_fts MATCH ?`
	args := []any{needle}
	if query.ConversationID != "" {
		if _, err := a.GetConversation(ctx, query.ConversationID); err != nil {
			return SearchPage{}, err
		}
		where += ` AND m.conversation_id = ?`
		args = append(args, query.ConversationID)
	}
	var total int
	countSQL := `SELECT COUNT(*) FROM messages_fts JOIN messages m ON m.message_id = messages_fts.message_id WHERE ` + where
	if err := a.store.DB().QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return SearchPage{}, err
	}
	rows, err := a.store.DB().QueryContext(ctx, `SELECT `+messageColumnsQualified+`, c.name FROM messages_fts JOIN messages m ON m.message_id = messages_fts.message_id JOIN conversations c ON c.conversation_id = m.conversation_id WHERE `+where+` ORDER BY m.timestamp_ms DESC, m.message_id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return SearchPage{}, err
	}
	defer rows.Close()
	hits := make([]SearchHit, 0, limit)
	for rows.Next() {
		message, name, err := scanSearchMessage(rows)
		if err != nil {
			return SearchPage{}, err
		}
		hits = append(hits, SearchHit{ConversationID: message.ConversationID, ConversationName: name, Message: message})
	}
	return SearchPage{Query: needle, Hits: hits, Total: total, Offset: offset, Limit: limit}, rows.Err()
}

func (a *Archive) MessageContext(ctx context.Context, conversationID, messageID string, query ContextQuery) (MessageContext, error) {
	conversation, err := a.GetConversation(ctx, conversationID)
	if err != nil {
		return MessageContext{}, err
	}
	target, err := a.getMessage(ctx, conversationID, messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return MessageContext{}, fmt.Errorf("%w: message %q", ErrNotFound, messageID)
	}
	if err != nil {
		return MessageContext{}, err
	}
	beforeLimit, afterLimit := bounded(query.Before, 20, 0, 500), bounded(query.After, 20, 0, 500)
	before, err := a.queryAround(ctx, conversationID, target, beforeLimit, true)
	if err != nil {
		return MessageContext{}, err
	}
	reverseMessages(before)
	after, err := a.queryAround(ctx, conversationID, target, afterLimit, false)
	if err != nil {
		return MessageContext{}, err
	}
	messages := append(before, target)
	messages = append(messages, after...)
	enrichSenders(messages, conversation)
	return MessageContext{Conversation: conversation, TargetID: messageID, TargetIndex: len(before), Messages: messages}, nil
}

const messageColumns = `message_id, conversation_id, source_platform, sender_id, body, timestamp_ms, status, is_from_me, media_id, mime_type, reactions_json, reply_to_id`
const messageColumnsQualified = `m.message_id, m.conversation_id, m.source_platform, m.sender_id, m.body, m.timestamp_ms, m.status, m.is_from_me, m.media_id, m.mime_type, m.reactions_json, m.reply_to_id`

type rowScanner interface{ Scan(...any) error }

func scanConversation(row rowScanner) (Conversation, error) {
	var c Conversation
	var participants string
	var group, unread, pinned, archived int
	var updated int64
	err := row.Scan(&c.ID, &c.SourcePlatform, &c.Name, &group, &participants, &c.LastMessageTimeMS, &unread, &pinned, &archived, &updated, &c.MessageCount, &c.Preview)
	if err != nil {
		return c, err
	}
	c.IsGroup = group != 0
	c.Unread = unread != 0
	c.Pinned = pinned != 0
	c.Archived = archived != 0
	c.UpdatedAt = time.UnixMilli(updated).UTC()
	if participants != "" {
		if err := json.Unmarshal([]byte(participants), &c.Participants); err != nil {
			return c, err
		}
	}
	return c, nil
}
func scanMessage(row rowScanner) (Message, error) {
	var m Message
	var fromMe int
	var body, media, mime, reactions, reply sql.NullString
	err := row.Scan(&m.ID, &m.ConversationID, &m.SourcePlatform, &m.SenderID, &body, &m.TimestampMS, &m.Status, &fromMe, &media, &mime, &reactions, &reply)
	if err != nil {
		return m, err
	}
	m.IsFromMe = fromMe != 0
	if body.Valid {
		m.Body = &body.String
	}
	if media.Valid {
		m.MediaID = &media.String
	}
	if mime.Valid {
		m.MimeType = &mime.String
	}
	if reactions.Valid {
		m.Reactions = json.RawMessage(reactions.String)
	}
	if reply.Valid {
		m.ReplyToID = &reply.String
	}
	return m, nil
}
func scanMessages(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func scanSearchMessage(row rowScanner) (Message, string, error) {
	var m Message
	var fromMe int
	var body, media, mime, reactions, reply sql.NullString
	var name string
	err := row.Scan(&m.ID, &m.ConversationID, &m.SourcePlatform, &m.SenderID, &body, &m.TimestampMS, &m.Status, &fromMe, &media, &mime, &reactions, &reply, &name)
	if err != nil {
		return m, "", err
	}
	m.IsFromMe = fromMe != 0
	if body.Valid {
		m.Body = &body.String
	}
	if media.Valid {
		m.MediaID = &media.String
	}
	if mime.Valid {
		m.MimeType = &mime.String
	}
	if reactions.Valid {
		m.Reactions = json.RawMessage(reactions.String)
	}
	if reply.Valid {
		m.ReplyToID = &reply.String
	}
	return m, name, nil
}
func (a *Archive) getMessage(ctx context.Context, conversationID, messageID string) (Message, error) {
	return scanMessage(a.store.DB().QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE conversation_id=? AND message_id=?`, conversationID, messageID))
}
func (a *Archive) queryAround(ctx context.Context, id string, target Message, limit int, older bool) ([]Message, error) {
	if limit == 0 {
		return []Message{}, nil
	}
	op, order := "<", "DESC"
	if !older {
		op, order = ">", "ASC"
	}
	rows, err := a.store.DB().QueryContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE conversation_id=? AND (timestamp_ms `+op+` ? OR (timestamp_ms=? AND message_id `+op+` ?)) ORDER BY timestamp_ms `+order+`, message_id `+order+` LIMIT ?`, id, target.TimestampMS, target.TimestampMS, target.ID, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}
func (a *Archive) messageExists(ctx context.Context, id string, m Message, older bool) (bool, error) {
	op := "<"
	if !older {
		op = ">"
	}
	var exists int
	err := a.store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE conversation_id=? AND (timestamp_ms `+op+` ? OR (timestamp_ms=? AND message_id `+op+` ?)))`, id, m.TimestampMS, m.TimestampMS, m.ID).Scan(&exists)
	return exists != 0, err
}
func enrichSenders(messages []Message, c Conversation) {
	names := map[string]string{}
	for _, p := range c.Participants {
		name := p.Name
		if name == "" {
			name = p.FormattedNumber
		}
		if name == "" {
			name = p.E164
		}
		names[p.ID] = name
	}
	for i := range messages {
		messages[i].SenderName = names[messages[i].SenderID]
		if messages[i].IsFromMe && messages[i].SenderName == "" {
			messages[i].SenderName = "You"
		}
	}
}
func reverseMessages(values []Message) {
	for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
		values[i], values[j] = values[j], values[i]
	}
}

type messageCursor struct {
	TimestampMS int64
	MessageID   string
}

func encodeMessageCursor(m Message) Cursor {
	return Cursor(fmt.Sprintf("%d.%s", m.TimestampMS, base64.RawURLEncoding.EncodeToString([]byte(m.ID))))
}
func decodeMessageCursor(raw Cursor) (messageCursor, error) {
	parts := strings.SplitN(string(raw), ".", 2)
	if len(parts) != 2 {
		return messageCursor{}, ErrInvalidCursor
	}
	ts, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || ts < 0 {
		return messageCursor{}, ErrInvalidCursor
	}
	id, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(id) == 0 {
		return messageCursor{}, ErrInvalidCursor
	}
	return messageCursor{ts, string(id)}, nil
}
func bounded(value, fallback, min, max int) int {
	if value == 0 && fallback != 0 {
		return fallback
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}
