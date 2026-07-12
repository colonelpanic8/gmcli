package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/fdsouvenir/gmcli/internal/output"
	"github.com/fdsouvenir/gmcli/internal/store"
)

type coverageConversationView struct {
	Status             string                  `json:"status"`
	HistoryStartMS     *int64                  `json:"history_start_ms,omitempty"`
	LastAttemptAt      *time.Time              `json:"last_attempt_at,omitempty"`
	LastSuccessAt      *time.Time              `json:"last_success_at,omitempty"`
	ExhaustedAt        *time.Time              `json:"exhausted_at,omitempty"`
	TerminalReason     string                  `json:"terminal_reason,omitempty"`
	LastError          string                  `json:"last_error,omitempty"`
	LastRequests       int                     `json:"last_requests"`
	LastRecordsFetched int                     `json:"last_records_fetched"`
	Segments           []store.CoverageSegment `json:"segments"`
}

type coverageFolderView struct {
	Status            string     `json:"status"`
	PagesFetched      int        `json:"pages_fetched"`
	ConversationsSeen int        `json:"conversations_seen"`
	LastAttemptAt     *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt     *time.Time `json:"last_success_at,omitempty"`
	TerminalReason    string     `json:"terminal_reason,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
}

type coverageSummary struct {
	Conversations   int            `json:"conversations"`
	ByStatus        map[string]int `json:"by_status"`
	CoveredSegments int            `json:"covered_segments"`
}

type coverageReport struct {
	Version       int                                 `json:"version"`
	GeneratedAt   time.Time                           `json:"generated_at"`
	Summary       coverageSummary                     `json:"summary"`
	Folders       map[string]coverageFolderView       `json:"folders"`
	Conversations map[string]coverageConversationView `json:"conversations"`
}

func coverageCmd() *cobra.Command {
	var conversationID string
	c := &cobra.Command{
		Use:   "coverage",
		Short: "Inspect durable folder and per-conversation synchronization coverage",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			report, err := buildCoverageReport(cmd.Context(), st)
			if err != nil {
				return err
			}
			if conversationID != "" {
				value, ok := report.Conversations[conversationID]
				if !ok {
					return fmt.Errorf("conversation %s not found", conversationID)
				}
				if flags.jsonOut {
					return output.JSON(os.Stdout, value)
				}
				renderConversationCoverage(conversationID, value)
				return nil
			}
			if flags.jsonOut {
				return output.JSON(os.Stdout, report)
			}
			renderCoverageReport(report)
			return nil
		},
	}
	c.Flags().StringVar(&conversationID, "conversation", "", "show one conversation_id")
	return c
}

func buildCoverageReport(ctx context.Context, st *store.Store) (coverageReport, error) {
	conversations, err := st.ListConversationCoverage(ctx)
	if err != nil {
		return coverageReport{}, err
	}
	folders, err := st.ListFolderCoverage(ctx)
	if err != nil {
		return coverageReport{}, err
	}
	report := coverageReport{
		Version: 1, GeneratedAt: time.Now().UTC(),
		Summary: coverageSummary{Conversations: len(conversations), ByStatus: make(map[string]int)},
		Folders: make(map[string]coverageFolderView), Conversations: make(map[string]coverageConversationView),
	}
	for _, value := range conversations {
		report.Summary.ByStatus[value.Status]++
		report.Summary.CoveredSegments += len(value.Segments)
		report.Conversations[value.ConversationID] = coverageConversationView{
			Status: value.Status, HistoryStartMS: value.HistoryStartMS,
			LastAttemptAt: coverageTimePtr(value.LastAttemptAt), LastSuccessAt: coverageTimePtr(value.LastSuccessAt),
			ExhaustedAt: coverageTimePtr(value.ExhaustedAt), TerminalReason: value.TerminalReason,
			LastError: value.LastError, LastRequests: value.LastRequests,
			LastRecordsFetched: value.LastRecordsFetched, Segments: value.Segments,
		}
	}
	for _, value := range folders {
		report.Folders[value.Folder] = coverageFolderView{
			Status: value.Status, PagesFetched: value.PagesFetched,
			ConversationsSeen: value.ConversationsSeen, LastAttemptAt: coverageTimePtr(value.LastAttemptAt),
			LastSuccessAt: coverageTimePtr(value.LastSuccessAt), TerminalReason: value.TerminalReason,
			LastError: value.LastError,
		}
	}
	return report, nil
}

func coverageTimePtr(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func renderCoverageReport(report coverageReport) {
	fmt.Printf("Conversation coverage: %d total, %d segment(s)\n", report.Summary.Conversations, report.Summary.CoveredSegments)
	statuses := make([]string, 0, len(report.Summary.ByStatus))
	for status := range report.Summary.ByStatus {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	for _, status := range statuses {
		fmt.Printf("  %-18s %d\n", status, report.Summary.ByStatus[status])
	}
	if len(report.Folders) > 0 {
		fmt.Println("Folder discovery:")
		folders := make([]string, 0, len(report.Folders))
		for folder := range report.Folders {
			folders = append(folders, folder)
		}
		sort.Strings(folders)
		for _, folder := range folders {
			value := report.Folders[folder]
			fmt.Printf("  %-14s %-9s pages=%d conversations=%d reason=%s\n", folder, value.Status, value.PagesFetched, value.ConversationsSeen, value.TerminalReason)
		}
	}
}

func renderConversationCoverage(id string, value coverageConversationView) {
	fmt.Printf("conversation_id: %s\nstatus:          %s\nterminal_reason: %s\nsegments:        %d\n", id, value.Status, value.TerminalReason, len(value.Segments))
	if value.HistoryStartMS != nil {
		fmt.Printf("history_start:    %s\n", time.UnixMilli(*value.HistoryStartMS).Format(time.RFC3339))
	}
	if value.LastError != "" {
		fmt.Printf("last_error:       %s\n", value.LastError)
	}
	for _, segment := range value.Segments {
		fmt.Printf("  [%s, %s) verified %s\n", time.UnixMilli(segment.StartMS).Format(time.RFC3339), time.UnixMilli(segment.EndMS).Format(time.RFC3339), segment.VerifiedAt.Format(time.RFC3339))
	}
}
