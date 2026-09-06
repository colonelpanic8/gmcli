package gm

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/fdsouvenir/gmcli/internal/paths"
	"github.com/fdsouvenir/gmcli/internal/store"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

func TestDecodeFailureBecomesFatalEvent(t *testing.T) {
	var received any
	logger := zerolog.New(io.Discard).Hook(decodeFailureHook{notify: func(event any) { received = event }})
	logger.Error().Msg("Failed to decode incoming RPC message")
	if _, ok := received.(*events.ListenFatalError); !ok {
		t.Fatalf("got %T, want fatal event", received)
	}
}

func TestConnectRejectsChangedPairingBeforeNetwork(t *testing.T) {
	layout, err := paths.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	auth := libgm.NewAuthData()
	auth.Browser = &gmproto.Device{SourceID: "browser"}
	auth.Mobile = &gmproto.Device{SourceID: "same-google-account"}
	auth.PairingID = uuid.New()
	if err := saveAuth(layout.Session, auth); err != nil {
		t.Fatal(err)
	}
	c, err := Open(layout, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.bindSource(); err != nil {
		t.Fatal(err)
	}
	auth.PairingID = uuid.New()
	if err := saveAuth(layout.Session, auth); err != nil {
		t.Fatal(err)
	}
	c, err = Open(layout, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(); err == nil || !strings.Contains(err.Error(), "another pairing") {
		t.Fatalf("connect should fail locally: %v", err)
	}
	st, err := store.Open(context.Background(), layout.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n, err := st.CountMessages(context.Background()); err != nil || n != 0 {
		t.Fatalf("unexpected writes: %d, %v", n, err)
	}
}

func TestPairingFingerprintIgnoresTokenRotation(t *testing.T) {
	auth := libgm.NewAuthData()
	auth.Mobile = &gmproto.Device{SourceID: "phone"}
	auth.PairingID = uuid.New()
	want, err := pairingFingerprint(auth)
	if err != nil {
		t.Fatal(err)
	}
	auth.SetCookies(map[string]string{"SID": "rotated"})
	auth.TachyonAuthToken = []byte("rotated")
	got, err := pairingFingerprint(auth)
	if err != nil || got != want {
		t.Fatalf("token rotation changed source: %s, %v", got, err)
	}
	auth.Mobile.SourceID = "different-phone"
	got, err = pairingFingerprint(auth)
	if err != nil || got == want {
		t.Fatalf("phone change not detected: %s, %v", got, err)
	}
}
