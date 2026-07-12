package androidtelephony

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// VerifyResult describes a successfully verified Android Telephony archive.
type VerifyResult struct {
	Path          string `json:"path"`
	Format        string `json:"format"`
	FormatVersion int    `json:"format_version"`
	Threads       int    `json:"threads"`
	Records       int    `json:"records"`
	MediaFiles    int    `json:"media_files"`
	MediaBytes    int64  `json:"media_bytes"`
}

// Verify rechecks the complete installed archive, including containment,
// permissions, manifest coverage, checksums, JSONL ownership, and media refs.
func Verify(path string) (VerifyResult, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return VerifyResult{}, err
	}
	rootInfo, err := os.Stat(abs)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("inspect archive: %w", err)
	}
	if !rootInfo.IsDir() {
		return VerifyResult{}, fmt.Errorf("archive is not a directory: %s", abs)
	}
	if rootInfo.Mode().Perm()&0o077 != 0 {
		return VerifyResult{}, fmt.Errorf("archive directory is accessible by group/others (chmod 700 %s)", abs)
	}
	manifestPath := filepath.Join(abs, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("read manifest: %w", err)
	}
	var archiveManifest manifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&archiveManifest); err != nil {
		return VerifyResult{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return VerifyResult{}, errors.New("manifest contains trailing JSON data")
	}
	if archiveManifest.Format != "gmcli-android-telephony" || archiveManifest.FormatVersion != 1 {
		return VerifyResult{}, fmt.Errorf("unsupported Telephony archive format %q version %d", archiveManifest.Format, archiveManifest.FormatVersion)
	}

	expected := map[string]manifestFile{
		"manifest.json": {Path: "manifest.json"},
	}
	threadIDs := make(map[string]string)
	for _, entry := range archiveManifest.Files {
		if err := addExpected(expected, entry); err != nil {
			return VerifyResult{}, err
		}
	}
	for _, thread := range archiveManifest.Threads {
		if thread.ThreadID == "" {
			return VerifyResult{}, errors.New("manifest contains an empty thread ID")
		}
		wantPath := filepath.ToSlash(filepath.Join("threads", base64.RawURLEncoding.EncodeToString([]byte(thread.ThreadID))+".jsonl"))
		if thread.Path != wantPath {
			return VerifyResult{}, fmt.Errorf("thread %q has noncanonical path %q (want %q)", thread.ThreadID, thread.Path, wantPath)
		}
		if previous, ok := threadIDs[thread.ThreadID]; ok {
			return VerifyResult{}, fmt.Errorf("duplicate thread %q at %q and %q", thread.ThreadID, previous, thread.Path)
		}
		threadIDs[thread.ThreadID] = thread.Path
		if err := addExpected(expected, manifestFile{Path: thread.Path, Records: thread.Records, Bytes: thread.Bytes, SHA256: thread.SHA256}); err != nil {
			return VerifyResult{}, err
		}
	}

	seen := make(map[string]bool)
	if err := filepath.WalkDir(abs, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(abs, current)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive contains symlink %q", rel)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("archive path %q is accessible by group/others", rel)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("archive contains non-regular file %q", rel)
		}
		declared, ok := expected[rel]
		if !ok {
			return fmt.Errorf("unmanifested file %q", rel)
		}
		seen[rel] = true
		if rel == "manifest.json" {
			return nil
		}
		if info.Size() != declared.Bytes {
			return fmt.Errorf("size mismatch for %q: got %d want %d", rel, info.Size(), declared.Bytes)
		}
		_, checksum, err := checksumFile(current)
		if err != nil {
			return err
		}
		if checksum != declared.SHA256 {
			return fmt.Errorf("SHA-256 mismatch for %q", rel)
		}
		return nil
	}); err != nil {
		return VerifyResult{}, err
	}
	for rel := range expected {
		if !seen[rel] {
			return VerifyResult{}, fmt.Errorf("manifested file is missing: %q", rel)
		}
	}

	result := VerifyResult{Path: abs, Format: archiveManifest.Format, FormatVersion: archiveManifest.FormatVersion, Threads: len(archiveManifest.Threads)}
	mediaReferenced := make(map[string]bool)
	mediaDeclared := make(map[string]manifestFile)
	for _, file := range archiveManifest.Files {
		if strings.HasPrefix(file.Path, "media/") {
			if filepath.Base(file.Path) != file.SHA256 {
				return VerifyResult{}, fmt.Errorf("media path %q is not named by its SHA-256", file.Path)
			}
			mediaDeclared[file.Path] = file
			result.MediaFiles++
			result.MediaBytes += file.Bytes
		} else if strings.HasSuffix(file.Path, ".jsonl") {
			count, err := verifyGlobalJSONL(filepath.Join(abs, filepath.FromSlash(file.Path)), file.Path, mediaReferenced, mediaDeclared)
			if err != nil {
				return VerifyResult{}, err
			}
			if count != file.Records {
				return VerifyResult{}, fmt.Errorf("record count mismatch for %q: got %d want %d", file.Path, count, file.Records)
			}
			result.Records += count
		}
	}
	for _, thread := range archiveManifest.Threads {
		count, err := verifyThreadJSONL(filepath.Join(abs, filepath.FromSlash(thread.Path)), thread.ThreadID, mediaReferenced, mediaDeclared)
		if err != nil {
			return VerifyResult{}, err
		}
		if count != thread.Records {
			return VerifyResult{}, fmt.Errorf("record count mismatch for %q: got %d want %d", thread.Path, count, thread.Records)
		}
		result.Records += count
	}
	for path := range mediaDeclared {
		if !mediaReferenced[path] {
			return VerifyResult{}, fmt.Errorf("manifested media file is unreferenced: %q", path)
		}
	}
	return result, nil
}

func addExpected(expected map[string]manifestFile, entry manifestFile) error {
	if err := validateRelativePath(entry.Path); err != nil {
		return err
	}
	if _, exists := expected[entry.Path]; exists {
		return fmt.Errorf("duplicate manifest path %q", entry.Path)
	}
	if len(entry.SHA256) != 64 {
		return fmt.Errorf("invalid SHA-256 for %q", entry.Path)
	}
	expected[entry.Path] = entry
	return nil
}

func validateRelativePath(path string) error {
	if path == "" || filepath.IsAbs(path) || strings.Contains(path, "\\") || filepath.ToSlash(filepath.Clean(path)) != path || path == "." || strings.HasPrefix(path, "../") {
		return fmt.Errorf("unsafe manifest path %q", path)
	}
	return nil
}

func verifyGlobalJSONL(path, manifestPath string, mediaReferenced map[string]bool, mediaDeclared map[string]manifestFile) (int, error) {
	metadataSeen, summarySeen := false, false
	count, err := readJSONL(path, func(line int, raw []byte) error {
		var record envelope
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		switch manifestPath {
		case "metadata.jsonl":
			switch record.RecordType {
			case "metadata":
				metadataSeen = true
			case "summary":
				var summary struct {
					Complete bool `json:"complete"`
				}
				if err := json.Unmarshal(raw, &summary); err != nil || !summary.Complete {
					return errors.New("metadata summary is not complete")
				}
				summarySeen = true
			default:
				return fmt.Errorf("unexpected metadata record type %q", record.RecordType)
			}
		case "canonical-addresses.jsonl":
			if record.RecordType != "canonical_address" {
				return fmt.Errorf("unexpected canonical-address record type %q", record.RecordType)
			}
		default:
			return fmt.Errorf("unexpected global JSONL file %q", manifestPath)
		}
		return verifyMediaReference(record, raw, mediaReferenced, mediaDeclared)
	})
	if err != nil {
		return 0, err
	}
	if manifestPath == "metadata.jsonl" && (!metadataSeen || !summarySeen) {
		return 0, errors.New("metadata.jsonl lacks metadata or a complete summary")
	}
	return count, nil
}

func verifyThreadJSONL(path, threadID string, mediaReferenced map[string]bool, mediaDeclared map[string]manifestFile) (int, error) {
	mmsIDs := make(map[int64]bool)
	return readJSONL(path, func(line int, raw []byte) error {
		var record envelope
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		switch record.RecordType {
		case "sms", "mms", "thread":
			column := "thread_id"
			if record.RecordType == "thread" {
				column = "_id"
			}
			actual, err := integerValue(record.Values, column)
			if err != nil {
				return err
			}
			if actual != threadID {
				return fmt.Errorf("record belongs to thread %q, not %q", actual, threadID)
			}
			if record.RecordType == "mms" {
				id, err := int64Value(record.Values, "_id")
				if err != nil {
					return err
				}
				mmsIDs[id] = true
			}
		case "mms_address", "mms_part", "mms_part_data":
			if record.MMSID == nil || !mmsIDs[*record.MMSID] {
				return fmt.Errorf("%s references MMS not declared in this thread", record.RecordType)
			}
		default:
			return fmt.Errorf("unexpected record type %q in thread file", record.RecordType)
		}
		return verifyMediaReference(record, raw, mediaReferenced, mediaDeclared)
	})
}

func verifyMediaReference(record envelope, raw []byte, referenced map[string]bool, declared map[string]manifestFile) error {
	if record.RecordType != "mms_part_data" {
		return nil
	}
	var reference mediaReference
	if err := json.Unmarshal(raw, &reference); err != nil {
		return err
	}
	entry, ok := declared[reference.MediaPath]
	if !ok {
		return fmt.Errorf("MMS part references unmanifested media %q", reference.MediaPath)
	}
	if reference.SHA256 != entry.SHA256 || reference.ByteLength != entry.Bytes {
		return fmt.Errorf("MMS part reference metadata disagrees with %q", reference.MediaPath)
	}
	referenced[reference.MediaPath] = true
	return nil
}

func readJSONL(path string, visit func(line int, raw []byte) error) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<20)
	count := 0
	for {
		raw, readErr := reader.ReadBytes('\n')
		if len(raw) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		count++
		if len(strings.TrimSpace(string(raw))) == 0 || !json.Valid(raw) {
			return 0, fmt.Errorf("invalid JSONL in %q at line %d", path, count)
		}
		if err := visit(count, raw); err != nil {
			return 0, fmt.Errorf("verify %q line %d: %w", path, count, err)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return 0, readErr
		}
	}
	return count, nil
}

func int64Value(values map[string]taggedValue, column string) (int64, error) {
	text, err := integerValue(values, column)
	if err != nil {
		return 0, err
	}
	var value int64
	if _, err := fmt.Sscan(text, &value); err != nil {
		return 0, err
	}
	return value, nil
}
