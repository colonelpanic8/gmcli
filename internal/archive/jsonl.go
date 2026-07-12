package archive

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fdsouvenir/gmcli/internal/store"
)

type jsonlManifest struct {
	Format               string                  `json:"format"`
	FormatVersion        int                     `json:"format_version"`
	SchemaVersion        int                     `json:"schema_version"`
	ExportedAt           time.Time               `json:"exported_at"`
	Files                map[string]jsonlFile    `json:"files"`
	ConversationMessages []jsonlConversationFile `json:"conversation_messages"`
}

type jsonlFile struct {
	Path    string `json:"path"`
	Records int    `json:"records"`
	SHA256  string `json:"sha256"`
}

type jsonlConversationFile struct {
	ConversationID string `json:"conversation_id"`
	Path           string `json:"path"`
	Messages       int    `json:"messages"`
	SHA256         string `json:"sha256"`
}

const jsonlFormatVersion = 3

type contactLookupValue struct {
	SourcePlatform  string `json:"source_platform"`
	ContactID       string `json:"contact_id"`
	Name            string `json:"name"`
	E164            string `json:"e164"`
	FormattedNumber string `json:"formatted_number"`
	AvatarColor     string `json:"avatar_color"`
	IsMe            bool   `json:"is_me"`
}

type aliasLookupValue struct {
	Alias     string    `json:"alias"`
	UpdatedAt time.Time `json:"updated_at"`
}

type coverageLookup struct {
	Version       int                                   `json:"version"`
	Folders       map[string]coverageFolderLookup       `json:"folders"`
	Conversations map[string]coverageConversationLookup `json:"conversations"`
}

type coverageFolderLookup struct {
	Status            string     `json:"status"`
	PagesFetched      int        `json:"pages_fetched"`
	ConversationsSeen int        `json:"conversations_seen"`
	LastAttemptAt     *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt     *time.Time `json:"last_success_at,omitempty"`
	TerminalReason    string     `json:"terminal_reason,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
}

type coverageConversationLookup struct {
	Status             string                  `json:"status"`
	HistoryStartMS     *int64                  `json:"history_start_ms,omitempty"`
	LastAttemptAt      *time.Time              `json:"last_attempt_at,omitempty"`
	LastSuccessAt      *time.Time              `json:"last_success_at,omitempty"`
	ExhaustedAt        *time.Time              `json:"exhausted_at,omitempty"`
	TerminalReason     string                  `json:"terminal_reason,omitempty"`
	LastError          string                  `json:"last_error,omitempty"`
	LastRequests       int                     `json:"last_requests"`
	LastRecordsFetched int                     `json:"last_records_fetched"`
	Segments           []store.CoverageSegment `json:"segments"`
}

// VerifyResult describes a successfully verified JSONL archive.
type VerifyResult struct {
	Path          string `json:"path"`
	Format        string `json:"format"`
	FormatVersion int    `json:"format_version"`
	SchemaVersion int    `json:"schema_version"`
	Conversations int    `json:"conversations"`
	Messages      int    `json:"messages"`
	Contacts      int    `json:"contacts"`
	Aliases       int    `json:"aliases"`
}

// WriteJSONL writes a consistent segmented snapshot. Conversations and
// messages are JSONL; small keyed metadata tables are deterministic JSON
// objects. A manifest records format metadata, row counts, and checksums. The
// destination is installed only after every file has been synced successfully.
func WriteJSONL(ctx context.Context, st *store.Store, path string, force bool) (Result, error) {
	if path == "" {
		return Result{}, fmt.Errorf("output directory is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Result{}, fmt.Errorf("resolve output directory: %w", err)
	}
	if !force {
		if _, err := os.Stat(abs); err == nil {
			return Result{}, fmt.Errorf("output already exists: %s (use --force to replace it)", abs)
		} else if !os.IsNotExist(err) {
			return Result{}, fmt.Errorf("inspect output %s: %w", abs, err)
		}
	}
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return Result{}, fmt.Errorf("create output parent: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, ".gmcli-jsonl-*")
	if err != nil {
		return Result{}, fmt.Errorf("create temporary export directory: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := os.Chmod(tmp, 0o700); err != nil {
		return Result{}, fmt.Errorf("secure temporary export directory: %w", err)
	}

	result, manifest, err := writeJSONLSnapshot(ctx, st, tmp)
	if err != nil {
		return Result{}, err
	}
	if err := writeManifest(filepath.Join(tmp, "manifest.json"), manifest); err != nil {
		return Result{}, err
	}
	if err := installDirectory(tmp, abs, force); err != nil {
		return Result{}, err
	}
	keep = true
	result.Path = abs
	return result, nil
}

func writeJSONLSnapshot(ctx context.Context, st *store.Store, dir string) (Result, jsonlManifest, error) {
	tx, err := st.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Result{}, jsonlManifest{}, fmt.Errorf("begin archive snapshot: %w", err)
	}
	defer tx.Rollback()

	var schemaVersion int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&schemaVersion); err != nil {
		return Result{}, jsonlManifest{}, fmt.Errorf("read schema version: %w", err)
	}
	result := Result{Format: "gmcli-jsonl-archive", FormatVersion: jsonlFormatVersion, SchemaVersion: schemaVersion}

	conversationFile, err := writeJSONLFile(ctx, tx, filepath.Join(dir, "conversations.jsonl"), conversationsQuery, scanConversation)
	if err != nil {
		return Result{}, jsonlManifest{}, err
	}
	result.Conversations = conversationFile.Records
	contactFile, err := writeContactLookup(ctx, tx, filepath.Join(dir, "contacts.json"))
	if err != nil {
		return Result{}, jsonlManifest{}, err
	}
	result.Contacts = contactFile.Records
	aliasFile, err := writeAliasLookup(ctx, tx, filepath.Join(dir, "aliases.json"))
	if err != nil {
		return Result{}, jsonlManifest{}, err
	}
	result.Aliases = aliasFile.Records
	coverageFile, err := writeCoverageLookup(ctx, tx, filepath.Join(dir, "coverage.json"))
	if err != nil {
		return Result{}, jsonlManifest{}, err
	}

	conversationIDs, err := listConversationIDs(ctx, tx)
	if err != nil {
		return Result{}, jsonlManifest{}, err
	}
	messagesDir := filepath.Join(dir, "messages")
	if err := os.Mkdir(messagesDir, 0o700); err != nil {
		return Result{}, jsonlManifest{}, fmt.Errorf("create messages directory: %w", err)
	}
	conversationFiles := make([]jsonlConversationFile, 0, len(conversationIDs))
	for _, conversationID := range conversationIDs {
		name := base64.RawURLEncoding.EncodeToString([]byte(conversationID)) + ".jsonl"
		relativePath := filepath.ToSlash(filepath.Join("messages", name))
		file, err := writeJSONLFile(ctx, tx, filepath.Join(dir, relativePath), `
			SELECT message_id, conversation_id, source_platform, sender_id, body,
			       timestamp_ms, status, is_from_me, media_id, mime_type,
			       reactions_json, reply_to_id
			  FROM messages
			 WHERE conversation_id = ?
			 ORDER BY timestamp_ms, message_id`, scanMessage, conversationID)
		if err != nil {
			return Result{}, jsonlManifest{}, err
		}
		result.Messages += file.Records
		conversationFiles = append(conversationFiles, jsonlConversationFile{
			ConversationID: conversationID,
			Path:           relativePath,
			Messages:       file.Records,
			SHA256:         file.SHA256,
		})
	}
	if err := tx.Commit(); err != nil {
		return Result{}, jsonlManifest{}, fmt.Errorf("finish archive snapshot: %w", err)
	}
	manifest := jsonlManifest{
		Format:        result.Format,
		FormatVersion: result.FormatVersion,
		SchemaVersion: result.SchemaVersion,
		ExportedAt:    time.Now().UTC(),
		Files: map[string]jsonlFile{
			"conversations": {Path: "conversations.jsonl", Records: conversationFile.Records, SHA256: conversationFile.SHA256},
			"contacts":      {Path: "contacts.json", Records: contactFile.Records, SHA256: contactFile.SHA256},
			"aliases":       {Path: "aliases.json", Records: aliasFile.Records, SHA256: aliasFile.SHA256},
			"coverage":      {Path: "coverage.json", Records: coverageFile.Records, SHA256: coverageFile.SHA256},
		},
		ConversationMessages: conversationFiles,
	}
	return result, manifest, nil
}

func writeCoverageLookup(ctx context.Context, tx *sql.Tx, path string) (jsonlFile, error) {
	value := coverageLookup{Version: 1, Folders: make(map[string]coverageFolderLookup), Conversations: make(map[string]coverageConversationLookup)}
	rows, err := tx.QueryContext(ctx, `
		SELECT c.conversation_id, COALESCE(cc.status, 'not_attempted'), cc.history_start_ms,
		       COALESCE(cc.last_attempt_ts, 0), COALESCE(cc.last_success_ts, 0),
		       COALESCE(cc.exhausted_at, 0), COALESCE(cc.terminal_reason, ''),
		       COALESCE(cc.last_error, ''), COALESCE(cc.last_requests, 0),
		       COALESCE(cc.last_records_fetched, 0)
		  FROM conversations c LEFT JOIN conversation_coverage cc USING (conversation_id)
		 ORDER BY c.conversation_id`)
	if err != nil {
		return jsonlFile{}, fmt.Errorf("query coverage.json conversations: %w", err)
	}
	for rows.Next() {
		var id, status, reason, lastError string
		var history sql.NullInt64
		var attempted, success, exhausted int64
		var requests, fetched int
		if err := rows.Scan(&id, &status, &history, &attempted, &success, &exhausted, &reason, &lastError, &requests, &fetched); err != nil {
			rows.Close()
			return jsonlFile{}, err
		}
		entry := coverageConversationLookup{Status: status, LastAttemptAt: archiveMillisTimePtr(attempted), LastSuccessAt: archiveMillisTimePtr(success), ExhaustedAt: archiveMillisTimePtr(exhausted), TerminalReason: reason, LastError: lastError, LastRequests: requests, LastRecordsFetched: fetched, Segments: []store.CoverageSegment{}}
		if history.Valid {
			v := history.Int64
			entry.HistoryStartMS = &v
		}
		value.Conversations[id] = entry
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return jsonlFile{}, err
	}
	if err := rows.Close(); err != nil {
		return jsonlFile{}, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT conversation_id, start_ms, end_ms, verified_at FROM conversation_coverage_segments ORDER BY conversation_id, start_ms`)
	if err != nil {
		return jsonlFile{}, err
	}
	for rows.Next() {
		var id string
		var segment store.CoverageSegment
		var verified int64
		if err := rows.Scan(&id, &segment.StartMS, &segment.EndMS, &verified); err != nil {
			rows.Close()
			return jsonlFile{}, err
		}
		segment.VerifiedAt = archiveMillisTime(verified)
		entry := value.Conversations[id]
		entry.Segments = append(entry.Segments, segment)
		value.Conversations[id] = entry
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return jsonlFile{}, err
	}
	if err := rows.Close(); err != nil {
		return jsonlFile{}, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT folder, status, pages_fetched, conversations_seen, last_attempt_ts, last_success_ts, terminal_reason, last_error FROM folder_coverage ORDER BY folder`)
	if err != nil {
		return jsonlFile{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var folder, status, reason, lastError string
		var pages, seen int
		var attempted, success int64
		if err := rows.Scan(&folder, &status, &pages, &seen, &attempted, &success, &reason, &lastError); err != nil {
			return jsonlFile{}, err
		}
		value.Folders[folder] = coverageFolderLookup{Status: status, PagesFetched: pages, ConversationsSeen: seen, LastAttemptAt: archiveMillisTimePtr(attempted), LastSuccessAt: archiveMillisTimePtr(success), TerminalReason: reason, LastError: lastError}
	}
	if err := rows.Err(); err != nil {
		return jsonlFile{}, err
	}
	return writeLookupFile(path, value, len(value.Conversations))
}

func archiveMillisTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value).UTC()
}

func archiveMillisTimePtr(value int64) *time.Time {
	if value <= 0 {
		return nil
	}
	t := time.UnixMilli(value).UTC()
	return &t
}

func writeContactLookup(ctx context.Context, tx *sql.Tx, path string) (jsonlFile, error) {
	rows, err := tx.QueryContext(ctx, contactsQuery)
	if err != nil {
		return jsonlFile{}, fmt.Errorf("query contacts.json: %w", err)
	}
	defer rows.Close()
	values := make(map[string]contactLookupValue)
	for rows.Next() {
		contact, err := scanContact(rows)
		if err != nil {
			return jsonlFile{}, fmt.Errorf("scan contacts.json: %w", err)
		}
		values[contact.ParticipantID] = contactLookupValue{
			SourcePlatform: contact.SourcePlatform, ContactID: contact.ContactID,
			Name: contact.Name, E164: contact.E164, FormattedNumber: contact.FormattedNumber,
			AvatarColor: contact.AvatarColor, IsMe: contact.IsMe,
		}
	}
	if err := rows.Err(); err != nil {
		return jsonlFile{}, fmt.Errorf("read contacts.json: %w", err)
	}
	return writeLookupFile(path, values, len(values))
}

func writeAliasLookup(ctx context.Context, tx *sql.Tx, path string) (jsonlFile, error) {
	rows, err := tx.QueryContext(ctx, aliasesQuery)
	if err != nil {
		return jsonlFile{}, fmt.Errorf("query aliases.json: %w", err)
	}
	defer rows.Close()
	values := make(map[string]map[string]aliasLookupValue)
	count := 0
	for rows.Next() {
		alias, err := scanAlias(rows)
		if err != nil {
			return jsonlFile{}, fmt.Errorf("scan aliases.json: %w", err)
		}
		targetType := string(alias.TargetType)
		if values[targetType] == nil {
			values[targetType] = make(map[string]aliasLookupValue)
		}
		values[targetType][alias.TargetID] = aliasLookupValue{Alias: alias.Alias, UpdatedAt: alias.UpdatedAt}
		count++
	}
	if err := rows.Err(); err != nil {
		return jsonlFile{}, fmt.Errorf("read aliases.json: %w", err)
	}
	return writeLookupFile(path, values, count)
}

func writeLookupFile(path string, value any, records int) (jsonlFile, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return jsonlFile{}, fmt.Errorf("create %s: %w", path, err)
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	hash := sha256.New()
	enc := json.NewEncoder(io.MultiWriter(f, hash))
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return jsonlFile{}, fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	if err := f.Sync(); err != nil {
		return jsonlFile{}, fmt.Errorf("sync %s: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		return jsonlFile{}, fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	ok = true
	return jsonlFile{Path: filepath.Base(path), Records: records, SHA256: fmt.Sprintf("%x", hash.Sum(nil))}, nil
}

func listConversationIDs(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT conversation_id FROM conversations ORDER BY conversation_id`)
	if err != nil {
		return nil, fmt.Errorf("query conversation IDs: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan conversation ID: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func writeJSONLFile[T any](ctx context.Context, tx *sql.Tx, path, query string, scan func(*sql.Rows) (T, error), args ...any) (jsonlFile, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return jsonlFile{}, fmt.Errorf("query %s: %w", filepath.Base(path), err)
	}
	defer rows.Close()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return jsonlFile{}, fmt.Errorf("create %s: %w", path, err)
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	hash := sha256.New()
	w := bufio.NewWriter(io.MultiWriter(f, hash))
	count := 0
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return jsonlFile{}, fmt.Errorf("scan %s: %w", filepath.Base(path), err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return jsonlFile{}, fmt.Errorf("encode %s: %w", filepath.Base(path), err)
		}
		if _, err := w.Write(encoded); err != nil {
			return jsonlFile{}, err
		}
		if err := w.WriteByte('\n'); err != nil {
			return jsonlFile{}, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return jsonlFile{}, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if err := w.Flush(); err != nil {
		return jsonlFile{}, fmt.Errorf("flush %s: %w", filepath.Base(path), err)
	}
	if err := f.Sync(); err != nil {
		return jsonlFile{}, fmt.Errorf("sync %s: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		return jsonlFile{}, fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	ok = true
	return jsonlFile{Path: filepath.Base(path), Records: count, SHA256: fmt.Sprintf("%x", hash.Sum(nil))}, nil
}

func writeManifest(path string, manifest jsonlManifest) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create manifest: %w", err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		_ = f.Close()
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync manifest: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close manifest: %w", err)
	}
	return nil
}

func installDirectory(tmp, target string, force bool) error {
	if _, err := os.Stat(target); os.IsNotExist(err) {
		if err := os.Rename(tmp, target); err != nil {
			return fmt.Errorf("install JSONL export: %w", err)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect existing JSONL export: %w", err)
	}
	if !force {
		return fmt.Errorf("output already exists: %s (use --force to replace it)", target)
	}
	backup, err := os.MkdirTemp(filepath.Dir(target), ".gmcli-replaced-*")
	if err != nil {
		return fmt.Errorf("reserve replacement path: %w", err)
	}
	if err := os.Remove(backup); err != nil {
		return fmt.Errorf("prepare replacement path: %w", err)
	}
	if err := os.Rename(target, backup); err != nil {
		return fmt.Errorf("move existing JSONL export: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Rename(backup, target)
		return fmt.Errorf("install JSONL export: %w", err)
	}
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("remove replaced JSONL export: %w", err)
	}
	return nil
}

// VerifyJSONL validates the manifest, SHA-256 checksums, JSON syntax, record
// counts, and per-conversation message ownership of a JSONL archive.
func VerifyJSONL(path string) (VerifyResult, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("resolve archive directory: %w", err)
	}
	manifestData, err := os.ReadFile(filepath.Join(abs, "manifest.json"))
	if err != nil {
		return VerifyResult{}, fmt.Errorf("read manifest: %w", err)
	}
	var manifest jsonlManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return VerifyResult{}, fmt.Errorf("decode manifest: %w", err)
	}
	if manifest.Format != "gmcli-jsonl-archive" || manifest.FormatVersion < 1 || manifest.FormatVersion > jsonlFormatVersion {
		return VerifyResult{}, fmt.Errorf("unsupported archive format %q version %d", manifest.Format, manifest.FormatVersion)
	}
	result := VerifyResult{
		Path:          abs,
		Format:        manifest.Format,
		FormatVersion: manifest.FormatVersion,
		SchemaVersion: manifest.SchemaVersion,
	}
	required := []string{"conversations", "contacts", "aliases"}
	if manifest.FormatVersion >= 3 {
		required = append(required, "coverage")
	}
	tablePaths := make(map[string]struct{}, len(required))
	var coverageIDs map[string]struct{}
	for _, name := range required {
		file, ok := manifest.Files[name]
		if !ok {
			return VerifyResult{}, fmt.Errorf("manifest is missing %s file", name)
		}
		if manifest.FormatVersion == 1 || name == "conversations" {
			if err := verifyJSONLFile(abs, file.Path, file.Records, file.SHA256, ""); err != nil {
				return VerifyResult{}, err
			}
		} else if name == "coverage" {
			var err error
			coverageIDs, err = verifyCoverageFile(abs, file.Path, file.Records, file.SHA256)
			if err != nil {
				return VerifyResult{}, err
			}
		} else if err := verifyLookupFile(abs, file.Path, file.Records, file.SHA256, name == "aliases"); err != nil {
			return VerifyResult{}, err
		}
		if _, exists := tablePaths[file.Path]; exists {
			return VerifyResult{}, fmt.Errorf("manifest repeats table path %q", file.Path)
		}
		tablePaths[file.Path] = struct{}{}
		switch name {
		case "conversations":
			result.Conversations = file.Records
		case "contacts":
			result.Contacts = file.Records
		case "aliases":
			result.Aliases = file.Records
		}
	}
	if len(manifest.ConversationMessages) != result.Conversations {
		return VerifyResult{}, fmt.Errorf("manifest has %d conversation message files, want %d", len(manifest.ConversationMessages), result.Conversations)
	}
	conversationFile := manifest.Files["conversations"]
	conversationIDs, err := readConversationIDs(abs, conversationFile.Path)
	if err != nil {
		return VerifyResult{}, err
	}
	if len(conversationIDs) != result.Conversations {
		return VerifyResult{}, fmt.Errorf("conversation file contains %d unique IDs, want %d", len(conversationIDs), result.Conversations)
	}
	if manifest.FormatVersion >= 3 {
		for id := range conversationIDs {
			if _, ok := coverageIDs[id]; !ok {
				return VerifyResult{}, fmt.Errorf("coverage file is missing conversation ID %q", id)
			}
		}
	}
	seenIDs := make(map[string]struct{}, len(manifest.ConversationMessages))
	seenPaths := make(map[string]struct{}, len(manifest.ConversationMessages))
	for _, file := range manifest.ConversationMessages {
		if file.ConversationID == "" {
			return VerifyResult{}, fmt.Errorf("manifest contains an empty conversation ID")
		}
		if _, exists := seenIDs[file.ConversationID]; exists {
			return VerifyResult{}, fmt.Errorf("manifest repeats conversation ID %q", file.ConversationID)
		}
		if _, exists := seenPaths[file.Path]; exists {
			return VerifyResult{}, fmt.Errorf("manifest repeats message path %q", file.Path)
		}
		if _, exists := tablePaths[file.Path]; exists {
			return VerifyResult{}, fmt.Errorf("message path %q conflicts with a table file", file.Path)
		}
		if _, exists := conversationIDs[file.ConversationID]; !exists {
			return VerifyResult{}, fmt.Errorf("message file references unknown conversation ID %q", file.ConversationID)
		}
		seenIDs[file.ConversationID] = struct{}{}
		seenPaths[file.Path] = struct{}{}
		if err := verifyJSONLFile(abs, file.Path, file.Messages, file.SHA256, file.ConversationID); err != nil {
			return VerifyResult{}, err
		}
		result.Messages += file.Messages
	}
	return result, nil
}

func verifyCoverageFile(root, relativePath string, wantRecords int, wantSHA string) (map[string]struct{}, error) {
	path, err := safeArchivePath(root, relativePath)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", relativePath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("archive path %s is not a regular file", relativePath)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", relativePath, err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != wantSHA {
		return nil, fmt.Errorf("%s SHA-256 mismatch: got %s, want %s", relativePath, got, wantSHA)
	}
	var value coverageLookup
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("decode %s: %w", relativePath, err)
	}
	if value.Version != 1 {
		return nil, fmt.Errorf("unsupported coverage format version %d", value.Version)
	}
	if len(value.Conversations) != wantRecords {
		return nil, fmt.Errorf("%s contains %d conversation records, manifest says %d", relativePath, len(value.Conversations), wantRecords)
	}
	ids := make(map[string]struct{}, len(value.Conversations))
	for id, conversation := range value.Conversations {
		if id == "" {
			return nil, fmt.Errorf("%s contains an empty conversation key", relativePath)
		}
		ids[id] = struct{}{}
		for _, segment := range conversation.Segments {
			if segment.StartMS >= segment.EndMS {
				return nil, fmt.Errorf("%s conversation %s has invalid segment [%d,%d)", relativePath, id, segment.StartMS, segment.EndMS)
			}
		}
	}
	return ids, nil
}

func verifyLookupFile(root, relativePath string, wantRecords int, wantSHA string, nested bool) error {
	path, err := safeArchivePath(root, relativePath)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", relativePath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("archive path %s is not a regular file", relativePath)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", relativePath, err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != wantSHA {
		return fmt.Errorf("%s SHA-256 mismatch: got %s, want %s", relativePath, got, wantSHA)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("decode %s: %w", relativePath, err)
	}
	records := len(values)
	if nested {
		records = 0
		for targetType, raw := range values {
			if targetType != string(store.AliasContact) && targetType != string(store.AliasConversation) {
				return fmt.Errorf("%s contains unknown alias target type %q", relativePath, targetType)
			}
			var targets map[string]json.RawMessage
			if err := json.Unmarshal(raw, &targets); err != nil {
				return fmt.Errorf("decode %s target %q: %w", relativePath, targetType, err)
			}
			for targetID, value := range targets {
				if targetID == "" {
					return fmt.Errorf("%s contains an empty alias target ID", relativePath)
				}
				var alias aliasLookupValue
				if err := json.Unmarshal(value, &alias); err != nil {
					return fmt.Errorf("decode %s alias %q: %w", relativePath, targetID, err)
				}
			}
			records += len(targets)
		}
	} else {
		for key, raw := range values {
			if key == "" {
				return fmt.Errorf("%s contains an empty lookup key", relativePath)
			}
			var contact contactLookupValue
			if err := json.Unmarshal(raw, &contact); err != nil {
				return fmt.Errorf("decode %s contact %q: %w", relativePath, key, err)
			}
		}
	}
	if records != wantRecords {
		return fmt.Errorf("%s contains %d records, manifest says %d", relativePath, records, wantRecords)
	}
	return nil
}

func verifyJSONLFile(root, relativePath string, wantRecords int, wantSHA, conversationID string) error {
	path, err := safeArchivePath(root, relativePath)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", relativePath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("archive path %s is not a regular file", relativePath)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", relativePath, err)
	}
	defer f.Close()
	hash := sha256.New()
	r := bufio.NewReader(io.TeeReader(f, hash))
	records := 0
	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := strings.TrimSpace(string(line))
			if trimmed == "" {
				return fmt.Errorf("%s line %d is blank", relativePath, records+1)
			}
			var value map[string]any
			if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
				return fmt.Errorf("decode %s line %d: %w", relativePath, records+1, err)
			}
			if conversationID != "" {
				got, _ := value["conversation_id"].(string)
				if got != conversationID {
					return fmt.Errorf("%s line %d belongs to conversation %q, want %q", relativePath, records+1, got, conversationID)
				}
			}
			records++
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read %s: %w", relativePath, readErr)
		}
	}
	if records != wantRecords {
		return fmt.Errorf("%s contains %d records, manifest says %d", relativePath, records, wantRecords)
	}
	if got := fmt.Sprintf("%x", hash.Sum(nil)); got != wantSHA {
		return fmt.Errorf("%s SHA-256 mismatch: got %s, want %s", relativePath, got, wantSHA)
	}
	return nil
}

func readConversationIDs(root, relativePath string) (map[string]struct{}, error) {
	path, err := safeArchivePath(root, relativePath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", relativePath, err)
	}
	defer f.Close()
	ids := make(map[string]struct{})
	r := bufio.NewReader(f)
	lineNumber := 0
	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) > 0 {
			lineNumber++
			var value map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(string(line))), &value); err != nil {
				return nil, fmt.Errorf("decode %s line %d: %w", relativePath, lineNumber, err)
			}
			id, _ := value["conversation_id"].(string)
			if id == "" {
				return nil, fmt.Errorf("%s line %d has no conversation_id", relativePath, lineNumber)
			}
			if _, exists := ids[id]; exists {
				return nil, fmt.Errorf("%s repeats conversation ID %q", relativePath, id)
			}
			ids[id] = struct{}{}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", relativePath, readErr)
		}
	}
	return ids, nil
}

func safeArchivePath(root, relativePath string) (string, error) {
	if relativePath == "" || filepath.IsAbs(relativePath) {
		return "", fmt.Errorf("unsafe archive path %q", relativePath)
	}
	clean := filepath.Clean(filepath.FromSlash(relativePath))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe archive path %q", relativePath)
	}
	return filepath.Join(root, clean), nil
}
