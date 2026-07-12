package gm

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	gmcrypto "go.mau.fi/mautrix-gmessages/pkg/libgm/crypto"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"

	"github.com/fdsouvenir/gmcli/internal/paths"
)

func TestRefreshSessionFromWebStorageValidatesBeforeSaving(t *testing.T) {
	layout, values := webStorageFixture(t)
	now := time.Now().UTC()
	validated := false
	err := refreshSessionFromWebStorage(layout, values, "g_", func(candidate *libgm.AuthData) error {
		validated = true
		if candidate.TachyonExpiry.After(now.Add(-30 * time.Minute)) {
			t.Fatalf("candidate token was not forced due before validation: %s", candidate.TachyonExpiry)
		}
		candidate.TachyonAuthToken = []byte("server-refreshed-token")
		candidate.TachyonTTL = (24 * time.Hour).Microseconds()
		candidate.TachyonExpiry = now.Add(24 * time.Hour)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !validated {
		t.Fatal("candidate was not validated")
	}
	got, err := loadAuth(layout.Session)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.TachyonAuthToken) != "server-refreshed-token" || got.PairingID.String() != values["bg_tachyon_pairing_attempt_id"] {
		t.Fatal("validated token or pairing id was not saved")
	}
	if len(got.RequestCrypto.AESKey) != 32 || got.RequestCrypto.HMACKey[0] != 1 {
		t.Fatal("request keys were not refreshed")
	}
	info, err := os.Stat(layout.Session)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("session permissions = %04o, want 0600", info.Mode().Perm())
	}
}

func TestRefreshSessionValidationFailurePreservesOriginal(t *testing.T) {
	layout, values := webStorageFixture(t)
	original, err := os.ReadFile(layout.Session)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("tachyon rejected candidate")
	err = refreshSessionFromWebStorage(layout, values, "g_", func(candidate *libgm.AuthData) error {
		// Mutate nested data to prove the candidate does not alias the loaded
		// session even when validation fails.
		candidate.Browser.SourceID = "mutated"
		candidate.Cookies["SID"] = "mutated"
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected validation failure, got %v", err)
	}
	after, err := os.ReadFile(layout.Session)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("failed candidate changed the durable session")
	}
}

func TestRefreshSessionRejectsDifferentBrowserWithoutValidation(t *testing.T) {
	layout, values := webStorageFixture(t)
	otherDevice, err := proto.Marshal(&gmproto.SignInGaiaRequest_Inner_DeviceID{UnknownInt1: 3, DeviceID: "messages-web-someone-else"})
	if err != nil {
		t.Fatal(err)
	}
	values["bg_tachyon_auth_device_id"] = base64.StdEncoding.EncodeToString(otherDevice)
	validated := false
	err = refreshSessionFromWebStorage(layout, values, "g_", func(*libgm.AuthData) error {
		validated = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "different Messages browser device") {
		t.Fatalf("expected device mismatch, got %v", err)
	}
	if validated {
		t.Fatal("invalid browser storage reached network validation")
	}
}

func TestApplyWebStorageSessionRejectsMismatchedCurveKeys(t *testing.T) {
	layout, values := webStorageFixture(t)
	existing, err := loadAuth(layout.Session)
	if err != nil {
		t.Fatal(err)
	}
	other := gmcrypto.GenerateECDSAKey()
	values["crypto_pub_key"] = base64.StdEncoding.EncodeToString(uncompressedPublicKey(other))
	_, err = applyWebStorageSession(existing, values, "g_", time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected public/private mismatch, got %v", err)
	}
}

func TestRefreshSessionRejectsValidatorThatDoesNotRefresh(t *testing.T) {
	layout, values := webStorageFixture(t)
	original, err := os.ReadFile(layout.Session)
	if err != nil {
		t.Fatal(err)
	}
	err = refreshSessionFromWebStorage(layout, values, "g_", func(*libgm.AuthData) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "did not return a usable token") {
		t.Fatalf("expected unusable-token error, got %v", err)
	}
	after, err := os.ReadFile(layout.Session)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("unrefreshed candidate changed the durable session")
	}
}

func webStorageFixture(t *testing.T) (paths.Layout, map[string]string) {
	t.Helper()
	sessionID := uuid.MustParse("4f442f07-297d-217d-eb4f-97d489c8a3da")
	destID := uuid.MustParse("0a8e910f-2613-445c-97d0-02195676a6f4")
	layout := paths.Layout{Session: filepath.Join(t.TempDir(), "session.json")}
	auth := libgm.NewAuthData()
	auth.SessionID = sessionID
	auth.DestRegID = destID
	auth.Browser = &gmproto.Device{UserID: 16, SourceID: "IvanMalison", Network: "GDitto"}
	auth.Mobile = &gmproto.Device{UserID: 16, SourceID: "ivanmalison", Network: "GDitto"}
	auth.Cookies = map[string]string{"SID": "original"}
	if err := saveAuth(layout.Session, auth); err != nil {
		t.Fatal(err)
	}
	refresh := gmcrypto.GenerateECDSAKey()
	b64 := base64.StdEncoding.EncodeToString
	deviceData, err := proto.Marshal(&gmproto.SignInGaiaRequest_Inner_DeviceID{
		UnknownInt1: 3,
		DeviceID:    "messages-web-" + strings.ReplaceAll(sessionID.String(), "-", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	return layout, map[string]string{
		"bg_tachyon_auth_device_id":       b64(deviceData),
		"bg_tachyon_auth_token":           b64([]byte("browser-token")),
		"bg_tachyon_dest_registration_id": destID.String(),
		"bg_tachyon_pairing_attempt_id":   "d735a87f-adfd-46ae-8299-6ae31659fe2b",
		"g_crypto_msg_enc_key":            b64(make([]byte, 32)),
		"g_crypto_msg_hmac":               b64(bytesOf(1, 32)),
		"crypto_priv_key":                 b64(pad32(refresh.D)),
		"crypto_pub_key":                  b64(uncompressedPublicKey(refresh)),
	}
}

func uncompressedPublicKey(key *gmcrypto.JWK) []byte {
	public := append([]byte{4}, pad32(key.X)...)
	return append(public, pad32(key.Y)...)
}

func pad32(value []byte) []byte {
	out := make([]byte, 32)
	copy(out[32-len(value):], value)
	return out
}

func bytesOf(value byte, count int) []byte {
	out := make([]byte, count)
	for i := range out {
		out[i] = value
	}
	return out
}
