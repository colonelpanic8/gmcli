package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fdsouvenir/gmcli/internal/store"
)

func TestNormalizeRequiredCoverageFolders(t *testing.T) {
	got, err := normalizeRequiredCoverageFolders([]string{" inbox, archive ", "SPAM_BLOCKED", "archive"})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := []string{"INBOX", "ARCHIVE", "SPAM_BLOCKED"}
	if len(got) != len(want) {
		t.Fatalf("folders = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("folders = %v, want %v", got, want)
		}
	}
	if _, err := normalizeRequiredCoverageFolders([]string{"", " , "}); err == nil {
		t.Fatal("empty required folder list unexpectedly succeeded")
	}
}

func TestVerifyCoverageReportComplete(t *testing.T) {
	report := coverageReport{
		Summary: coverageSummary{Conversations: 2},
		Folders: map[string]coverageFolderView{
			"INBOX":        {Status: store.CoverageComplete},
			"ARCHIVE":      {Status: store.CoverageComplete},
			"SPAM_BLOCKED": {Status: store.CoverageComplete},
		},
		Conversations: map[string]coverageConversationView{
			"a": {Status: store.CoverageSourceExhausted},
			"b": {Status: store.CoverageSourceExhausted},
		},
	}

	got := verifyCoverageReport(report, defaultRequiredCoverageFolders)
	if !got.Complete {
		t.Fatalf("verification unexpectedly incomplete: %+v", got)
	}
	if got.FolderSummary.Complete != 3 || got.ConversationSummary.SourceExhausted != 2 || len(got.IncompleteConversations) != 0 {
		t.Fatalf("unexpected complete summary: %+v", got)
	}
	if err := coverageVerificationFailure(got); err != nil {
		t.Fatalf("complete verification returned an error: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"complete":true`) || !strings.Contains(string(raw), `"source_exhausted":2`) {
		t.Fatalf("JSON lacks completion metrics: %s", raw)
	}
}

func TestVerifyCoverageReportDistinguishesIncompleteStatuses(t *testing.T) {
	report := coverageReport{
		Summary: coverageSummary{Conversations: 5},
		Folders: map[string]coverageFolderView{
			"INBOX":        {Status: store.CoverageComplete},
			"ARCHIVE":      {Status: store.CoverageFailed},
			"SPAM_BLOCKED": {Status: store.CoveragePartial},
		},
		Conversations: map[string]coverageConversationView{
			"exhausted": {Status: store.CoverageSourceExhausted},
			"new":       {Status: store.CoverageNotAttempted},
			"running":   {Status: store.CoverageInProgress},
			"partial":   {Status: store.CoveragePartial},
			"failed":    {Status: store.CoverageFailed},
		},
	}

	got := verifyCoverageReport(report, []string{"INBOX", "ARCHIVE", "SPAM_BLOCKED", "MISSING_FOLDER"})
	if got.Complete {
		t.Fatalf("verification unexpectedly complete: %+v", got)
	}
	if err := coverageVerificationFailure(got); err == nil {
		t.Fatal("incomplete verification did not produce a nonzero-exit error")
	}
	if got.FolderSummary.Required != 4 || got.FolderSummary.Complete != 1 || got.FolderSummary.Missing != 1 || got.FolderSummary.Failed != 1 || got.FolderSummary.Partial != 1 {
		t.Fatalf("folder statuses were not distinguished: %+v", got.FolderSummary)
	}
	if got.FolderSummary.ByStatus["missing"] != 1 || got.FolderStatuses["MISSING_FOLDER"] != "missing" {
		t.Fatalf("missing folder was not reported: %+v", got)
	}
	conversations := got.ConversationSummary
	if conversations.SourceExhausted != 1 || conversations.NotAttempted != 1 || conversations.InProgress != 1 || conversations.Partial != 1 || conversations.Failed != 1 {
		t.Fatalf("conversation statuses were not distinguished: %+v", conversations)
	}
	if len(got.IncompleteConversations) != 4 || got.IncompleteConversations["failed"] != store.CoverageFailed {
		t.Fatalf("incomplete conversations were not identified: %+v", got.IncompleteConversations)
	}
}

func TestVerifyCoverageReportAllowsAnEmptyCompleteMailbox(t *testing.T) {
	report := coverageReport{
		Summary:       coverageSummary{Conversations: 0},
		Folders:       map[string]coverageFolderView{"INBOX": {Status: store.CoverageComplete}},
		Conversations: map[string]coverageConversationView{},
	}
	got := verifyCoverageReport(report, []string{"INBOX"})
	if !got.Complete {
		t.Fatalf("empty but fully enumerated mailbox should be complete: %+v", got)
	}
}
