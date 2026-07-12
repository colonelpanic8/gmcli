package androidtelephony

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutputFilesBoundsDescriptorsAndAppends(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "threads"), 0o700); err != nil {
		t.Fatal(err)
	}
	outputs := newOutputFiles(dir, 2)
	for round := 0; round < 3; round++ {
		for thread := 0; thread < 7; thread++ {
			key := filepath.ToSlash(filepath.Join("threads", string(rune('a'+thread))+".jsonl"))
			if err := outputs.write(key, []byte{byte('0' + round), '\n'}); err != nil {
				t.Fatal(err)
			}
			if outputs.open > 2 {
				t.Fatalf("open descriptors = %d, want at most 2", outputs.open)
			}
		}
	}
	if err := outputs.closeAll(); err != nil {
		t.Fatal(err)
	}
	if outputs.open != 0 {
		t.Fatalf("open descriptors after close = %d", outputs.open)
	}
	for key, output := range outputs.files {
		if output.records != 3 {
			t.Fatalf("%s records = %d, want 3", key, output.records)
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key)))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "0\n1\n2\n" {
			t.Fatalf("%s contents = %q", key, data)
		}
		if info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(key))); err != nil {
			t.Fatal(err)
		} else if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s permissions = %o, want 600", key, info.Mode().Perm())
		}
	}
}

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

func TestSegmentRawPreservesOrphanedMMSRowsAndMedia(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("orphaned attachment")
	digest := sha256.Sum256(payload)
	checksum := hex.EncodeToString(digest[:])
	lines := []string{
		`{"record_type":"metadata","format":"gmcli-android-telephony","format_version":1,"device_serial":"pixel"}`,
		`{"record_type":"mms_address","mms_id":99,"values":{"_id":{"type":"integer","value":10}}}`,
		`{"record_type":"mms_part","mms_id":99,"values":{"_id":{"type":"integer","value":11},"mid":{"type":"integer","value":99}}}`,
		`{"record_type":"mms_part_data","source_uri":"content://mms/part/11","part_id":11,"mms_id":99,"encoding":"base64","data":"` + base64.StdEncoding.EncodeToString(payload) + `","byte_length":19,"sha256":"` + checksum + `"}`,
		`{"record_type":"summary","complete":true}`,
	}
	result, archiveManifest, err := segmentRaw(strings.NewReader(strings.Join(lines, "\n")+"\n"), dir, "pixel")
	if err != nil {
		t.Fatal(err)
	}
	if result.Threads != 0 || result.Records != len(lines) || result.MediaFiles != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	var orphanFile manifestFile
	for _, file := range archiveManifest.Files {
		if file.Path == "orphaned-mms.jsonl" {
			orphanFile = file
		}
	}
	if orphanFile.Path == "" || orphanFile.Records != 3 {
		t.Fatalf("unexpected orphan manifest entry: %+v", orphanFile)
	}
	data, err := os.ReadFile(filepath.Join(dir, "orphaned-mms.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), base64.StdEncoding.EncodeToString(payload)) || !strings.Contains(string(data), `"media_path":"media/`+checksum+`"`) {
		t.Fatalf("orphan JSONL did not replace payload with media reference: %s", data)
	}
	if err := writeManifest(filepath.Join(dir, "manifest.json"), archiveManifest); err != nil {
		t.Fatal(err)
	}
	verified, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Records != result.Records || verified.MediaFiles != 1 {
		t.Fatalf("unexpected verification result: %+v", verified)
	}
}

func TestSegmentRawTreatsCompleteSummaryAsTerminal(t *testing.T) {
	input := &terminalSummaryReader{data: []byte("{\"record_type\":\"metadata\"}\n{\"record_type\":\"summary\",\"complete\":true}\n")}
	result, _, err := segmentRaw(input, t.TempDir(), "pixel")
	if err != nil {
		t.Fatal(err)
	}
	if result.Records != 2 || input.readAfterData {
		t.Fatalf("result = %+v, read after terminal summary = %v", result, input.readAfterData)
	}
}

type terminalSummaryReader struct {
	data          []byte
	readAfterData bool
}

func (reader *terminalSummaryReader) Read(p []byte) (int, error) {
	if len(reader.data) == 0 {
		reader.readAfterData = true
		return 0, errors.New("read after terminal summary")
	}
	n := copy(p, reader.data)
	reader.data = reader.data[n:]
	return n, nil
}

func TestSegmentRawManyThreadsWithBoundedDescriptors(t *testing.T) {
	const threadCount = 1500
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lines := make([]string, 0, threadCount+2)
	lines = append(lines, `{"record_type":"metadata","format":"gmcli-android-telephony","format_version":1,"device_serial":"pixel"}`)
	for thread := 1; thread <= threadCount; thread++ {
		lines = append(lines, row("sms", map[string]int64{"_id": int64(thread), "thread_id": int64(thread)}))
	}
	lines = append(lines, `{"record_type":"summary","complete":true}`)
	result, archiveManifest, err := segmentRaw(strings.NewReader(strings.Join(lines, "\n")+"\n"), dir, "pixel")
	if err != nil {
		t.Fatal(err)
	}
	if result.Threads != threadCount || result.Records != threadCount+2 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(archiveManifest.Threads) != threadCount {
		t.Fatalf("manifest threads = %d, want %d", len(archiveManifest.Threads), threadCount)
	}
	if err := writeManifest(filepath.Join(dir, "manifest.json"), archiveManifest); err != nil {
		t.Fatal(err)
	}
	verified, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Threads != threadCount || verified.Records != result.Records {
		t.Fatalf("unexpected verification result: %+v", verified)
	}
}

func TestInstallDirectoryRejectsInvalidCandidateBeforeReplacingDestination(t *testing.T) {
	parent := t.TempDir()
	destination := filepath.Join(parent, "archive")
	tmp := filepath.Join(parent, "candidate")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "existing"), []byte("good"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "manifest.json"), []byte("not JSON"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := installDirectory(tmp, destination, true)
	if err == nil || !strings.Contains(err.Error(), "pre-install") {
		t.Fatalf("expected pre-install verification failure, got %v", err)
	}
	if data, readErr := os.ReadFile(filepath.Join(destination, "existing")); readErr != nil || string(data) != "good" {
		t.Fatalf("existing archive was replaced: data=%q err=%v", data, readErr)
	}
	if _, statErr := os.Stat(tmp); statErr != nil {
		t.Fatalf("candidate should remain available to caller cleanup: %v", statErr)
	}
	if backups, globErr := filepath.Glob(filepath.Join(parent, ".archive.old-*")); globErr != nil || len(backups) != 0 {
		t.Fatalf("unexpected backup debris: paths=%v err=%v", backups, globErr)
	}
}

func TestInstallDirectoryRollsBackPostInstallVerificationFailure(t *testing.T) {
	parent := t.TempDir()
	destination := filepath.Join(parent, "archive")
	tmp := filepath.Join(parent, "candidate")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "existing"), []byte("good"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "candidate"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	verificationCalls := 0
	err := installDirectoryWithVerifier(tmp, destination, true, func(string) error {
		verificationCalls++
		if verificationCalls == 2 {
			return errors.New("simulated post-install corruption")
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("expected rolled-back post-install failure, got %v", err)
	}
	if verificationCalls != 2 {
		t.Fatalf("verification calls = %d, want 2", verificationCalls)
	}
	if data, readErr := os.ReadFile(filepath.Join(destination, "existing")); readErr != nil || string(data) != "good" {
		t.Fatalf("existing archive was not restored: data=%q err=%v", data, readErr)
	}
	if data, readErr := os.ReadFile(filepath.Join(tmp, "candidate")); readErr != nil || string(data) != "new" {
		t.Fatalf("failed candidate was not moved back for cleanup: data=%q err=%v", data, readErr)
	}
	if backups, globErr := filepath.Glob(filepath.Join(parent, ".archive.old-*")); globErr != nil || len(backups) != 0 {
		t.Fatalf("unexpected backup debris: paths=%v err=%v", backups, globErr)
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
