// Package viewer provides renderer-independent queries over portable gmcli
// JSONL archives. JSONL is authoritative; SQLite is a disposable cache.
package viewer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fdsouvenir/gmcli/internal/store"
)

const maxJSONLRecordSize = 16 << 20

type manifest struct {
	Format               string                     `json:"format"`
	FormatVersion        int                        `json:"format_version"`
	ExportedAt           time.Time                  `json:"exported_at"`
	Files                map[string]manifestFile    `json:"files"`
	ConversationMessages []manifestConversationFile `json:"conversation_messages"`
}

type manifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type manifestConversationFile struct {
	ConversationID string `json:"conversation_id"`
	Path           string `json:"path"`
	Messages       int    `json:"messages"`
	SHA256         string `json:"sha256"`
}

// Participant is the portable participant shape embedded in conversations.jsonl.
type Participant struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	E164            string `json:"e164"`
	FormattedNumber string `json:"formatted_number"`
	IsMe            bool   `json:"is_me"`
}

// Conversation is a viewer-ready conversation summary.
type Conversation struct {
	ID                string        `json:"conversation_id"`
	SourcePlatform    string        `json:"source_platform"`
	Name              string        `json:"name"`
	IsGroup           bool          `json:"is_group"`
	Participants      []Participant `json:"participants"`
	LastMessageTimeMS int64         `json:"last_message_time_ms"`
	Unread            bool          `json:"unread"`
	Pinned            bool          `json:"pinned"`
	Archived          bool          `json:"archived"`
	UpdatedAt         time.Time     `json:"updated_at"`
	MessageCount      int           `json:"message_count"`
	Preview           string        `json:"preview,omitempty"`
}

// Message is the portable message shape emitted by gmcli export jsonl.
type Message struct {
	ID             string          `json:"message_id"`
	ConversationID string          `json:"conversation_id"`
	SourcePlatform string          `json:"source_platform"`
	SenderID       string          `json:"sender_id"`
	SenderName     string          `json:"sender_name,omitempty"`
	Body           *string         `json:"body,omitempty"`
	TimestampMS    int64           `json:"timestamp_ms"`
	Status         int64           `json:"status"`
	IsFromMe       bool            `json:"is_from_me"`
	MediaID        *string         `json:"media_id,omitempty"`
	MimeType       *string         `json:"mime_type,omitempty"`
	Reactions      json.RawMessage `json:"reactions,omitempty"`
	ReplyToID      *string         `json:"reply_to_id,omitempty"`
}

// OpenOptions controls the disposable SQLite cache.
type OpenOptions struct {
	CachePath string
	Rebuild   bool
}

// Archive is a query source backed by a rebuildable SQLite cache.
type Archive struct {
	dir           string
	cachePath     string
	exportedAt    time.Time
	formatVersion int
	store         *store.Store
	refreshMu     sync.Mutex
	metadataMu    sync.RWMutex
}

// Meta describes the authoritative JSONL archive and its cache.
type Meta struct {
	FormatVersion int       `json:"format_version"`
	ExportedAt    time.Time `json:"exported_at"`
	Conversations int       `json:"conversations"`
	Messages      int       `json:"messages"`
	CachePath     string    `json:"cache_path"`
}

// Open indexes changed JSONL files into a disposable SQLite cache and returns
// a typed query source. The JSONL archive is never modified.
func Open(ctx context.Context, dir string, options OpenOptions) (*Archive, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve archive directory: %w", err)
	}
	m, err := readManifest(abs)
	if err != nil {
		return nil, err
	}
	cachePath := options.CachePath
	defaultCache := cachePath == ""
	if cachePath == "" {
		cachePath, err = defaultCachePath(abs)
		if err != nil {
			return nil, err
		}
	}
	cachePath, err = filepath.Abs(cachePath)
	if err != nil {
		return nil, fmt.Errorf("resolve cache path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	if defaultCache {
		if err := os.Chmod(filepath.Dir(cachePath), 0o700); err != nil {
			return nil, fmt.Errorf("secure cache directory: %w", err)
		}
	}
	if options.Rebuild {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(cachePath + suffix); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("remove old cache: %w", err)
			}
		}
	}
	_, cacheExistedErr := os.Stat(cachePath)
	cacheExisted := cacheExistedErr == nil
	if cacheExistedErr != nil && !os.IsNotExist(cacheExistedErr) {
		return nil, fmt.Errorf("inspect cache: %w", cacheExistedErr)
	}
	if cacheExisted {
		if err := validateExistingCache(ctx, cachePath); err != nil {
			return nil, err
		}
	}
	st, err := store.Open(ctx, cachePath)
	if err != nil {
		return nil, err
	}
	a := &Archive{dir: abs, cachePath: cachePath, exportedAt: m.ExportedAt, formatVersion: m.FormatVersion, store: st}
	if err := prepareCacheIdentity(ctx, st.DB(), abs, cacheExisted); err != nil {
		_ = st.Close()
		return nil, err
	}
	if err := a.refresh(ctx, m); err != nil {
		_ = st.Close()
		return nil, err
	}
	return a, nil
}

func validateExistingCache(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return fmt.Errorf("inspect existing cache: %w", err)
	}
	defer db.Close()
	var hasIdentity, hasFiles int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'archive_cache_identity'`).Scan(&hasIdentity); err != nil {
		return fmt.Errorf("inspect existing cache identity: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'archive_cache_files'`).Scan(&hasFiles); err != nil {
		return fmt.Errorf("inspect existing cache metadata: %w", err)
	}
	if hasIdentity == 0 && hasFiles == 0 {
		return fmt.Errorf("refusing to use existing non-cache SQLite database %s", path)
	}
	return nil
}

func prepareCacheIdentity(ctx context.Context, db *sql.DB, archiveRoot string, existed bool) error {
	var hasIdentity, hasFiles int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'archive_cache_identity'`).Scan(&hasIdentity); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'archive_cache_files'`).Scan(&hasFiles); err != nil {
		return err
	}
	if existed && hasIdentity == 0 && hasFiles == 0 {
		return fmt.Errorf("refusing to use an existing SQLite database without gmcli archive cache identity")
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS archive_cache_identity (id INTEGER PRIMARY KEY CHECK (id = 1), archive_root TEXT NOT NULL)`); err != nil {
		return err
	}
	var existing string
	err := db.QueryRowContext(ctx, `SELECT archive_root FROM archive_cache_identity WHERE id = 1`).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = db.ExecContext(ctx, `INSERT INTO archive_cache_identity (id, archive_root) VALUES (1, ?)`, archiveRoot)
		return err
	}
	if err != nil {
		return err
	}
	if existing != archiveRoot {
		return fmt.Errorf("cache belongs to archive %s, not %s; choose another cache path or use --rebuild-cache", existing, archiveRoot)
	}
	return nil
}

// Close releases the disposable cache database.
func (a *Archive) Close() error { return a.store.Close() }

// Refresh verifies the current manifest and incrementally brings the
// disposable cache up to date after the authoritative JSONL export changes.
func (a *Archive) Refresh(ctx context.Context) error {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	m, err := readManifest(a.dir)
	if err != nil {
		return err
	}
	if err := a.refresh(ctx, m); err != nil {
		return err
	}
	a.metadataMu.Lock()
	a.exportedAt = m.ExportedAt
	a.formatVersion = m.FormatVersion
	a.metadataMu.Unlock()
	return nil
}

func readManifest(dir string) (manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return manifest{}, fmt.Errorf("read archive manifest: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, fmt.Errorf("decode archive manifest: %w", err)
	}
	if m.Format != "gmcli-jsonl-archive" {
		return manifest{}, fmt.Errorf("unsupported archive format %q", m.Format)
	}
	if m.Files["conversations"].Path == "" || m.Files["conversations"].SHA256 == "" {
		return manifest{}, fmt.Errorf("archive manifest lacks conversations metadata")
	}
	return m, nil
}

func defaultCachePath(archiveDir string) (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache directory: %w", err)
	}
	digest := sha256.Sum256([]byte(archiveDir))
	name := hex.EncodeToString(digest[:12]) + ".sqlite"
	return filepath.Join(root, "gmcli", "archives", name), nil
}

type desiredFile struct {
	Path           string
	SHA256         string
	Kind           string
	ConversationID string
}

func (a *Archive) refresh(ctx context.Context, m manifest) error {
	db := a.store.DB()
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS archive_cache_files (
			path TEXT PRIMARY KEY,
			sha256 TEXT NOT NULL,
			kind TEXT NOT NULL,
			conversation_id TEXT NOT NULL DEFAULT '',
			indexed_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("initialize archive cache metadata: %w", err)
	}
	desired := make(map[string]desiredFile, len(m.ConversationMessages)+1)
	conversationManifest := m.Files["conversations"]
	desired[conversationManifest.Path] = desiredFile{Path: conversationManifest.Path, SHA256: conversationManifest.SHA256, Kind: "conversations"}
	seenConversations := make(map[string]struct{}, len(m.ConversationMessages))
	for _, file := range m.ConversationMessages {
		if file.ConversationID == "" || file.Path == "" || file.SHA256 == "" {
			return fmt.Errorf("archive manifest contains incomplete conversation message metadata")
		}
		if _, exists := seenConversations[file.ConversationID]; exists {
			return fmt.Errorf("archive manifest repeats conversation %q", file.ConversationID)
		}
		seenConversations[file.ConversationID] = struct{}{}
		if _, err := safeArchivePath(a.dir, file.Path); err != nil {
			return fmt.Errorf("conversation %q: %w", file.ConversationID, err)
		}
		desired[file.Path] = desiredFile{Path: file.Path, SHA256: file.SHA256, Kind: "messages", ConversationID: file.ConversationID}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin archive cache refresh: %w", err)
	}
	defer tx.Rollback()
	existing := make(map[string]desiredFile)
	rows, err := tx.QueryContext(ctx, `SELECT path, sha256, kind, conversation_id FROM archive_cache_files`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var file desiredFile
		if err := rows.Scan(&file.Path, &file.SHA256, &file.Kind, &file.ConversationID); err != nil {
			rows.Close()
			return err
		}
		existing[file.Path] = file
	}
	if err := rows.Close(); err != nil {
		return err
	}

	conversationFile := desired[conversationManifest.Path]
	if current, ok := existing[conversationFile.Path]; !ok || current.SHA256 != conversationFile.SHA256 {
		if err := a.indexConversations(ctx, tx, conversationFile, seenConversations); err != nil {
			return err
		}
	}
	for path, current := range existing {
		if current.Kind != "messages" {
			continue
		}
		if _, ok := desired[path]; !ok {
			if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE conversation_id = ?`, current.ConversationID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM archive_cache_files WHERE path = ?`, path); err != nil {
				return err
			}
		}
	}
	for _, file := range desired {
		if file.Kind != "messages" {
			continue
		}
		if current, ok := existing[file.Path]; ok && current.SHA256 == file.SHA256 && current.ConversationID == file.ConversationID {
			continue
		}
		if err := a.indexMessages(ctx, tx, file); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit archive cache refresh: %w", err)
	}
	return nil
}

func (a *Archive) indexConversations(ctx context.Context, tx *sql.Tx, file desiredFile, desiredIDs map[string]struct{}) error {
	path, err := safeArchivePath(a.dir, file.Path)
	if err != nil {
		return err
	}
	if err := verifySourceHash(path, file.SHA256); err != nil {
		return err
	}
	conversations, err := readJSONL[Conversation](path)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(conversations))
	for _, conversation := range conversations {
		if conversation.ID == "" {
			return fmt.Errorf("conversations.jsonl contains an empty conversation ID")
		}
		if _, ok := desiredIDs[conversation.ID]; !ok {
			return fmt.Errorf("conversation %q has no message file in the manifest", conversation.ID)
		}
		if _, ok := seen[conversation.ID]; ok {
			return fmt.Errorf("conversations.jsonl repeats conversation %q", conversation.ID)
		}
		seen[conversation.ID] = struct{}{}
		participants, err := json.Marshal(conversation.Participants)
		if err != nil {
			return fmt.Errorf("encode participants for %q: %w", conversation.ID, err)
		}
		updatedAt := conversation.UpdatedAt.UnixMilli()
		if conversation.UpdatedAt.IsZero() {
			updatedAt = 0
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversations (
				conversation_id, source_platform, name, is_group, participants_json,
				last_message_ts, unread, pinned, archived, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(conversation_id) DO UPDATE SET
				source_platform = excluded.source_platform,
				name = excluded.name,
				is_group = excluded.is_group,
				participants_json = excluded.participants_json,
				last_message_ts = excluded.last_message_ts,
				unread = excluded.unread,
				pinned = excluded.pinned,
				archived = excluded.archived,
				updated_at = excluded.updated_at`,
			conversation.ID, conversation.SourcePlatform, conversation.Name, boolInt(conversation.IsGroup), string(participants),
			conversation.LastMessageTimeMS, boolInt(conversation.Unread), boolInt(conversation.Pinned), boolInt(conversation.Archived), updatedAt); err != nil {
			return fmt.Errorf("index conversation %q: %w", conversation.ID, err)
		}
	}
	for id := range desiredIDs {
		if _, ok := seen[id]; !ok {
			return fmt.Errorf("manifest conversation %q is absent from conversations.jsonl", id)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT conversation_id FROM conversations`)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if _, ok := seen[id]; !ok {
			stale = append(stale, id)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE conversation_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE conversation_id = ?`, id); err != nil {
			return err
		}
	}
	return recordIndexedFile(ctx, tx, file)
}

func (a *Archive) indexMessages(ctx context.Context, tx *sql.Tx, file desiredFile) error {
	path, err := safeArchivePath(a.dir, file.Path)
	if err != nil {
		return err
	}
	if err := verifySourceHash(path, file.SHA256); err != nil {
		return err
	}
	messages, err := readJSONL[Message](path)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS archive_desired_message_ids (message_id TEXT PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return fmt.Errorf("initialize desired message IDs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM archive_desired_message_ids`); err != nil {
		return fmt.Errorf("clear desired message IDs: %w", err)
	}
	rememberID, err := tx.PrepareContext(ctx, `INSERT INTO archive_desired_message_ids (message_id) VALUES (?)`)
	if err != nil {
		return fmt.Errorf("prepare desired message ID insert: %w", err)
	}
	defer rememberID.Close()
	insert, err := tx.PrepareContext(ctx, `
		INSERT INTO messages (
			message_id, conversation_id, source_platform, sender_id, body,
			timestamp_ms, status, is_from_me, media_id, mime_type,
			decryption_key, reactions_json, reply_to_id, raw_proto, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, NULL, ?)
		ON CONFLICT(message_id) DO UPDATE SET
			conversation_id = excluded.conversation_id,
			source_platform = excluded.source_platform,
			sender_id = excluded.sender_id,
			body = excluded.body,
			timestamp_ms = excluded.timestamp_ms,
			status = excluded.status,
			is_from_me = excluded.is_from_me,
			media_id = excluded.media_id,
			mime_type = excluded.mime_type,
			reactions_json = excluded.reactions_json,
			reply_to_id = excluded.reply_to_id,
			updated_at = excluded.updated_at
		WHERE messages.conversation_id IS NOT excluded.conversation_id
			OR messages.source_platform IS NOT excluded.source_platform
			OR messages.sender_id IS NOT excluded.sender_id
			OR messages.body IS NOT excluded.body
			OR messages.timestamp_ms IS NOT excluded.timestamp_ms
			OR messages.status IS NOT excluded.status
			OR messages.is_from_me IS NOT excluded.is_from_me
			OR messages.media_id IS NOT excluded.media_id
			OR messages.mime_type IS NOT excluded.mime_type
			OR messages.reactions_json IS NOT excluded.reactions_json
			OR messages.reply_to_id IS NOT excluded.reply_to_id`)
	if err != nil {
		return fmt.Errorf("prepare cached message insert: %w", err)
	}
	defer insert.Close()
	now := time.Now().UnixMilli()
	for _, message := range messages {
		if message.ID == "" || message.ConversationID != file.ConversationID {
			return fmt.Errorf("message %q belongs to conversation %q, expected %q", message.ID, message.ConversationID, file.ConversationID)
		}
		if _, err := rememberID.ExecContext(ctx, message.ID); err != nil {
			return fmt.Errorf("remember message %q for conversation %q: %w", message.ID, file.ConversationID, err)
		}
		var reactions any
		if len(message.Reactions) > 0 && string(message.Reactions) != "null" {
			reactions = string(message.Reactions)
		}
		if _, err := insert.ExecContext(ctx,
			message.ID, message.ConversationID, message.SourcePlatform, message.SenderID, message.Body,
			message.TimestampMS, message.Status, boolInt(message.IsFromMe), message.MediaID, message.MimeType,
			reactions, message.ReplyToID, now); err != nil {
			return fmt.Errorf("index message %q: %w", message.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE conversation_id = ? AND message_id NOT IN (SELECT message_id FROM archive_desired_message_ids)`, file.ConversationID); err != nil {
		return fmt.Errorf("remove stale cached messages for %q: %w", file.ConversationID, err)
	}
	return recordIndexedFile(ctx, tx, file)
}

func recordIndexedFile(ctx context.Context, tx *sql.Tx, file desiredFile) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO archive_cache_files (path, sha256, kind, conversation_id, indexed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			sha256 = excluded.sha256,
			kind = excluded.kind,
			conversation_id = excluded.conversation_id,
			indexed_at = excluded.indexed_at`,
		file.Path, file.SHA256, file.Kind, file.ConversationID, time.Now().UnixMilli())
	return err
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func verifySourceHash(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open source file for verification: %w", err)
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return fmt.Errorf("hash source file: %w", err)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(got, expected) {
		return fmt.Errorf("source file %s does not match manifest SHA-256", filepath.Base(path))
	}
	return nil
}

func readJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), maxJSONLRecordSize)
	values := make([]T, 0)
	line := 0
	for scanner.Scan() {
		line++
		var value T
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			return nil, fmt.Errorf("decode %s line %d: %w", filepath.Base(path), line, err)
		}
		values = append(values, value)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return values, nil
}

func safeArchivePath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", fmt.Errorf("unsafe archive path %q", relative)
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe archive path %q", relative)
	}
	path := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe archive path %q", relative)
	}
	return path, nil
}

func compactPreview(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}
