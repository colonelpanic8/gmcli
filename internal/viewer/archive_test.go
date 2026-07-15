package viewer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fdsouvenir/gmcli/internal/store"
)

func TestJSONLSourceWithSQLiteCache(t *testing.T) {
	dir, cachePath := writeFixture(t, fixtureMessages())
	ctx := context.Background()
	archive, err := Open(ctx, dir, OpenOptions{CachePath: cachePath})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()

	meta, err := archive.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Conversations != 1 || meta.Messages != 4 || meta.CachePath != cachePath {
		t.Fatalf("metadata = %+v", meta)
	}
	conversations, err := archive.ListConversations(ctx, ConversationQuery{Query: "friend"})
	if err != nil || conversations.Total != 1 || conversations.Conversations[0].MessageCount != 4 {
		t.Fatalf("conversations = %+v, err = %v", conversations, err)
	}

	page, err := archive.ListMessages(ctx, "chat-1", MessageQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{page.Messages[0].ID, page.Messages[1].ID}; got[0] != "m2" || got[1] != "m3" || !page.HasOlder {
		t.Fatalf("latest page IDs = %v, page = %+v", got, page)
	}
	older, err := archive.ListMessages(ctx, "chat-1", MessageQuery{Before: page.BeforeCursor, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Messages) != 2 || older.Messages[0].ID != "m0" || older.Messages[1].ID != "m1" {
		t.Fatalf("older page = %+v", older)
	}
	newer, err := archive.ListMessages(ctx, "chat-1", MessageQuery{After: older.AfterCursor, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(newer.Messages) != 2 || newer.Messages[0].ID != "m2" || newer.Messages[1].ID != "m3" {
		t.Fatalf("newer page = %+v", newer)
	}

	search, err := archive.SearchMessages(ctx, SearchQuery{Query: "second"})
	if err != nil || search.Total != 1 || search.Hits[0].Message.ID != "m1" {
		t.Fatalf("search = %+v, err = %v", search, err)
	}
	messageContext, err := archive.MessageContext(ctx, "chat-1", "m2", ContextQuery{Before: 1, After: 1})
	if err != nil || messageContext.TargetIndex != 1 || len(messageContext.Messages) != 3 {
		t.Fatalf("context = %+v, err = %v", messageContext, err)
	}
	if _, err := archive.GetConversation(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing conversation error = %v", err)
	}
}

func TestConversationSorting(t *testing.T) {
	dir, cachePath := writeFixture(t, fixtureMessages())
	ctx := context.Background()
	archive, err := Open(ctx, dir, OpenOptions{CachePath: cachePath})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if _, err := archive.store.DB().Exec(`
		INSERT INTO conversations (
			conversation_id, source_platform, name, participants_json,
			last_message_ts, updated_at
		) VALUES ('chat-2', 'gm', 'Large old chat', '[]', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := archive.store.DB().Exec(`
			INSERT INTO messages (
				message_id, conversation_id, source_platform, sender_id,
				timestamp_ms, status, is_from_me, updated_at
			) VALUES (?, 'chat-2', 'gm', '', ?, 0, 0, 1)`, fmt.Sprintf("large-%d", i), i+1); err != nil {
			t.Fatal(err)
		}
	}

	recent, err := archive.ListConversations(ctx, ConversationQuery{Sort: ConversationSortRecent})
	if err != nil {
		t.Fatal(err)
	}
	if recent.Sort != ConversationSortRecent || recent.Conversations[0].ID != "chat-1" {
		t.Fatalf("recent order = %+v", recent)
	}
	mostMessages, err := archive.ListConversations(ctx, ConversationQuery{Sort: ConversationSortMessages})
	if err != nil {
		t.Fatal(err)
	}
	if mostMessages.Sort != ConversationSortMessages || mostMessages.Conversations[0].ID != "chat-2" || mostMessages.Conversations[0].MessageCount != 6 {
		t.Fatalf("message-count order = %+v", mostMessages)
	}
	if _, err := archive.ListConversations(ctx, ConversationQuery{Sort: "unknown"}); err == nil {
		t.Fatal("invalid sort was accepted")
	}
}

func TestCacheRefreshesOnlyChangedManifestFiles(t *testing.T) {
	dir, cachePath := writeFixture(t, fixtureMessages())
	ctx := context.Background()
	archive, err := Open(ctx, dir, OpenOptions{CachePath: cachePath})
	if err != nil {
		t.Fatal(err)
	}
	indexedAt := indexedFileTime(t, archive, "messages/Y2hhdC0x.jsonl")
	unchangedUpdatedAt := cachedMessageTime(t, archive, "m0")
	changedUpdatedAt := cachedMessageTime(t, archive, "m1")
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Millisecond)
	archive, err = Open(ctx, dir, OpenOptions{CachePath: cachePath})
	if err != nil {
		t.Fatal(err)
	}
	if got := indexedFileTime(t, archive, "messages/Y2hhdC0x.jsonl"); got != indexedAt {
		t.Fatalf("unchanged file was reindexed: %d != %d", got, indexedAt)
	}
	defer archive.Close()

	messages := fixtureMessages()
	messages[1].Body = stringPtr("second changed")
	messages = append(messages[:2], messages[3:]...)
	messages = append(messages, Message{ID: "m4", ConversationID: "chat-1", SourcePlatform: "gm", SenderID: "friend", Body: stringPtr("new"), TimestampMS: 4})
	writeMessagesAndManifest(t, dir, messages)
	time.Sleep(2 * time.Millisecond)
	if err := archive.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	meta, err := archive.Metadata(ctx)
	if err != nil || meta.Messages != 4 {
		t.Fatalf("metadata after refresh = %+v, err = %v", meta, err)
	}
	if got := indexedFileTime(t, archive, "messages/Y2hhdC0x.jsonl"); got <= indexedAt {
		t.Fatalf("changed file indexed_at = %d, want > %d", got, indexedAt)
	}
	if got := cachedMessageTime(t, archive, "m0"); got != unchangedUpdatedAt {
		t.Fatalf("unchanged message was rewritten: %d != %d", got, unchangedUpdatedAt)
	}
	if got := cachedMessageTime(t, archive, "m1"); got <= changedUpdatedAt {
		t.Fatalf("changed message updated_at = %d, want > %d", got, changedUpdatedAt)
	}
	if _, err := archive.getMessage(ctx, "chat-1", "m2"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("removed source message remains cached: %v", err)
	}
	search, err := archive.SearchMessages(ctx, SearchQuery{Query: "changed"})
	if err != nil || search.Total != 1 || search.Hits[0].Message.ID != "m1" {
		t.Fatalf("updated search = %+v, err = %v", search, err)
	}
}

func TestOpenRejectsUnsafeManifestPath(t *testing.T) {
	dir, cachePath := writeFixture(t, fixtureMessages())
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	value["conversation_messages"].([]any)[0].(map[string]any)["path"] = "../outside.jsonl"
	writeJSON(t, filepath.Join(dir, "manifest.json"), value)
	if _, err := Open(context.Background(), dir, OpenOptions{CachePath: cachePath}); err == nil {
		t.Fatal("Open() accepted an unsafe manifest path")
	}
}

func TestOpenRefusesUnrelatedSQLiteDatabase(t *testing.T) {
	dir, _ := writeFixture(t, fixtureMessages())
	path := filepath.Join(t.TempDir(), "state.sqlite")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), dir, OpenOptions{CachePath: path}); err == nil || !strings.Contains(err.Error(), "non-cache SQLite") {
		t.Fatalf("Open() error = %v", err)
	}
}

func TestCacheIsPinnedToArchiveRoot(t *testing.T) {
	dir, cachePath := writeFixture(t, fixtureMessages())
	archive, err := Open(context.Background(), dir, OpenOptions{CachePath: cachePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	otherDir, _ := writeFixture(t, fixtureMessages())
	if _, err := Open(context.Background(), otherDir, OpenOptions{CachePath: cachePath}); err == nil || !strings.Contains(err.Error(), "cache belongs to archive") {
		t.Fatalf("Open() error = %v", err)
	}
}

func indexedFileTime(t *testing.T, archive *Archive, path string) int64 {
	t.Helper()
	var value int64
	if err := archive.store.DB().QueryRow(`SELECT indexed_at FROM archive_cache_files WHERE path = ?`, path).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func cachedMessageTime(t *testing.T, archive *Archive, id string) int64 {
	t.Helper()
	var value int64
	if err := archive.store.DB().QueryRow(`SELECT updated_at FROM messages WHERE message_id = ?`, id).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func writeFixture(t *testing.T, messages []Message) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "archive")
	if err := os.MkdirAll(filepath.Join(dir, "messages"), 0o700); err != nil {
		t.Fatal(err)
	}
	conversation := Conversation{ID: "chat-1", SourcePlatform: "gm", Name: "Friendly Chat", Participants: []Participant{{ID: "me", Name: "Me", IsMe: true}, {ID: "friend", Name: "Friend"}}, LastMessageTimeMS: 3, UpdatedAt: time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)}
	writeJSONL(t, filepath.Join(dir, "conversations.jsonl"), []Conversation{conversation})
	writeMessagesAndManifest(t, dir, messages)
	return dir, filepath.Join(t.TempDir(), "cache.sqlite")
}

func writeMessagesAndManifest(t *testing.T, dir string, messages []Message) {
	t.Helper()
	messagePath := filepath.Join(dir, "messages", "Y2hhdC0x.jsonl")
	writeJSONL(t, messagePath, messages)
	manifestValue := manifest{
		Format:        "gmcli-jsonl-archive",
		FormatVersion: 3,
		ExportedAt:    time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC),
		Files: map[string]manifestFile{
			"conversations": {Path: "conversations.jsonl", SHA256: fileSHA256(t, filepath.Join(dir, "conversations.jsonl"))},
		},
		ConversationMessages: []manifestConversationFile{{ConversationID: "chat-1", Path: "messages/Y2hhdC0x.jsonl", Messages: len(messages), SHA256: fileSHA256(t, messagePath)}},
	}
	writeJSON(t, filepath.Join(dir, "manifest.json"), manifestValue)
}

func fixtureMessages() []Message {
	return []Message{
		{ID: "m0", ConversationID: "chat-1", SourcePlatform: "gm", SenderID: "friend", Body: stringPtr("first"), TimestampMS: 1},
		{ID: "m1", ConversationID: "chat-1", SourcePlatform: "gm", SenderID: "me", Body: stringPtr("second"), TimestampMS: 2, IsFromMe: true},
		{ID: "m2", ConversationID: "chat-1", SourcePlatform: "gm", SenderID: "friend", Body: stringPtr("same time A"), TimestampMS: 2},
		{ID: "m3", ConversationID: "chat-1", SourcePlatform: "gm", SenderID: "friend", Body: stringPtr("same time B"), TimestampMS: 2},
	}
}

func stringPtr(value string) *string { return &value }

func writeJSONL[T any](t *testing.T, path string, values []T) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(f)
	for _, value := range values {
		if err := encoder.Encode(value); err != nil {
			f.Close()
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
