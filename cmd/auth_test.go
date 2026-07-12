package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadGoogleCookiesFromStdin(t *testing.T) {
	input := `{"SID":"sid","HSID":"hsid","OSID":"osid","SSID":"ssid","APISID":"apisid","SAPISID":"sapisid","optional":"kept"}`
	cookies, err := readGoogleCookies("-", strings.NewReader(input))
	if err != nil {
		t.Fatalf("read cookies: %v", err)
	}
	if cookies["SAPISID"] != "sapisid" || cookies["optional"] != "kept" {
		t.Fatalf("unexpected cookies: %#v", cookies)
	}
}

func TestReadGoogleCookiesFromExportArray(t *testing.T) {
	input := `[{"name":"SID","value":"sid"},{"name":"HSID","value":"hsid"},{"name":"OSID","value":"osid"},{"name":"SSID","value":"ssid"},{"name":"APISID","value":"apisid"},{"name":"SAPISID","value":"sapisid"}]`
	cookies, err := readGoogleCookies("-", strings.NewReader(input))
	if err != nil {
		t.Fatalf("read cookies: %v", err)
	}
	if cookies["SID"] != "sid" || cookies["SAPISID"] != "sapisid" {
		t.Fatalf("unexpected cookies: %#v", cookies)
	}
}

func TestReadGoogleCookiesRejectsPermissiveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.json")
	input := `{"SID":"sid","HSID":"hsid","OSID":"osid","SSID":"ssid","APISID":"apisid","SAPISID":"sapisid"}`
	if err := os.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatalf("write cookies: %v", err)
	}
	if _, err := readGoogleCookies(path, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("expected permissions error, got %v", err)
	}
}

func TestReadGoogleCookiesRejectsMissingRequiredCookies(t *testing.T) {
	_, err := readGoogleCookies("-", strings.NewReader(`{"SID":"sid"}`))
	if err == nil || !strings.Contains(err.Error(), "SAPISID") {
		t.Fatalf("expected missing-cookie error, got %v", err)
	}
}

func TestReadGoogleCookiesRequiresSource(t *testing.T) {
	_, err := readGoogleCookies("", strings.NewReader("{}"))
	if err == nil || !strings.Contains(err.Error(), "--cookies-file") {
		t.Fatalf("expected source error, got %v", err)
	}
}

func TestReadPrivateStringMapFromStdin(t *testing.T) {
	values, err := readPrivateStringMap("-", strings.NewReader(`{"token":"secret"}`))
	if err != nil {
		t.Fatalf("read private map: %v", err)
	}
	if values["token"] != "secret" {
		t.Fatalf("unexpected private map: %#v", values)
	}
}

func TestReadPrivateStringMapRejectsPermissiveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "storage.json")
	if err := os.WriteFile(path, []byte(`{"token":"secret"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateStringMap(path, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("expected permissions error, got %v", err)
	}
}

func TestReadPrivateStringMapRejectsTrailingJSON(t *testing.T) {
	_, err := readPrivateStringMap("-", strings.NewReader(`{"token":"secret"} {"other":"value"}`))
	if err == nil || !strings.Contains(err.Error(), "decode private JSON object") {
		t.Fatalf("expected trailing JSON error, got %v", err)
	}
}

func TestReadPrivateStringMapRejectsEmptyObject(t *testing.T) {
	_, err := readPrivateStringMap("-", strings.NewReader(`{}`))
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty object error, got %v", err)
	}
}
