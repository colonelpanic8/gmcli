package unifiedarchive

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fdsouvenir/gmcli/internal/archive"
	"github.com/fdsouvenir/gmcli/internal/store"
)

func TestOpenManyScopesUnknownRecipientsAndMergesKnownPeers(t *testing.T) {
	ctx := context.Background()
	var relays []string
	for _, phone := range []string{"phone-a", "phone-b"} {
		root := t.TempDir()
		st, err := store.Open(ctx, filepath.Join(root, "store.db"))
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"known", "unknown"} {
			participants := `[{"e164":"+12025550100","is_me":true}]`
			if id == "known" {
				participants = `[{"e164":"+12025550100","is_me":true},{"e164":"+12025550101"}]`
			}
			if err := st.UpsertConversation(ctx, store.Conversation{ID: id, ParticipantsJSON: participants}); err != nil {
				t.Fatal(err)
			}
			if err := st.UpsertMessage(ctx, store.Message{ID: id, ConversationID: id, TimestampMS: 1000, Body: &phone}); err != nil {
				t.Fatal(err)
			}
		}
		dir := filepath.Join(root, "archive")
		if _, err := archive.WriteJSONL(ctx, st, dir, false); err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		relays = append(relays, dir)
	}
	telephony := t.TempDir()
	if err := os.Chmod(telephony, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(telephony, "manifest.json"), []byte(`{"format":"gmcli-android-telephony","format_version":1,"files":[],"threads":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Repeating an archive path must not count its records twice.
	dataset, err := OpenMany(append(relays, relays[0]), []string{telephony, telephony})
	if err != nil {
		t.Fatal(err)
	}
	result := dataset.Result()
	if result.Conversations != 3 || result.Messages != 4 || result.RelaySourceMessages != 4 {
		t.Fatalf("result=%+v", result)
	}
	messages, ok := dataset.Messages("e164:+12025550101")
	if !ok || len(messages) != 2 {
		t.Fatalf("known-peer messages=%+v", messages)
	}
	if messages[0].UnifiedMessageID == messages[1].UnifiedMessageID {
		t.Fatal("raw message ID collided")
	}
}
