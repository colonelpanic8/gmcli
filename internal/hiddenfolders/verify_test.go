package hiddenfolders

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerify(t *testing.T) {
	dir := writeTestAudit(t,
		`{"record_type":"conversation_observation","format_version":1,"audit_conversation_id":"audit:one","folder":"ARCHIVE"}`,
		`{"record_type":"message_observation","format_version":1,"audit_conversation_id":"audit:one","body":"hello"}`,
		`{"record_type":"reconciliation","format_version":1,"audit_conversation_id":"audit:one","status":"matched"}`,
	)
	result, err := Verify(dir)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.Format != manifestFormat || result.FormatVersion != manifestFormatVersion || result.Conversations != 1 || result.Records != 3 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.Bytes == 0 || result.Path != dir {
		t.Fatalf("result lacks path or bytes: %#v", result)
	}
}

func TestVerifyRejectsUnsafeAndDuplicateManifestEntries(t *testing.T) {
	tests := []struct {
		name string
		edit func(*manifest)
		want string
	}{
		{"parent traversal", func(value *manifest) { value.ConversationFiles[0].Path = "../outside.jsonl" }, "unsafe manifest path"},
		{"absolute path", func(value *manifest) { value.ConversationFiles[0].Path = "/tmp/outside.jsonl" }, "unsafe manifest path"},
		{"noncanonical path", func(value *manifest) { value.ConversationFiles[0].Path = "conversations/../conversation.jsonl" }, "unsafe manifest path"},
		{"duplicate ID", func(value *manifest) {
			value.ConversationFiles = append(value.ConversationFiles, value.ConversationFiles[0])
		}, "repeats audit conversation ID"},
		{"duplicate path", func(value *manifest) {
			duplicate := value.ConversationFiles[0]
			duplicate.AuditConversationID = "audit:two"
			value.ConversationFiles = append(value.ConversationFiles, duplicate)
		}, "repeats conversation path"},
		{"manifest conflict", func(value *manifest) { value.ConversationFiles[0].Path = "manifest.json" }, "conflicts with manifest.json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := writeTestAudit(t, testConversationObservation("audit:one"))
			modifyManifest(t, dir, test.edit)
			assertVerifyError(t, dir, test.want)
		})
	}
}

func TestVerifyRejectsSymlinkComponent(t *testing.T) {
	dir := writeTestAudit(t, testConversationObservation("audit:one"))
	outside := t.TempDir()
	data := []byte(testConversationObservation("audit:one") + "\n")
	if err := os.WriteFile(filepath.Join(outside, "conversation.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	modifyManifest(t, dir, func(value *manifest) {
		value.ConversationFiles[0].Path = "linked/conversation.jsonl"
		value.ConversationFiles[0].Bytes = int64(len(data))
		sum := sha256.Sum256(data)
		value.ConversationFiles[0].SHA256 = hex.EncodeToString(sum[:])
	})
	assertVerifyError(t, dir, "traverses a symlink")
}

func TestVerifyRejectsUnmanifestedFile(t *testing.T) {
	dir := writeTestAudit(t, testConversationObservation("audit:one"))
	if err := os.WriteFile(filepath.Join(dir, "extra.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertVerifyError(t, dir, "unmanifested file")
}

func TestVerifyRejectsIntegrityFailures(t *testing.T) {
	tests := []struct {
		name string
		edit func(string, *manifest)
		want string
	}{
		{"byte count", func(_ string, value *manifest) { value.ConversationFiles[0].Bytes++ }, "byte count mismatch"},
		{"hash", func(_ string, value *manifest) { value.ConversationFiles[0].SHA256 = strings.Repeat("0", 64) }, "SHA-256 mismatch"},
		{"record count", func(_ string, value *manifest) { value.ConversationFiles[0].Records++ }, "record count mismatch"},
		{"invalid hash syntax", func(_ string, value *manifest) { value.ConversationFiles[0].SHA256 = "not-a-hash" }, "invalid SHA-256"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := writeTestAudit(t, testConversationObservation("audit:one"))
			modifyManifest(t, dir, func(value *manifest) { test.edit(dir, value) })
			assertVerifyError(t, dir, test.want)
		})
	}
}

func TestVerifyRejectsJSONLRecordSchemaFailures(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{"malformed JSON", `{not-json}`, "decode"},
		{"unknown record type", `{"record_type":"other","format_version":1,"audit_conversation_id":"audit:one"}`, "unsupported record_type"},
		{"missing record type", `{"format_version":1,"audit_conversation_id":"audit:one"}`, "unsupported record_type"},
		{"wrong format version", `{"record_type":"conversation_observation","format_version":2,"audit_conversation_id":"audit:one"}`, "unsupported format_version"},
		{"missing format version", `{"record_type":"conversation_observation","audit_conversation_id":"audit:one"}`, "unsupported format_version"},
		{"wrong conversation", testConversationObservation("audit:two"), "belongs to audit conversation"},
		{"no conversation observation", `{"record_type":"message_observation","format_version":1,"audit_conversation_id":"audit:one"}`, "0 conversation_observation records"},
		{"blank line", testConversationObservation("audit:one") + "\n", "line 2 is blank"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := writeTestAudit(t, test.line)
			assertVerifyError(t, dir, test.want)
		})
	}
}

func TestVerifyRejectsUnsupportedManifestFolder(t *testing.T) {
	dir := writeTestAudit(t, testConversationObservation("audit:one"))
	modifyManifest(t, dir, func(value *manifest) { value.ConversationFiles[0].Folder = "INBOX" })
	assertVerifyError(t, dir, `unsupported folder "INBOX"`)
}

func TestVerifyRequiresExactlyOneConversationObservation(t *testing.T) {
	dir := writeTestAudit(t,
		testConversationObservation("audit:one"),
		testConversationObservation("audit:one"),
	)
	assertVerifyError(t, dir, "2 conversation_observation records, want exactly one")
}

func TestVerifyRequiresConversationObservationFolderMatch(t *testing.T) {
	dir := writeTestAudit(t, `{"record_type":"conversation_observation","format_version":1,"audit_conversation_id":"audit:one","folder":"SPAM_BLOCKED"}`)
	assertVerifyError(t, dir, `observes folder "SPAM_BLOCKED", want "ARCHIVE"`)
}

func TestVerifyRejectsEmptyConversationFileList(t *testing.T) {
	dir := writeTestAudit(t, testConversationObservation("audit:one"))
	modifyManifest(t, dir, func(value *manifest) { value.ConversationFiles = nil })
	assertVerifyError(t, dir, "manifest contains no conversation files")
}

func TestVerifyRejectsManifestSchemaFailures(t *testing.T) {
	t.Run("wrong format", func(t *testing.T) {
		dir := writeTestAudit(t, testConversationObservation("audit:one"))
		modifyManifest(t, dir, func(value *manifest) { value.Format = "other" })
		assertVerifyError(t, dir, "unsupported hidden-folder audit format")
	})
	t.Run("wrong version", func(t *testing.T) {
		dir := writeTestAudit(t, testConversationObservation("audit:one"))
		modifyManifest(t, dir, func(value *manifest) { value.FormatVersion = 2 })
		assertVerifyError(t, dir, "unsupported hidden-folder audit format")
	})
	t.Run("unknown field", func(t *testing.T) {
		dir := writeTestAudit(t, testConversationObservation("audit:one"))
		path := filepath.Join(dir, "manifest.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = []byte(strings.Replace(string(data), `"format":`, `"unexpected":true,"format":`, 1))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		assertVerifyError(t, dir, "unknown field")
	})
	t.Run("trailing value", func(t *testing.T) {
		dir := writeTestAudit(t, testConversationObservation("audit:one"))
		path := filepath.Join(dir, "manifest.json")
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("{}\n"); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		assertVerifyError(t, dir, "trailing JSON value")
	})
}

func testConversationObservation(id string) string {
	return `{"record_type":"conversation_observation","format_version":1,"audit_conversation_id":"` + id + `","folder":"ARCHIVE"}`
}

func writeTestAudit(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "conversations"), 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(strings.Join(lines, "\n") + "\n")
	relativePath := "conversations/YXVkaXQ6b25l.jsonl"
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(relativePath)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	value := manifest{
		Format: manifestFormat, FormatVersion: manifestFormatVersion, GeneratedAt: "2026-07-13T00:58:30Z", Source: "android_messages_ui",
		FolderSnapshots: []json.RawMessage{}, Limitations: []string{},
		ConversationFiles: []conversationFile{{
			AuditConversationID: "audit:one", Folder: "ARCHIVE", Path: relativePath,
			Records: len(lines), Bytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
		}},
	}
	writeTestManifest(t, dir, value)
	return dir
}

func modifyManifest(t *testing.T, dir string, edit func(*manifest)) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value manifest
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	edit(&value)
	writeTestManifest(t, dir, value)
}

func writeTestManifest(t *testing.T, dir string, value manifest) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertVerifyError(t *testing.T, dir, want string) {
	t.Helper()
	_, err := Verify(dir)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Verify error = %v, want substring %q", err, want)
	}
}
