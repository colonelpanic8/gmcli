package viewer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fdsouvenir/gmcli/internal/unifiedarchive"
)

// Unified provides the normal viewer query surface over the dynamically
// reconciled relay and Android Telephony archives.
type Unified struct {
	relayDir     string
	telephonyDir string
	mu           sync.RWMutex
	dataset      *unifiedarchive.Dataset
	exportedAt   time.Time
}

// OpenUnified verifies both archives and builds their canonical participant
// view in memory.
func OpenUnified(ctx context.Context, relayDir, telephonyDir string) (*Unified, error) {
	u := &Unified{relayDir: relayDir, telephonyDir: telephonyDir}
	if err := u.Refresh(ctx); err != nil {
		return nil, err
	}
	return u, nil
}

// Close implements the same lifecycle as Archive. Unified owns no open files.
func (u *Unified) Close() error { return nil }

// Refresh rebuilds the derived view after either authoritative archive changes.
func (u *Unified) Refresh(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dataset, err := unifiedarchive.Open(u.relayDir, u.telephonyDir)
	if err != nil {
		return err
	}
	exportedAt, err := relayExportedAt(u.relayDir)
	if err != nil {
		return err
	}
	u.mu.Lock()
	u.dataset = dataset
	u.exportedAt = exportedAt
	u.mu.Unlock()
	return nil
}

func relayExportedAt(dir string) (time.Time, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return time.Time{}, err
	}
	var manifest struct {
		ExportedAt time.Time `json:"exported_at"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return time.Time{}, fmt.Errorf("decode relay manifest: %w", err)
	}
	return manifest.ExportedAt, nil
}

func (u *Unified) snapshot() (*unifiedarchive.Dataset, time.Time) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.dataset, u.exportedAt
}

func (u *Unified) Metadata(context.Context) (Meta, error) {
	dataset, exportedAt := u.snapshot()
	result := dataset.Result()
	return Meta{
		FormatVersion: result.FormatVersion,
		ExportedAt:    exportedAt,
		Conversations: result.Conversations,
		Messages:      result.Messages,
		CachePath:     "dynamic unified view",
	}, nil
}

func (u *Unified) ListConversations(_ context.Context, query ConversationQuery) (ConversationPage, error) {
	limit, offset := bounded(query.Limit, 100, 1, 500), bounded(query.Offset, 0, 0, 1_000_000)
	sortOrder := query.Sort
	if sortOrder == "" {
		sortOrder = ConversationSortRecent
	}
	if sortOrder != ConversationSortRecent && sortOrder != ConversationSortMessages {
		return ConversationPage{}, fmt.Errorf("unsupported conversation sort %q (want recent or messages)", sortOrder)
	}
	dataset, exportedAt := u.snapshot()
	values := dataset.Conversations()
	needle := strings.ToLower(strings.TrimSpace(query.Query))
	conversations := make([]Conversation, 0, len(values))
	for _, value := range values {
		conversation := unifiedConversation(value, dataset, exportedAt)
		if needle != "" && !strings.Contains(strings.ToLower(conversationHaystack(conversation)), needle) {
			continue
		}
		conversations = append(conversations, conversation)
	}
	sort.SliceStable(conversations, func(i, j int) bool {
		if sortOrder == ConversationSortMessages && conversations[i].MessageCount != conversations[j].MessageCount {
			return conversations[i].MessageCount > conversations[j].MessageCount
		}
		if conversations[i].LastMessageTimeMS != conversations[j].LastMessageTimeMS {
			return conversations[i].LastMessageTimeMS > conversations[j].LastMessageTimeMS
		}
		return conversations[i].ID < conversations[j].ID
	})
	total := len(conversations)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return ConversationPage{Conversations: conversations[offset:end], Total: total, Offset: offset, Limit: limit, Sort: sortOrder}, nil
}

func (u *Unified) GetConversation(_ context.Context, id string) (Conversation, error) {
	dataset, exportedAt := u.snapshot()
	for _, value := range dataset.Conversations() {
		if value.CanonicalConversationID == id {
			return unifiedConversation(value, dataset, exportedAt), nil
		}
	}
	return Conversation{}, fmt.Errorf("%w: conversation %q", ErrNotFound, id)
}

func unifiedConversation(value unifiedarchive.Conversation, dataset *unifiedarchive.Dataset, exportedAt time.Time) Conversation {
	participants := make([]Participant, 0, len(value.Participants))
	for _, participant := range value.Participants {
		participants = append(participants, Participant{
			ID: participant.E164, Name: participant.Name, E164: participant.E164,
			FormattedNumber: participant.E164,
		})
	}
	preview := ""
	if messages, ok := dataset.Messages(value.CanonicalConversationID); ok {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Body != nil {
				preview = compactUnifiedPreview(*messages[i].Body, 160)
				break
			}
		}
	}
	return Conversation{
		ID: value.CanonicalConversationID, SourcePlatform: "unified", Name: value.Name,
		IsGroup: len(participants) > 1, Participants: participants,
		LastMessageTimeMS: value.LastMessageMS, UpdatedAt: exportedAt,
		MessageCount: value.Messages, Preview: preview,
	}
}

func conversationHaystack(conversation Conversation) string {
	parts := []string{conversation.ID, conversation.Name, conversation.Preview}
	for _, participant := range conversation.Participants {
		parts = append(parts, participant.Name, participant.E164, participant.FormattedNumber)
	}
	return strings.Join(parts, " ")
}

func compactUnifiedPreview(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

func (u *Unified) ListMessages(ctx context.Context, conversationID string, query MessageQuery) (MessagePage, error) {
	conversation, err := u.GetConversation(ctx, conversationID)
	if err != nil {
		return MessagePage{}, err
	}
	if query.Before != "" && query.After != "" {
		return MessagePage{}, fmt.Errorf("before and after cursors are mutually exclusive")
	}
	dataset, _ := u.snapshot()
	values, ok := dataset.Messages(conversationID)
	if !ok {
		return MessagePage{}, fmt.Errorf("%w: conversation %q", ErrNotFound, conversationID)
	}
	messages := make([]Message, len(values))
	for i := range values {
		messages[i] = unifiedMessage(values[i], conversation)
	}
	limit := bounded(query.Limit, 200, 1, 1000)
	start, end := 0, len(messages)
	if query.Before != "" {
		cursor, err := decodeMessageCursor(query.Before)
		if err != nil {
			return MessagePage{}, err
		}
		end = sort.Search(len(messages), func(i int) bool { return !messageLess(messages[i], cursor.TimestampMS, cursor.MessageID) })
		start = end - limit
		if start < 0 {
			start = 0
		}
	} else if query.After != "" {
		cursor, err := decodeMessageCursor(query.After)
		if err != nil {
			return MessagePage{}, err
		}
		start = sort.Search(len(messages), func(i int) bool { return messageGreater(messages[i], cursor.TimestampMS, cursor.MessageID) })
		end = start + limit
		if end > len(messages) {
			end = len(messages)
		}
	} else if end > limit {
		start = end - limit
	}
	pageMessages := append([]Message(nil), messages[start:end]...)
	result := MessagePage{Conversation: conversation, Messages: pageMessages, HasOlder: start > 0, HasNewer: end < len(messages)}
	if len(pageMessages) > 0 {
		result.BeforeCursor = encodeMessageCursor(pageMessages[0])
		result.AfterCursor = encodeMessageCursor(pageMessages[len(pageMessages)-1])
	}
	return result, nil
}

func messageLess(message Message, timestamp int64, id string) bool {
	return message.TimestampMS < timestamp || message.TimestampMS == timestamp && message.ID < id
}

func messageGreater(message Message, timestamp int64, id string) bool {
	return message.TimestampMS > timestamp || message.TimestampMS == timestamp && message.ID > id
}

func unifiedMessage(value unifiedarchive.Message, conversation Conversation) Message {
	message := Message{
		ID: value.UnifiedMessageID, ConversationID: value.CanonicalConversationID,
		SourcePlatform: "unified", Body: value.Body, TimestampMS: value.TimestampMS,
		IsFromMe: value.IsFromMe,
	}
	if value.IsFromMe {
		message.SenderName = "You"
	} else if len(conversation.Participants) == 1 {
		message.SenderID = conversation.Participants[0].ID
		message.SenderName = conversation.Participants[0].Name
	}
	if len(value.Attachments) > 0 {
		attachment := value.Attachments[0]
		if attachment.MediaID != "" {
			message.MediaID = &attachment.MediaID
		} else if attachment.RecordID != "" {
			message.MediaID = &attachment.RecordID
		}
		if attachment.MimeType != "" {
			message.MimeType = &attachment.MimeType
		}
	}
	return message
}

func (u *Unified) SearchMessages(ctx context.Context, query SearchQuery) (SearchPage, error) {
	limit, offset := bounded(query.Limit, 100, 1, 500), bounded(query.Offset, 0, 0, 1_000_000)
	needle := strings.ToLower(strings.TrimSpace(query.Query))
	if needle == "" {
		return SearchPage{Query: query.Query, Hits: []SearchHit{}, Offset: offset, Limit: limit}, nil
	}
	if query.ConversationID != "" {
		if _, err := u.GetConversation(ctx, query.ConversationID); err != nil {
			return SearchPage{}, err
		}
	}
	dataset, exportedAt := u.snapshot()
	var hits []SearchHit
	for _, value := range dataset.Conversations() {
		if query.ConversationID != "" && value.CanonicalConversationID != query.ConversationID {
			continue
		}
		conversation := unifiedConversation(value, dataset, exportedAt)
		messages, _ := dataset.Messages(value.CanonicalConversationID)
		for _, value := range messages {
			if value.Body == nil || !strings.Contains(strings.ToLower(*value.Body), needle) {
				continue
			}
			hits = append(hits, SearchHit{ConversationID: conversation.ID, ConversationName: conversation.Name, Message: unifiedMessage(value, conversation)})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Message.TimestampMS != hits[j].Message.TimestampMS {
			return hits[i].Message.TimestampMS > hits[j].Message.TimestampMS
		}
		return hits[i].Message.ID > hits[j].Message.ID
	})
	total := len(hits)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return SearchPage{Query: query.Query, Hits: hits[offset:end], Total: total, Offset: offset, Limit: limit}, nil
}

func (u *Unified) MessageContext(ctx context.Context, conversationID, messageID string, query ContextQuery) (MessageContext, error) {
	conversation, err := u.GetConversation(ctx, conversationID)
	if err != nil {
		return MessageContext{}, err
	}
	dataset, _ := u.snapshot()
	values, _ := dataset.Messages(conversationID)
	index := -1
	for i := range values {
		if values[i].UnifiedMessageID == messageID {
			index = i
			break
		}
	}
	if index < 0 {
		return MessageContext{}, fmt.Errorf("%w: message %q", ErrNotFound, messageID)
	}
	before, after := bounded(query.Before, 20, 0, 500), bounded(query.After, 20, 0, 500)
	start, end := index-before, index+after+1
	if start < 0 {
		start = 0
	}
	if end > len(values) {
		end = len(values)
	}
	messages := make([]Message, 0, end-start)
	for _, value := range values[start:end] {
		messages = append(messages, unifiedMessage(value, conversation))
	}
	return MessageContext{Conversation: conversation, TargetID: messageID, TargetIndex: index - start, Messages: messages}, nil
}
