package unifiedarchive

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// VerifyResult describes a successfully verified unified archive.
type VerifyResult struct {
	Path               string `json:"path"`
	Format             string `json:"format"`
	FormatVersion      int    `json:"format_version"`
	Conversations      int    `json:"conversations"`
	Messages           int    `json:"messages"`
	CrossSourceMatches int    `json:"cross_source_matches"`
}

// Verify checks manifest coverage, safe paths, hashes, sizes, JSONL schemas,
// conversation ownership, ordering, unique IDs, and source provenance.
func Verify(path string) (VerifyResult, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return VerifyResult{}, err
	}
	rootInfo, err := os.Lstat(abs)
	if err != nil {
		return VerifyResult{}, err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return VerifyResult{}, fmt.Errorf("unified archive is not a real directory: %s", abs)
	}
	manifestPath := filepath.Join(abs, "manifest.json")
	if err := requireRegular(manifestPath); err != nil {
		return VerifyResult{}, err
	}
	var value manifest
	if err := decodeStrictFile(manifestPath, &value); err != nil {
		return VerifyResult{}, fmt.Errorf("decode manifest: %w", err)
	}
	if value.Format != Format || value.FormatVersion != FormatVersion {
		return VerifyResult{}, fmt.Errorf("unsupported unified archive format %q version %d", value.Format, value.FormatVersion)
	}
	if !validE164(value.SelfE164) {
		return VerifyResult{}, fmt.Errorf("manifest has invalid self_e164 %q", value.SelfE164)
	}
	if !validDigest(value.RelayManifestSHA256) || !validDigest(value.TelephonyManifestSHA256) {
		return VerifyResult{}, errors.New("manifest contains an invalid source-manifest SHA-256")
	}
	if len(value.ConversationMessages) == 0 {
		return VerifyResult{}, errors.New("manifest contains no conversation message files")
	}
	expected := map[string]struct{}{"manifest.json": {}}
	if err := verifyManifestFile(abs, value.ConversationsFile, expected); err != nil {
		return VerifyResult{}, err
	}
	if value.ConversationsFile.Records != len(value.ConversationMessages) {
		return VerifyResult{}, fmt.Errorf("conversations file has %d records, want %d", value.ConversationsFile.Records, len(value.ConversationMessages))
	}
	conversations := make(map[string]Conversation)
	if err := scanJSONL(filepath.Join(abs, filepath.FromSlash(value.ConversationsFile.Path)), func(raw []byte, line int) error {
		var conversation Conversation
		if err := json.Unmarshal(raw, &conversation); err != nil {
			return err
		}
		if conversation.RecordType != "unified_conversation" || conversation.FormatVersion != FormatVersion || conversation.CanonicalConversationID == "" {
			return fmt.Errorf("invalid unified conversation envelope")
		}
		if _, exists := conversations[conversation.CanonicalConversationID]; exists {
			return fmt.Errorf("duplicate canonical conversation %q", conversation.CanonicalConversationID)
		}
		conversations[conversation.CanonicalConversationID] = conversation
		return nil
	}); err != nil {
		return VerifyResult{}, fmt.Errorf("verify conversations: %w", err)
	}
	result := VerifyResult{Path: abs, Format: value.Format, FormatVersion: value.FormatVersion}
	seenIDs := make(map[string]struct{})
	for _, entry := range value.ConversationMessages {
		if entry.CanonicalConversationID == "" {
			return VerifyResult{}, errors.New("manifest contains empty canonical conversation ID")
		}
		conversation, ok := conversations[entry.CanonicalConversationID]
		if !ok {
			return VerifyResult{}, fmt.Errorf("manifest conversation %q is absent from conversations.jsonl", entry.CanonicalConversationID)
		}
		if !equalStrings(entry.Participants, participantNumbers(conversation.Participants)) {
			return VerifyResult{}, fmt.Errorf("conversation %q participant metadata disagrees with manifest", entry.CanonicalConversationID)
		}
		if len(entry.Participants) > 0 && canonicalConversationID(entry.Participants, "") != entry.CanonicalConversationID {
			return VerifyResult{}, fmt.Errorf("conversation %q is not canonical for its participants", entry.CanonicalConversationID)
		}
		if err := verifyManifestFile(abs, manifestFile{Path: entry.Path, Records: entry.Messages, Bytes: entry.Bytes, SHA256: entry.SHA256}, expected); err != nil {
			return VerifyResult{}, err
		}
		messages := 0
		matches := 0
		lastTimestamp := int64(-1)
		if err := scanJSONL(filepath.Join(abs, filepath.FromSlash(entry.Path)), func(raw []byte, line int) error {
			var message Message
			if err := json.Unmarshal(raw, &message); err != nil {
				return err
			}
			if message.RecordType != "unified_message" || message.FormatVersion != FormatVersion || message.CanonicalConversationID != entry.CanonicalConversationID || message.UnifiedMessageID == "" {
				return fmt.Errorf("invalid unified message envelope")
			}
			if message.TimestampMS < lastTimestamp {
				return fmt.Errorf("messages are not timestamp ordered")
			}
			lastTimestamp = message.TimestampMS
			if _, exists := seenIDs[message.UnifiedMessageID]; exists {
				return fmt.Errorf("duplicate unified message ID %q", message.UnifiedMessageID)
			}
			seenIDs[message.UnifiedMessageID] = struct{}{}
			if len(message.Sources) == 0 {
				return errors.New("unified message has no sources")
			}
			platforms := make(map[string]bool)
			for _, source := range message.Sources {
				if source.RecordID == "" || source.Path == "" || (source.Platform != "gm" && source.Platform != "android_telephony") {
					return fmt.Errorf("invalid message source")
				}
				if err := validateRelativePath(source.Path); err != nil {
					return fmt.Errorf("invalid message source path: %w", err)
				}
				if source.Platform == "gm" && source.ConversationID == "" {
					return errors.New("gm source lacks conversation_id")
				}
				if source.Platform == "android_telephony" && source.ThreadID == "" {
					return errors.New("Android source lacks thread_id")
				}
				platforms[source.Platform] = true
			}
			if platforms["gm"] && platforms["android_telephony"] {
				matches++
			}
			messages++
			return nil
		}); err != nil {
			return VerifyResult{}, fmt.Errorf("verify %q: %w", entry.Path, err)
		}
		if messages != entry.Messages || messages != conversation.Messages {
			return VerifyResult{}, fmt.Errorf("conversation %q message count mismatch: file=%d manifest=%d conversation=%d", entry.CanonicalConversationID, messages, entry.Messages, conversation.Messages)
		}
		if matches != conversation.CrossSourceMatches {
			return VerifyResult{}, fmt.Errorf("conversation %q cross-source count mismatch: file=%d conversation=%d", entry.CanonicalConversationID, matches, conversation.CrossSourceMatches)
		}
		result.Conversations++
		result.Messages += messages
		result.CrossSourceMatches += matches
	}
	if len(conversations) != result.Conversations {
		return VerifyResult{}, fmt.Errorf("conversations.jsonl has %d conversations, manifest has %d", len(conversations), result.Conversations)
	}
	if err := verifyCoverage(abs, expected); err != nil {
		return VerifyResult{}, err
	}
	return result, nil
}

func participantNumbers(participants []Participant) []string {
	values := make([]string, 0, len(participants))
	for _, participant := range participants {
		values = append(values, participant.E164)
	}
	return values
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

func verifyManifestFile(root string, entry manifestFile, expected map[string]struct{}) error {
	if err := validateRelativePath(entry.Path); err != nil {
		return err
	}
	if _, exists := expected[entry.Path]; exists {
		return fmt.Errorf("manifest repeats path %q", entry.Path)
	}
	expected[entry.Path] = struct{}{}
	path := filepath.Join(root, filepath.FromSlash(entry.Path))
	if err := requireRegularNoSymlink(root, entry.Path); err != nil {
		return err
	}
	digest, bytes, err := fileDigest(path)
	if err != nil {
		return err
	}
	if bytes != entry.Bytes || digest != entry.SHA256 {
		return fmt.Errorf("manifest integrity mismatch for %q", entry.Path)
	}
	count := 0
	if err := scanJSONL(path, func([]byte, int) error { count++; return nil }); err != nil {
		return err
	}
	if count != entry.Records {
		return fmt.Errorf("record count mismatch for %q: got %d want %d", entry.Path, count, entry.Records)
	}
	return nil
}

func verifyCoverage(root string, expected map[string]struct{}) error {
	seen := make(map[string]struct{})
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive contains symlink %q", rel)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("archive contains non-regular file %q", rel)
		}
		if _, ok := expected[rel]; !ok {
			return fmt.Errorf("unmanifested file %q", rel)
		}
		seen[rel] = struct{}{}
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

func validateRelativePath(path string) error {
	if path == "" || filepath.IsAbs(path) || strings.Contains(path, "\\") || filepath.ToSlash(filepath.Clean(path)) != path || path == "." || strings.HasPrefix(path, "../") {
		return fmt.Errorf("unsafe manifest path %q", path)
	}
	return nil
}

func requireRegular(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("not a regular file: %s", path)
	}
	return nil
}

func requireRegularNoSymlink(root, relative string) error {
	current := root
	for _, component := range strings.Split(filepath.FromSlash(relative), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("manifest path %q traverses a symlink", relative)
		}
	}
	return requireRegular(current)
}

func decodeStrictFile(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func scanJSONL(path string, visit func(raw []byte, line int) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<20)
	line := 0
	for {
		raw, readErr := reader.ReadBytes('\n')
		if len(raw) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		line++
		if len(strings.TrimSpace(string(raw))) == 0 || !json.Valid(raw) {
			return fmt.Errorf("invalid JSONL at line %d", line)
		}
		if err := visit(raw, line); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return readErr
		}
	}
	return nil
}
