package androidtelephony

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSegmentRawRoutesThreadsAndExtractsVerifiedMMSMedia(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte{0, 1, 2, '\n', 255}
	digest := sha256.Sum256(payload)
	checksum := hex.EncodeToString(digest[:])
	lines := []string{
		`{"record_type":"metadata","format":"gmcli-android-telephony","format_version":1,"device_serial":"pixel"}`,
		row("sms", map[string]int64{"_id": 1, "thread_id": 42}),
		row("mms", map[string]int64{"_id": 7, "thread_id": 42}),
		`{"record_type":"mms_address","mms_id":7,"values":{"_id":{"type":"integer","value":9}}}`,
		`{"record_type":"mms_part","mms_id":7,"values":{"_id":{"type":"integer","value":11},"mid":{"type":"integer","value":7},"text":{"type":"string","value":"caption"}}}`,
		`{"record_type":"mms_part_data","source_uri":"content://mms/part/11","part_id":11,"mms_id":7,"encoding":"base64","data":"` + base64.StdEncoding.EncodeToString(payload) + `","byte_length":5,"sha256":"` + checksum + `"}`,
		row("thread", map[string]int64{"_id": 42}),
		`{"record_type":"canonical_address","values":{"_id":{"type":"integer","value":3},"address":{"type":"string","value":"+15551234567"}}}`,
		`{"record_type":"summary","complete":true,"counts":{"sms":1,"mms":1}}`,
	}
	result, manifest, err := segmentRaw(strings.NewReader(strings.Join(lines, "\n")+"\n"), dir, "pixel")
	if err != nil {
		t.Fatal(err)
	}
	if result.Threads != 1 || result.MediaFiles != 1 || result.MediaBytes != int64(len(payload)) {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(manifest.Threads) != 1 || manifest.Threads[0].ThreadID != "42" || manifest.Threads[0].Records != 6 {
		t.Fatalf("unexpected thread manifest: %+v", manifest.Threads)
	}
	media, err := os.ReadFile(filepath.Join(dir, "media", checksum))
	if err != nil {
		t.Fatal(err)
	}
	if string(media) != string(payload) {
		t.Fatalf("media mismatch: %v", media)
	}
	threadData, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(manifest.Threads[0].Path)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(threadData), base64.StdEncoding.EncodeToString(payload)) {
		t.Fatal("thread JSONL should reference content-addressed media, not duplicate base64")
	}
	if !strings.Contains(string(threadData), `"media_path":"media/`+checksum+`"`) || !strings.Contains(string(threadData), `"value":"caption"`) {
		t.Fatalf("thread JSONL lacks media reference or text part: %s", threadData)
	}
	if err := writeManifest(filepath.Join(dir, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	verified, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Threads != 1 || verified.MediaFiles != 1 || verified.Records != result.Records {
		t.Fatalf("unexpected verification result: %+v", verified)
	}
	if err := os.WriteFile(filepath.Join(dir, "unmanifested"), []byte("oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir); err == nil || !strings.Contains(err.Error(), "unmanifested") {
		t.Fatalf("expected unmanifested-file error, got %v", err)
	}
}

func TestSegmentRawRejectsIncompleteStream(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := segmentRaw(strings.NewReader(row("sms", map[string]int64{"_id": 1, "thread_id": 2})+"\n"), dir, "pixel"); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("expected incomplete-stream error, got %v", err)
	}
}

func TestSegmentRawRejectsCorruptMMSMedia(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		row("mms", map[string]int64{"_id": 7, "thread_id": 8}),
		`{"record_type":"mms_part_data","part_id":2,"mms_id":7,"data":"YQ==","byte_length":1,"sha256":"wrong"}`,
		`{"record_type":"summary","complete":true}`,
	}
	if _, _, err := segmentRaw(strings.NewReader(strings.Join(lines, "\n")+"\n"), dir, "pixel"); err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("expected verification error, got %v", err)
	}
}

func row(recordType string, values map[string]int64) string {
	typed := make(map[string]map[string]any, len(values))
	for key, value := range values {
		typed[key] = map[string]any{"type": "integer", "value": value}
	}
	encoded, err := json.Marshal(map[string]any{"record_type": recordType, "values": typed})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
