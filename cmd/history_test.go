package cmd

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/fdsouvenir/gmcli/internal/store"
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

func TestHistoryBackfillAllResultJSONUsesKeyCursor(t *testing.T) {
	raw, err := json.Marshal(historyBackfillAllResult{
		AfterConversationID: "b", NextAfterConversationID: "c", Selected: 1, Attempted: 1,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(raw)
	if !strings.Contains(out, `"after_conversation_id":"b"`) || !strings.Contains(out, `"next_after_conversation_id":"c"`) {
		t.Fatalf("JSON lacks stable resume cursor: %s", out)
	}
	if strings.Contains(out, `"offset"`) || strings.Contains(out, `"next_offset"`) {
		t.Fatalf("JSON exposes misleading zero legacy offsets: %s", out)
	}
}

func TestSelectHistoryBackfillConversationsUsesStableKeysetAfterFiltering(t *testing.T) {
	conversations := []store.Conversation{{ID: "e"}, {ID: "b"}, {ID: "c"}, {ID: "a"}, {ID: "d"}}
	exhausted := map[string]struct{}{"b": {}, "d": {}}

	selected, eligible, skipped, err := selectHistoryBackfillConversations(conversations, exhausted, false, "b", 1)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if eligible != 3 || skipped != 2 {
		t.Fatalf("metrics = eligible %d, skipped %d; want 3, 2", eligible, skipped)
	}
	if len(selected) != 1 || selected[0].ID != "c" {
		t.Fatalf("selected = %+v, want conversation c", selected)
	}
}

func TestSelectHistoryBackfillConversationsCanRecheckExhausted(t *testing.T) {
	conversations := []store.Conversation{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	exhausted := map[string]struct{}{"b": {}}

	selected, eligible, skipped, err := selectHistoryBackfillConversations(conversations, exhausted, true, "a", 0)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if eligible != 3 || skipped != 0 {
		t.Fatalf("metrics = eligible %d, skipped %d; want 3, 0", eligible, skipped)
	}
	if len(selected) != 2 || selected[0].ID != "b" || selected[1].ID != "c" {
		t.Fatalf("selected = %+v, want conversations b and c", selected)
	}
}

func TestHistoryBackfillResumeRejectsLegacyAndStaleCursors(t *testing.T) {
	conversations := []store.Conversation{{ID: "a"}}
	if err := validateHistoryBackfillResume(1, ""); err == nil || !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("nonzero legacy offset error = %v", err)
	}
	if err := validateHistoryBackfillResume(1, "a"); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("combined resume controls error = %v", err)
	}
	if _, _, _, err := selectHistoryBackfillConversations(conversations, nil, false, "deleted", 0); err == nil || !strings.Contains(err.Error(), "stale cursor") {
		t.Fatalf("stale key cursor error = %v", err)
	}
}

func TestHistoryBackfillKeysetResumeSurvivesReorderingAndNewConversations(t *testing.T) {
	firstPass, _, _, err := selectHistoryBackfillConversations(
		[]store.Conversation{{ID: "c", LastMessageTimeMS: 300}, {ID: "a", LastMessageTimeMS: 100}, {ID: "b", LastMessageTimeMS: 200}},
		nil, false, "", 2,
	)
	if err != nil {
		t.Fatalf("first selection: %v", err)
	}
	if got := conversationIDs(firstPass); strings.Join(got, ",") != "a,b" {
		t.Fatalf("first selection = %v, want [a b]", got)
	}
	next := ""
	next = nextHistoryBackfillCursor(next, "a", historyBackfillOutcomeExhausted)
	next = nextHistoryBackfillCursor(next, "b", historyBackfillOutcomePartial)

	// Timestamp ordering changed, and conversations were inserted on both sides
	// of the cursor. Existing and new higher IDs remain in this pass; a new
	// lower ID is visible in the next full pass and therefore cannot make the
	// coverage verification gate pass unnoticed.
	changed := []store.Conversation{
		{ID: "d", LastMessageTimeMS: 1}, {ID: "aa", LastMessageTimeMS: 999},
		{ID: "c", LastMessageTimeMS: 2}, {ID: "a", LastMessageTimeMS: 1000},
		{ID: "bb", LastMessageTimeMS: 3}, {ID: "b", LastMessageTimeMS: 4},
	}
	resumed, _, _, err := selectHistoryBackfillConversations(changed, map[string]struct{}{"a": {}}, false, next, 0)
	if err != nil {
		t.Fatalf("resumed selection: %v", err)
	}
	if got := strings.Join(conversationIDs(resumed), ","); got != "bb,c,d" {
		t.Fatalf("resumed selection = %s, want bb,c,d", got)
	}
	fullSweep, _, _, err := selectHistoryBackfillConversations(changed, map[string]struct{}{"a": {}}, false, "", 0)
	if err != nil {
		t.Fatalf("full sweep selection: %v", err)
	}
	if got := strings.Join(conversationIDs(fullSweep), ","); got != "aa,b,bb,c,d" {
		t.Fatalf("new full sweep = %s, want aa,b,bb,c,d", got)
	}
}

func TestHistoryBackfillCursorMixedOutcomesRetriesTerminalFailure(t *testing.T) {
	conversations := []store.Conversation{{ID: "d"}, {ID: "b"}, {ID: "a"}, {ID: "c"}}
	selected, _, _, err := selectHistoryBackfillConversations(conversations, nil, false, "", 0)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	next := ""
	next = nextHistoryBackfillCursor(next, selected[0].ID, historyBackfillOutcomeExhausted)
	next = nextHistoryBackfillCursor(next, selected[1].ID, historyBackfillOutcomeFailed)
	next = nextHistoryBackfillCursor(next, selected[2].ID, historyBackfillOutcomePartial)
	next = nextHistoryBackfillCursor(next, selected[3].ID, historyBackfillOutcomeTerminalFailure)
	if next != "c" {
		t.Fatalf("next cursor = %q, want c so terminal failure d is retried", next)
	}
	resumed, _, _, err := selectHistoryBackfillConversations(conversations, nil, false, next, 0)
	if err != nil {
		t.Fatalf("resume selection: %v", err)
	}
	if got := conversationIDs(resumed); len(got) != 1 || got[0] != "d" {
		t.Fatalf("resume selection = %v, want [d]", got)
	}
}

func conversationIDs(conversations []store.Conversation) []string {
	ids := make([]string, len(conversations))
	for i, conversation := range conversations {
		ids[i] = conversation.ID
	}
	return ids
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
