package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fdsouvenir/gmcli/internal/store"
)

func TestConversationAndFolderCoveragePersistsAndMerges(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "coverage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertConversation(ctx, store.Conversation{ID: "chat-1", Name: "Test"}); err != nil {
		t.Fatal(err)
	}
	if err := st.StartConversationCoverage(ctx, "chat-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordConversationCoveragePage(ctx, "chat-1", 200, 301, 1, 50); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordConversationCoveragePage(ctx, "chat-1", 100, 200, 2, 100); err != nil {
		t.Fatal(err)
	}
	historyStart := int64(100)
	if err := st.FinishConversationCoverage(ctx, "chat-1", store.CoverageSourceExhausted, "empty_page", &historyStart, 3, 100, ""); err != nil {
		t.Fatal(err)
	}
	coverage, err := st.ListConversationCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(coverage) != 1 || coverage[0].Status != store.CoverageSourceExhausted || coverage[0].HistoryStartMS == nil || *coverage[0].HistoryStartMS != historyStart {
		t.Fatalf("unexpected conversation coverage: %+v", coverage)
	}
	if len(coverage[0].Segments) != 1 || coverage[0].Segments[0].StartMS != 100 || coverage[0].Segments[0].EndMS != 301 {
		t.Fatalf("coverage segments did not merge: %+v", coverage[0].Segments)
	}
	exhaustedAt := coverage[0].ExhaustedAt
	if err := st.StartConversationCoverage(ctx, "chat-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishConversationCoverage(ctx, "chat-1", store.CoverageFailed, "error", nil, 0, 0, "temporary relay failure"); err != nil {
		t.Fatal(err)
	}
	coverage, err = st.ListConversationCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if coverage[0].Status != store.CoverageSourceExhausted || coverage[0].TerminalReason != "empty_page" || coverage[0].LastError != "temporary relay failure" || !coverage[0].ExhaustedAt.Equal(exhaustedAt) {
		t.Fatalf("failed recheck erased source-exhausted proof: %+v", coverage[0])
	}
	if err := st.StartConversationCoverage(ctx, "chat-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordConversationCoveragePage(ctx, "chat-1", 50, 101, 1, 50); err != nil {
		t.Fatal(err)
	}
	coverage, err = st.ListConversationCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if coverage[0].Status != store.CoveragePartial {
		t.Fatalf("successful recheck page did not invalidate current exhaustion status: %+v", coverage[0])
	}
	if err := st.FinishConversationCoverage(ctx, "chat-1", store.CoverageSourceExhausted, "empty_page", &historyStart, 2, 50, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertConversation(ctx, store.Conversation{ID: "chat-2"}); err != nil {
		t.Fatal(err)
	}
	if err := st.StartConversationCoverage(ctx, "chat-2"); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishConversationCoverage(ctx, "chat-2", store.CoveragePartial, "request_budget", nil, 1, 50, ""); err != nil {
		t.Fatal(err)
	}
	exhaustedIDs, err := st.SourceExhaustedConversationIDs(ctx)
	if err != nil {
		t.Fatalf("list source-exhausted IDs: %v", err)
	}
	if _, ok := exhaustedIDs["chat-1"]; !ok || len(exhaustedIDs) != 1 {
		t.Fatalf("source-exhausted IDs = %v, want only chat-1", exhaustedIDs)
	}

	if err := st.StartFolderCoverage(ctx, "INBOX"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordFolderCoveragePage(ctx, "INBOX", 244); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordFolderCoveragePage(ctx, "INBOX", 219); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishFolderCoverage(ctx, "INBOX", store.CoverageComplete, "short_page", ""); err != nil {
		t.Fatal(err)
	}
	folders, err := st.ListFolderCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0].Status != store.CoverageComplete || folders[0].PagesFetched != 2 || folders[0].ConversationsSeen != 463 {
		t.Fatalf("unexpected folder coverage: %+v", folders)
	}
}
