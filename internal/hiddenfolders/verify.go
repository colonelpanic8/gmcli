// Package hiddenfolders verifies supplemental Google Messages folder audits
// captured from the Android UI when the relay cannot enumerate those folders.
package hiddenfolders

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	manifestFormat        = "gmcli-hidden-folder-audit"
	manifestFormatVersion = 1
)

var allowedRecordTypes = map[string]struct{}{
	"conversation_observation": {},
	"message_observation":      {},
	"reconciliation":           {},
}

var allowedFolders = map[string]struct{}{
	"ARCHIVE":      {},
	"SPAM_BLOCKED": {},
}

type manifest struct {
	Format            string             `json:"format"`
	FormatVersion     int                `json:"format_version"`
	GeneratedAt       string             `json:"generated_at"`
	Source            string             `json:"source"`
	FolderSnapshots   []json.RawMessage  `json:"folder_snapshots"`
	ConversationFiles []conversationFile `json:"conversation_files"`
	Limitations       []string           `json:"limitations"`
}

type conversationFile struct {
	AuditConversationID string `json:"audit_conversation_id"`
	Folder              string `json:"folder"`
	Path                string `json:"path"`
	Records             int    `json:"records"`
	Bytes               int64  `json:"bytes"`
	SHA256              string `json:"sha256"`
}

type recordEnvelope struct {
	RecordType          string `json:"record_type"`
	FormatVersion       int    `json:"format_version"`
	AuditConversationID string `json:"audit_conversation_id"`
	Folder              string `json:"folder"`
}

// VerifyResult describes a successfully verified hidden-folder audit.
type VerifyResult struct {
	Path          string `json:"path"`
	Format        string `json:"format"`
	FormatVersion int    `json:"format_version"`
	Conversations int    `json:"conversations"`
	Records       int    `json:"records"`
	Bytes         int64  `json:"bytes"`
}

// Verify validates the supplemental audit manifest and every declared JSONL
// file. It rejects paths which escape the audit root, symlinks, duplicate
// conversation IDs or paths, mismatched sizes/checksums/counts, malformed or
// unsupported records, and files without exactly one conversation observation.
func Verify(path string) (VerifyResult, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("resolve hidden-folder audit: %w", err)
	}
	rootInfo, err := os.Lstat(abs)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("inspect hidden-folder audit: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return VerifyResult{}, fmt.Errorf("hidden-folder audit is not a real directory: %s", abs)
	}

	manifestPath := filepath.Join(abs, "manifest.json")
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("inspect manifest: %w", err)
	}
	if manifestInfo.Mode()&os.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() {
		return VerifyResult{}, errors.New("manifest.json is not a regular file")
	}
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("read manifest: %w", err)
	}
	var auditManifest manifest
	if err := decodeStrictJSON(manifestData, &auditManifest); err != nil {
		return VerifyResult{}, fmt.Errorf("decode manifest: %w", err)
	}
	if auditManifest.Format != manifestFormat || auditManifest.FormatVersion != manifestFormatVersion {
		return VerifyResult{}, fmt.Errorf("unsupported hidden-folder audit format %q version %d", auditManifest.Format, auditManifest.FormatVersion)
	}
	if len(auditManifest.ConversationFiles) == 0 {
		return VerifyResult{}, errors.New("manifest contains no conversation files")
	}

	result := VerifyResult{Path: abs, Format: auditManifest.Format, FormatVersion: auditManifest.FormatVersion}
	seenIDs := make(map[string]struct{}, len(auditManifest.ConversationFiles))
	seenPaths := make(map[string]struct{}, len(auditManifest.ConversationFiles))
	for index, entry := range auditManifest.ConversationFiles {
		if entry.AuditConversationID == "" {
			return VerifyResult{}, fmt.Errorf("conversation_files[%d] has an empty audit_conversation_id", index)
		}
		if _, exists := seenIDs[entry.AuditConversationID]; exists {
			return VerifyResult{}, fmt.Errorf("manifest repeats audit conversation ID %q", entry.AuditConversationID)
		}
		if _, ok := allowedFolders[entry.Folder]; !ok {
			return VerifyResult{}, fmt.Errorf("conversation_files[%d] has unsupported folder %q", index, entry.Folder)
		}
		if err := validateRelativePath(entry.Path); err != nil {
			return VerifyResult{}, err
		}
		if entry.Path == "manifest.json" {
			return VerifyResult{}, errors.New("conversation path conflicts with manifest.json")
		}
		if _, exists := seenPaths[entry.Path]; exists {
			return VerifyResult{}, fmt.Errorf("manifest repeats conversation path %q", entry.Path)
		}
		if entry.Records < 0 || entry.Bytes < 0 {
			return VerifyResult{}, fmt.Errorf("manifest has negative count or size for %q", entry.Path)
		}
		if err := validateSHA256(entry.Path, entry.SHA256); err != nil {
			return VerifyResult{}, err
		}
		seenIDs[entry.AuditConversationID] = struct{}{}
		seenPaths[entry.Path] = struct{}{}

		records, bytes, err := verifyConversationFile(abs, entry)
		if err != nil {
			return VerifyResult{}, err
		}
		result.Conversations++
		result.Records += records
		result.Bytes += bytes
	}
	if err := verifyManifestCoverage(abs, seenPaths); err != nil {
		return VerifyResult{}, err
	}
	return result, nil
}

func verifyManifestCoverage(root string, conversationPaths map[string]struct{}) error {
	expected := make(map[string]struct{}, len(conversationPaths)+1)
	expected["manifest.json"] = struct{}{}
	for path := range conversationPaths {
		expected[path] = struct{}{}
	}
	seen := make(map[string]struct{}, len(expected))
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relativePath = filepath.ToSlash(relativePath)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("audit contains symlink %q", relativePath)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("audit contains non-regular file %q", relativePath)
		}
		if _, ok := expected[relativePath]; !ok {
			return fmt.Errorf("unmanifested file %q", relativePath)
		}
		seen[relativePath] = struct{}{}
		return nil
	}); err != nil {
		return err
	}
	for path := range expected {
		if _, ok := seen[path]; !ok {
			return fmt.Errorf("manifested file is missing: %q", path)
		}
	}
	return nil
}

func decodeStrictJSON(data []byte, value any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return fmt.Errorf("trailing JSON data: %w", err)
	}
	return nil
}

func validateRelativePath(path string) error {
	if path == "" || filepath.IsAbs(path) || strings.Contains(path, "\\") || path == "." ||
		filepath.ToSlash(filepath.Clean(path)) != path || strings.HasPrefix(path, "../") {
		return fmt.Errorf("unsafe manifest path %q", path)
	}
	return nil
}

func validateSHA256(path, value string) error {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size || strings.ToLower(value) != value {
		return fmt.Errorf("invalid SHA-256 for %q", path)
	}
	return nil
}

func verifyConversationFile(root string, entry conversationFile) (int, int64, error) {
	path, err := resolveRegularFile(root, entry.Path)
	if err != nil {
		return 0, 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, fmt.Errorf("inspect %q: %w", entry.Path, err)
	}
	if info.Size() != entry.Bytes {
		return 0, 0, fmt.Errorf("byte count mismatch for %q: got %d want %d", entry.Path, info.Size(), entry.Bytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("open %q: %w", entry.Path, err)
	}
	defer f.Close()

	hash := sha256.New()
	reader := bufio.NewReader(io.TeeReader(f, hash))
	records := 0
	conversationObservations := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			records++
			trimmed := strings.TrimSpace(string(line))
			if trimmed == "" {
				return 0, 0, fmt.Errorf("%s line %d is blank", entry.Path, records)
			}
			var envelope recordEnvelope
			if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
				return 0, 0, fmt.Errorf("decode %s line %d: %w", entry.Path, records, err)
			}
			if _, ok := allowedRecordTypes[envelope.RecordType]; !ok {
				return 0, 0, fmt.Errorf("%s line %d has unsupported record_type %q", entry.Path, records, envelope.RecordType)
			}
			if envelope.FormatVersion != manifestFormatVersion {
				return 0, 0, fmt.Errorf("%s line %d has unsupported format_version %d", entry.Path, records, envelope.FormatVersion)
			}
			if envelope.AuditConversationID != entry.AuditConversationID {
				return 0, 0, fmt.Errorf("%s line %d belongs to audit conversation %q, want %q", entry.Path, records, envelope.AuditConversationID, entry.AuditConversationID)
			}
			if envelope.RecordType == "conversation_observation" {
				conversationObservations++
				if envelope.Folder != entry.Folder {
					return 0, 0, fmt.Errorf("%s line %d observes folder %q, want %q", entry.Path, records, envelope.Folder, entry.Folder)
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return 0, 0, fmt.Errorf("read %q: %w", entry.Path, readErr)
		}
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != entry.SHA256 {
		return 0, 0, fmt.Errorf("SHA-256 mismatch for %q: got %s want %s", entry.Path, got, entry.SHA256)
	}
	if records != entry.Records {
		return 0, 0, fmt.Errorf("record count mismatch for %q: got %d want %d", entry.Path, records, entry.Records)
	}
	if conversationObservations != 1 {
		return 0, 0, fmt.Errorf("%q has %d conversation_observation records, want exactly one", entry.Path, conversationObservations)
	}
	return records, info.Size(), nil
}

func resolveRegularFile(root, relativePath string) (string, error) {
	current := root
	for _, component := range strings.Split(filepath.FromSlash(relativePath), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("inspect %q: %w", relativePath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("audit path %q traverses a symlink", relativePath)
		}
	}
	info, err := os.Lstat(current)
	if err != nil {
		return "", fmt.Errorf("inspect %q: %w", relativePath, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("audit path %q is not a regular file", relativePath)
	}
	return current, nil
}
