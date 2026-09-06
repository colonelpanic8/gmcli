package androidtelephony

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotsKeepSeparatePhonesAndRepeatedRuns(t *testing.T) {
	root := t.TempDir()
	var outputs []Result
	for _, hardware := range []string{"PHONE_A", "PHONE_B", "PHONE_A"} {
		adb := filepath.Join(t.TempDir(), "adb")
		script := fmt.Sprintf(`#!/bin/sh
case "$3" in
get-state) echo device ;;
shell) if [ "$4" = getprop ]; then echo %s; fi ;;
push) exit 0 ;;
exec-out)
printf '%%s\n' '{"record_type":"metadata","format":"gmcli-android-telephony","format_version":1,"device_serial":"192.0.2.1:5555"}' '{"record_type":"sms","values":{"_id":{"type":"integer","value":1},"thread_id":{"type":"integer","value":1}}}' '{"record_type":"summary","complete":true,"counts":{"sms":1}}'
;;
*) exit 1 ;;
esac
`, hardware)
		if err := os.WriteFile(adb, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		result, err := Snapshot(context.Background(), Options{ADB: adb, Serial: "192.0.2.1:5555", IncludePartData: true}, root)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(filepath.Dir(result.Path)) != hardware || result.HardwareSerial != hardware {
			t.Fatalf("wrong identity: %+v", result)
		}
		for _, previous := range outputs {
			if previous.Path == result.Path {
				t.Fatal("snapshot replaced previous output")
			}
		}
		outputs = append(outputs, result)
	}
	for _, result := range outputs {
		if _, err := Verify(result.Path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSnapshotRejectsAmbiguousDestination(t *testing.T) {
	_, err := Snapshot(context.Background(), Options{Force: true}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("got %v", err)
	}
}
