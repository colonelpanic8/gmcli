package store_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fdsouvenir/gmcli/internal/store"
)

func TestSourceBindingSurvivesReopenAndRejectsAnotherPairing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gmcli.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.BindSource(ctx, "phone-a"); err != nil {
		t.Fatal(err)
	}
	body := "original message"
	if err := st.UpsertMessage(ctx, store.Message{ID: "1", ConversationID: "1", Body: &body}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.BindSource(ctx, "phone-b"); err == nil || !strings.Contains(err.Error(), "another pairing") {
		t.Fatalf("binding mismatch: %v", err)
	}
	if err := st.BindSource(ctx, "phone-a"); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := st.DB().QueryRow(`SELECT body FROM messages WHERE message_id = '1'`).Scan(&got); err != nil || got != body {
		t.Fatalf("original changed: %q, %v", got, err)
	}
	other := openTempStore(t)
	if err := other.BindSource(ctx, "phone-b"); err != nil {
		t.Fatal(err)
	}
	if err := other.UpsertMessage(ctx, store.Message{ID: "1", ConversationID: "1"}); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyDataCannotBeAssignedToCurrentPhone(t *testing.T) {
	for _, insert := range []string{
		`INSERT INTO conversations (conversation_id, updated_at) VALUES ('1',0)`,
		`INSERT INTO contacts (participant_id, updated_at) VALUES ('1',0)`,
		`INSERT INTO folder_coverage (folder,status,updated_at) VALUES ('INBOX','complete',0)`,
	} {
		t.Run(insert, func(t *testing.T) {
			st := openTempStore(t)
			if _, err := st.DB().Exec(insert); err != nil {
				t.Fatal(err)
			}
			if err := st.BindSource(context.Background(), "phone-b"); err == nil || !strings.Contains(err.Error(), "legacy database") {
				t.Fatalf("legacy binding: %v", err)
			}
			var n int
			if err := st.DB().QueryRow(`SELECT count(*) FROM source_identity`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("bound legacy database: %d, %v", n, err)
			}
		})
	}
}

func TestMessageIDReuseCannotOverwriteContent(t *testing.T) {
	ctx := context.Background()
	st := openTempStore(t)
	body := "preserved"
	original := store.Message{ID: "1", ConversationID: "a", TimestampMS: 1000, Body: &body}
	if err := st.UpsertMessage(ctx, original); err != nil {
		t.Fatal(err)
	}
	for _, change := range []store.Message{
		{ID: "1", ConversationID: "b", TimestampMS: 1000},
		{ID: "1", ConversationID: "a", TimestampMS: 2000},
	} {
		if err := st.UpsertMessage(ctx, change); err == nil || !strings.Contains(err.Error(), "reused") {
			t.Fatalf("reuse not rejected: %v", err)
		}
	}
	var got string
	if err := st.DB().QueryRow(`SELECT body FROM messages WHERE message_id = '1'`).Scan(&got); err != nil || got != body {
		t.Fatalf("lost body: %q, %v", got, err)
	}
	original.Status = 2
	if err := st.UpsertMessage(ctx, original); err != nil {
		t.Fatalf("legitimate status update: %v", err)
	}
}
