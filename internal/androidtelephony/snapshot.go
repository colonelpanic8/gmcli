package androidtelephony

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var hardwareSerialPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Snapshot retains a new verified export under a stable hardware identity.
func Snapshot(ctx context.Context, options Options, root string) (Result, error) {
	if root == "" || options.OutputDirectory != "" || options.Force {
		return Result{}, errors.New("--snapshot-root requires a root and cannot be combined with --out or --force")
	}
	if options.ADB == "" {
		options.ADB = "adb"
	}
	serial, err := resolveDevice(ctx, options.ADB, options.Serial)
	if err != nil {
		return Result{}, err
	}
	output, err := exec.CommandContext(ctx, options.ADB, adbArgs(serial, "shell", "getprop", "ro.serialno")...).Output()
	if err != nil {
		return Result{}, fmt.Errorf("read hardware serial: %w", err)
	}
	hardware := strings.TrimSpace(string(output))
	if !hardwareSerialPattern.MatchString(hardware) || strings.EqualFold(hardware, "unknown") {
		return Result{}, errors.New("phone did not report a usable hardware serial; use --out with an explicit device-specific destination")
	}
	kind := "metadata"
	if options.IncludePartData {
		kind = "full"
	}
	options.Serial = serial
	options.hardwareSerial = hardware
	options.OutputDirectory = filepath.Join(root, hardware, time.Now().UTC().Format("20060102T150405.000000000Z")+"-"+kind)
	return Export(ctx, options)
}
