package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
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

var defaultRequiredCoverageFolders = []string{"INBOX", "ARCHIVE", "SPAM_BLOCKED"}

type coverageFolderVerificationSummary struct {
	Required int            `json:"required"`
	Complete int            `json:"complete"`
	Missing  int            `json:"missing"`
	Failed   int            `json:"failed"`
	Partial  int            `json:"partial"`
	ByStatus map[string]int `json:"by_status"`
}

type coverageConversationVerificationSummary struct {
	Conversations   int            `json:"conversations"`
	SourceExhausted int            `json:"source_exhausted"`
	NotAttempted    int            `json:"not_attempted"`
	InProgress      int            `json:"in_progress"`
	Partial         int            `json:"partial"`
	Failed          int            `json:"failed"`
	ByStatus        map[string]int `json:"by_status"`
}

type coverageVerification struct {
	Version                 int                                     `json:"version"`
	VerifiedAt              time.Time                               `json:"verified_at"`
	Complete                bool                                    `json:"complete"`
	RequiredFolders         []string                                `json:"required_folders"`
	FolderSummary           coverageFolderVerificationSummary       `json:"folder_summary"`
	ConversationSummary     coverageConversationVerificationSummary `json:"conversation_summary"`
	FolderStatuses          map[string]string                       `json:"folder_statuses"`
	IncompleteConversations map[string]string                       `json:"incomplete_conversations,omitempty"`
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
	c.AddCommand(coverageVerifyCmd())
	return c
}

func coverageVerifyCmd() *cobra.Command {
	requiredFolders := append([]string(nil), defaultRequiredCoverageFolders...)
	c := &cobra.Command{
		Use:   "verify",
		Short: "Fail unless folder discovery and all conversation history are complete",
		Long: "Verify that every required conversation folder has complete discovery coverage " +
			"and every locally known conversation is source-exhausted. The command exits nonzero " +
			"when either condition is not met.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			folders, err := normalizeRequiredCoverageFolders(requiredFolders)
			if err != nil {
				return err
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			report, err := buildCoverageReport(cmd.Context(), st)
			if err != nil {
				return err
			}
			verification := verifyCoverageReport(report, folders)
			if flags.jsonOut {
				if err := output.JSON(os.Stdout, verification); err != nil {
					return err
				}
			} else {
				renderCoverageVerification(verification)
			}
			return coverageVerificationFailure(verification)
		},
	}
	c.Flags().StringSliceVar(&requiredFolders, "folder", requiredFolders, "required folder name (repeat or use comma-separated values)")
	return c
}

func coverageVerificationFailure(verification coverageVerification) error {
	if verification.Complete {
		return nil
	}
	return fmt.Errorf("coverage verification failed: %d/%d required folders complete and %d/%d conversations source-exhausted",
		verification.FolderSummary.Complete, verification.FolderSummary.Required,
		verification.ConversationSummary.SourceExhausted, verification.ConversationSummary.Conversations)
}

func normalizeRequiredCoverageFolders(values []string) ([]string, error) {
	seen := make(map[string]struct{})
	folders := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			folder := strings.ToUpper(strings.TrimSpace(part))
			if folder == "" {
				continue
			}
			if _, ok := seen[folder]; ok {
				continue
			}
			seen[folder] = struct{}{}
			folders = append(folders, folder)
		}
	}
	if len(folders) == 0 {
		return nil, fmt.Errorf("at least one --folder is required")
	}
	return folders, nil
}

func verifyCoverageReport(report coverageReport, requiredFolders []string) coverageVerification {
	result := coverageVerification{
		Version: 1, VerifiedAt: time.Now().UTC(), RequiredFolders: append([]string(nil), requiredFolders...),
		FolderSummary: coverageFolderVerificationSummary{Required: len(requiredFolders), ByStatus: make(map[string]int)},
		ConversationSummary: coverageConversationVerificationSummary{
			Conversations: report.Summary.Conversations,
			ByStatus:      make(map[string]int),
		},
		FolderStatuses:          make(map[string]string, len(requiredFolders)),
		IncompleteConversations: make(map[string]string),
	}
	for _, folder := range requiredFolders {
		status := "missing"
		if value, ok := report.Folders[folder]; ok {
			status = value.Status
		}
		result.FolderStatuses[folder] = status
		result.FolderSummary.ByStatus[status]++
		switch status {
		case store.CoverageComplete:
			result.FolderSummary.Complete++
		case "missing":
			result.FolderSummary.Missing++
		case store.CoverageFailed:
			result.FolderSummary.Failed++
		case store.CoveragePartial:
			result.FolderSummary.Partial++
		}
	}
	for id, conversation := range report.Conversations {
		status := conversation.Status
		result.ConversationSummary.ByStatus[status]++
		switch status {
		case store.CoverageSourceExhausted:
			result.ConversationSummary.SourceExhausted++
		case store.CoverageNotAttempted:
			result.ConversationSummary.NotAttempted++
		case store.CoverageInProgress:
			result.ConversationSummary.InProgress++
		case store.CoveragePartial:
			result.ConversationSummary.Partial++
		case store.CoverageFailed:
			result.ConversationSummary.Failed++
		}
		if status != store.CoverageSourceExhausted {
			result.IncompleteConversations[id] = status
		}
	}
	result.Complete = result.FolderSummary.Complete == result.FolderSummary.Required &&
		result.ConversationSummary.SourceExhausted == result.ConversationSummary.Conversations
	return result
}

func renderCoverageVerification(result coverageVerification) {
	status := "INCOMPLETE"
	if result.Complete {
		status = "COMPLETE"
	}
	fmt.Printf("Coverage verification: %s\n", status)
	fmt.Printf("Required folders: %d/%d complete; %d missing, %d failed, %d partial\n",
		result.FolderSummary.Complete, result.FolderSummary.Required, result.FolderSummary.Missing,
		result.FolderSummary.Failed, result.FolderSummary.Partial)
	fmt.Printf("Conversations: %d/%d source-exhausted\n",
		result.ConversationSummary.SourceExhausted, result.ConversationSummary.Conversations)
	statuses := make([]string, 0, len(result.ConversationSummary.ByStatus))
	for status := range result.ConversationSummary.ByStatus {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	for _, status := range statuses {
		fmt.Printf("  %-18s %d\n", status, result.ConversationSummary.ByStatus[status])
	}
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
