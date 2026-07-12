// Package androidtelephony exports the Android Telephony provider over adb.
package androidtelephony

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed helper.dex
var helperDEX []byte

// Options configures an Android Telephony-provider export.
type Options struct {
	ADB             string
	Serial          string
	OutputDirectory string
	Force           bool
	IncludePartData bool
}

// Result describes an installed, checksummed Telephony export.
type Result struct {
	Path          string `json:"path"`
	Threads       int    `json:"threads"`
	Records       int    `json:"records"`
	MediaFiles    int    `json:"media_files"`
	MediaBytes    int64  `json:"media_bytes"`
	DeviceSerial  string `json:"device_serial"`
	FormatVersion int    `json:"format_version"`
}

type manifest struct {
	Format        string         `json:"format"`
	FormatVersion int            `json:"format_version"`
	DeviceSerial  string         `json:"device_serial"`
	Files         []manifestFile `json:"files"`
	Threads       []threadFile   `json:"threads"`
}

type manifestFile struct {
	Path    string `json:"path"`
	Records int    `json:"records,omitempty"`
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256"`
}

type threadFile struct {
	ThreadID string `json:"thread_id"`
	Path     string `json:"path"`
	Records  int    `json:"records"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
}

type taggedValue struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

type envelope struct {
	RecordType string                 `json:"record_type"`
	Device     string                 `json:"device_serial,omitempty"`
	MMSID      *int64                 `json:"mms_id,omitempty"`
	PartID     *int64                 `json:"part_id,omitempty"`
	Data       string                 `json:"data,omitempty"`
	ByteLength *int64                 `json:"byte_length,omitempty"`
	SHA256     string                 `json:"sha256,omitempty"`
	Values     map[string]taggedValue `json:"values,omitempty"`
}

type mediaReference struct {
	RecordType string `json:"record_type"`
	SourceURI  string `json:"source_uri"`
	PartID     int64  `json:"part_id"`
	MMSID      int64  `json:"mms_id"`
	MediaPath  string `json:"media_path"`
	ByteLength int64  `json:"byte_length"`
	SHA256     string `json:"sha256"`
}

// Export runs the bundled read-only helper and atomically installs a segmented archive.
func Export(ctx context.Context, options Options) (Result, error) {
	if options.OutputDirectory == "" {
		return Result{}, errors.New("output directory is required")
	}
	if options.ADB == "" {
		options.ADB = "adb"
	}
	abs, err := filepath.Abs(options.OutputDirectory)
	if err != nil {
		return Result{}, fmt.Errorf("resolve output directory: %w", err)
	}
	if !options.Force {
		if _, err := os.Stat(abs); err == nil {
			return Result{}, fmt.Errorf("output already exists: %s (use --force to replace it)", abs)
		} else if !os.IsNotExist(err) {
			return Result{}, fmt.Errorf("inspect output: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return Result{}, fmt.Errorf("create output parent: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(abs), ".gmcli-telephony-*")
	if err != nil {
		return Result{}, fmt.Errorf("create temporary export: %w", err)
	}
	installed := false
	defer func() {
		if !installed {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := os.Chmod(tmp, 0o700); err != nil {
		return Result{}, err
	}

	serial, err := resolveDevice(ctx, options.ADB, options.Serial)
	if err != nil {
		return Result{}, err
	}
	var result Result
	var archiveManifest manifest
	err = runHelper(ctx, options.ADB, serial, options.IncludePartData, func(raw io.Reader) error {
		var segmentErr error
		result, archiveManifest, segmentErr = segmentRaw(raw, tmp, serial)
		return segmentErr
	})
	if err != nil {
		return Result{}, err
	}
	if err := writeManifest(filepath.Join(tmp, "manifest.json"), archiveManifest); err != nil {
		return Result{}, err
	}
	if err := installDirectory(tmp, abs, options.Force); err != nil {
		return Result{}, err
	}
	installed = true
	if _, err := Verify(abs); err != nil {
		return Result{}, fmt.Errorf("post-install Telephony archive verification failed: %w", err)
	}
	result.Path = abs
	return result, nil
}

func adbArgs(serial string, args ...string) []string {
	if serial == "" {
		return args
	}
	return append([]string{"-s", serial}, args...)
}

func resolveDevice(ctx context.Context, adb, requested string) (string, error) {
	if requested != "" {
		cmd := exec.CommandContext(ctx, adb, adbArgs(requested, "get-state")...)
		if output, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "device" {
			return "", fmt.Errorf("adb device %q is unavailable: %s", requested, strings.TrimSpace(string(output)))
		}
		return requested, nil
	}
	cmd := exec.CommandContext(ctx, adb, "devices")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("list adb devices: %w", err)
	}
	var devices []string
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "device" {
			devices = append(devices, fields[0])
		}
	}
	if len(devices) != 1 {
		return "", fmt.Errorf("expected exactly one connected adb device, found %d (use --serial)", len(devices))
	}
	return devices[0], nil
}

func runHelper(ctx context.Context, adb, serial string, includePartData bool, consume func(io.Reader) error) error {
	digest := sha256.Sum256(helperDEX)
	remote := fmt.Sprintf("/data/local/tmp/gmcli-telephony-%d-%s.dex", os.Getpid(), hex.EncodeToString(digest[:6]))
	local, err := os.CreateTemp("", "gmcli-telephony-*.dex")
	if err != nil {
		return err
	}
	localPath := local.Name()
	defer os.Remove(localPath)
	if err := local.Chmod(0o600); err != nil {
		local.Close()
		return err
	}
	if _, err := local.Write(helperDEX); err != nil {
		local.Close()
		return err
	}
	if err := local.Close(); err != nil {
		return err
	}
	push := exec.CommandContext(ctx, adb, adbArgs(serial, "push", localPath, remote)...)
	if output, err := push.CombinedOutput(); err != nil {
		return fmt.Errorf("push Android helper: %w: %s", err, strings.TrimSpace(string(output)))
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_ = exec.CommandContext(cleanupCtx, adb, adbArgs(serial, "shell", "rm", "-f", remote)...).Run()
	}()

	args := adbArgs(serial, "exec-out", "env", "CLASSPATH="+remote, "app_process", "/system/bin", "com.gmcli.TelephonyExport", "--device-serial", serial)
	if !includePartData {
		args = append(args, "--no-part-data")
	}
	cmd := exec.CommandContext(ctx, adb, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{dst: &stderr, remaining: 1 << 20}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Android Telephony helper: %w", err)
	}
	consumeErr := consume(stdout)
	if consumeErr != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	runErr := cmd.Wait()
	if consumeErr != nil {
		return consumeErr
	}
	if runErr != nil {
		return fmt.Errorf("export Android Telephony provider: %w: %s", runErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

const cleanupTimeout = 10 * time.Second

type limitedWriter struct {
	dst       io.Writer
	remaining int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	original := len(p)
	if len(p) > w.remaining {
		p = p[:w.remaining]
	}
	if len(p) > 0 {
		_, _ = w.dst.Write(p)
		w.remaining -= len(p)
	}
	return original, nil
}

type outputFile struct {
	path    string
	file    *os.File
	records int
}

func segmentRaw(raw io.Reader, dir, serial string) (Result, manifest, error) {
	threadsDir := filepath.Join(dir, "threads")
	mediaDir := filepath.Join(dir, "media")
	if err := os.MkdirAll(threadsDir, 0o700); err != nil {
		return Result{}, manifest{}, err
	}
	if err := os.MkdirAll(mediaDir, 0o700); err != nil {
		return Result{}, manifest{}, err
	}
	files := make(map[string]*outputFile)
	defer func() {
		for _, output := range files {
			_ = output.file.Close()
		}
	}()
	mmsThreads := make(map[int64]string)
	result := Result{DeviceSerial: serial, FormatVersion: 1}
	complete := false
	reader := bufio.NewReaderSize(raw, 1<<20)
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := reader.ReadBytes('\n')
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return Result{}, manifest{}, fmt.Errorf("read helper output: %w", readErr)
		}
		var record envelope
		if err := json.Unmarshal(line, &record); err != nil {
			return Result{}, manifest{}, fmt.Errorf("invalid helper JSONL at line %d: %w", lineNumber, err)
		}
		var threadID string
		var err error
		switch record.RecordType {
		case "sms", "mms", "thread":
			column := "thread_id"
			if record.RecordType == "thread" {
				column = "_id"
			}
			threadID, err = integerValue(record.Values, column)
			if err != nil {
				return Result{}, manifest{}, fmt.Errorf("line %d: %w", lineNumber, err)
			}
			if record.RecordType == "mms" {
				mmsID, idErr := integerValue(record.Values, "_id")
				if idErr != nil {
					return Result{}, manifest{}, idErr
				}
				var numeric int64
				if _, err := fmt.Sscan(mmsID, &numeric); err != nil {
					return Result{}, manifest{}, err
				}
				mmsThreads[numeric] = threadID
			}
		case "mms_address", "mms_part", "mms_part_data":
			if record.MMSID == nil {
				return Result{}, manifest{}, fmt.Errorf("line %d: %s lacks mms_id", lineNumber, record.RecordType)
			}
			threadID = mmsThreads[*record.MMSID]
			if threadID == "" {
				return Result{}, manifest{}, fmt.Errorf("line %d: MMS %d has no thread mapping", lineNumber, *record.MMSID)
			}
		case "summary":
			var summary struct {
				Complete bool `json:"complete"`
			}
			if err := json.Unmarshal(line, &summary); err != nil || !summary.Complete {
				return Result{}, manifest{}, fmt.Errorf("helper did not report a complete export")
			}
			complete = true
		}
		if record.RecordType == "mms_part_data" {
			if record.PartID == nil || record.MMSID == nil || record.ByteLength == nil {
				return Result{}, manifest{}, fmt.Errorf("line %d: incomplete MMS part data record", lineNumber)
			}
			decoded, err := base64.StdEncoding.DecodeString(record.Data)
			if err != nil {
				return Result{}, manifest{}, fmt.Errorf("decode MMS part %d: %w", *record.PartID, err)
			}
			sum := sha256.Sum256(decoded)
			actualHash := hex.EncodeToString(sum[:])
			if int64(len(decoded)) != *record.ByteLength || actualHash != record.SHA256 {
				return Result{}, manifest{}, fmt.Errorf("MMS part %d failed length/SHA-256 verification", *record.PartID)
			}
			mediaPath := filepath.ToSlash(filepath.Join("media", actualHash))
			fullMediaPath := filepath.Join(dir, filepath.FromSlash(mediaPath))
			if _, err := os.Stat(fullMediaPath); os.IsNotExist(err) {
				if err := os.WriteFile(fullMediaPath, decoded, 0o600); err != nil {
					return Result{}, manifest{}, err
				}
				result.MediaFiles++
				result.MediaBytes += int64(len(decoded))
			}
			var rawRecord map[string]json.RawMessage
			_ = json.Unmarshal(line, &rawRecord)
			var sourceURI string
			_ = json.Unmarshal(rawRecord["source_uri"], &sourceURI)
			line, err = json.Marshal(mediaReference{RecordType: record.RecordType, SourceURI: sourceURI, PartID: *record.PartID, MMSID: *record.MMSID, MediaPath: mediaPath, ByteLength: *record.ByteLength, SHA256: record.SHA256})
			if err != nil {
				return Result{}, manifest{}, err
			}
			line = append(line, '\n')
		}
		key := "metadata.jsonl"
		if threadID != "" {
			key = filepath.ToSlash(filepath.Join("threads", base64.RawURLEncoding.EncodeToString([]byte(threadID))+".jsonl"))
		} else if record.RecordType == "canonical_address" {
			key = "canonical-addresses.jsonl"
		}
		output := files[key]
		if output == nil {
			file, err := os.OpenFile(filepath.Join(dir, filepath.FromSlash(key)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return Result{}, manifest{}, err
			}
			output = &outputFile{path: key, file: file}
			files[key] = output
		}
		if _, err := output.file.Write(line); err != nil {
			return Result{}, manifest{}, err
		}
		output.records++
		result.Records++
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if !complete {
		return Result{}, manifest{}, errors.New("helper output is incomplete (missing successful summary)")
	}
	for _, output := range files {
		if err := output.file.Sync(); err != nil {
			return Result{}, manifest{}, err
		}
		if err := output.file.Close(); err != nil {
			return Result{}, manifest{}, err
		}
	}
	archiveManifest := manifest{Format: "gmcli-android-telephony", FormatVersion: 1, DeviceSerial: serial}
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		info, checksum, err := checksumFile(filepath.Join(dir, filepath.FromSlash(key)))
		if err != nil {
			return Result{}, manifest{}, err
		}
		entry := manifestFile{Path: key, Records: files[key].records, Bytes: info.Size(), SHA256: checksum}
		if strings.HasPrefix(key, "threads/") {
			encoded := strings.TrimSuffix(filepath.Base(key), ".jsonl")
			decoded, err := base64.RawURLEncoding.DecodeString(encoded)
			if err != nil {
				return Result{}, manifest{}, err
			}
			archiveManifest.Threads = append(archiveManifest.Threads, threadFile{ThreadID: string(decoded), Path: key, Records: entry.Records, Bytes: entry.Bytes, SHA256: entry.SHA256})
		} else {
			archiveManifest.Files = append(archiveManifest.Files, entry)
		}
	}
	mediaEntries, err := os.ReadDir(mediaDir)
	if err != nil {
		return Result{}, manifest{}, err
	}
	for _, entry := range mediaEntries {
		path := filepath.Join(mediaDir, entry.Name())
		info, checksum, err := checksumFile(path)
		if err != nil {
			return Result{}, manifest{}, err
		}
		archiveManifest.Files = append(archiveManifest.Files, manifestFile{Path: filepath.ToSlash(filepath.Join("media", entry.Name())), Bytes: info.Size(), SHA256: checksum})
	}
	result.Threads = len(archiveManifest.Threads)
	return result, archiveManifest, nil
}

func integerValue(values map[string]taggedValue, column string) (string, error) {
	value, ok := values[column]
	if !ok || value.Type != "integer" {
		return "", fmt.Errorf("missing integer column %q", column)
	}
	var number json.Number
	if err := json.Unmarshal(value.Value, &number); err != nil {
		return "", fmt.Errorf("decode %s: %w", column, err)
	}
	return number.String(), nil
}

func checksumFile(path string) (os.FileInfo, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return nil, "", err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	return info, hex.EncodeToString(hash.Sum(nil)), nil
}

func writeManifest(path string, value manifest) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func installDirectory(tmp, destination string, force bool) error {
	if !force {
		return os.Rename(tmp, destination)
	}
	backup := destination + ".old"
	_ = os.RemoveAll(backup)
	if err := os.Rename(destination, backup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("preserve existing export: %w", err)
	}
	if err := os.Rename(tmp, destination); err != nil {
		_ = os.Rename(backup, destination)
		return fmt.Errorf("install export: %w", err)
	}
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("remove replaced export: %w", err)
	}
	return nil
}
