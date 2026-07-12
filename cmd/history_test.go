package cmd

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

func TestHistoryBackfillResultJSONIsUnambiguous(t *testing.T) {
	res := historyBackfillResult{
		ConversationID:       "198",
		Requests:             2,
		Count:                100,
		FetchedMessages:      150,
		SyncRecordsProcessed: 150,
		MessagesBefore:       301,
		MessagesAfter:        325,
		MessagesAddedForChat: 24,
	}

	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(raw)
	for _, want := range []string{
		`"conversation_id":"198"`,
		`"fetched_messages":150`,
		`"sync_records_processed":150`,
		`"messages_before":301`,
		`"messages_after":325`,
		`"messages_added_for_chat":24`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("json missing %s: %s", want, out)
		}
	}
	if strings.Contains(out, `"imported"`) {
		t.Fatalf("json should not expose ambiguous imported field: %s", out)
	}
}

func TestHistoryPageBoundsOverlapCursorBoundary(t *testing.T) {
	messages := []*gmproto.Message{{Timestamp: 1_000}, {Timestamp: 900}, {Timestamp: 950}}
	start, end := historyPageBounds(messages, &gmproto.Cursor{LastItemTimestamp: 1_100})
	if start != 900 || end != 1_101 {
		t.Fatalf("bounds = [%d,%d), want [900,1101)", start, end)
	}
}

func TestTerminalSessionErrorsStopResumableBackfill(t *testing.T) {
	for _, message := range []string{
		"fetch messages: HTTP 401: invalid authentication credentials",
		"SESSION_COOKIE_INVALID",
		"session expired while polling",
	} {
		if !isTerminalSessionError(errors.New(message)) {
			t.Errorf("expected terminal session error for %q", message)
		}
	}
	for _, message := range []string{
		"fetch messages: context deadline exceeded",
		"temporary relay failure",
	} {
		if isTerminalSessionError(errors.New(message)) {
			t.Errorf("unexpected terminal session error for %q", message)
		}
	}
}
