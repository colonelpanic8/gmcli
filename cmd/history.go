package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/fdsouvenir/gmcli/internal/gm"
	"github.com/fdsouvenir/gmcli/internal/output"
	"github.com/fdsouvenir/gmcli/internal/store"
	gmsync "github.com/fdsouvenir/gmcli/internal/sync"
)

type historyBackfillResult struct {
	ConversationID       string `json:"conversation_id"`
	Requests             int    `json:"requests"`
	Count                int64  `json:"count"`
	FetchedMessages      int    `json:"fetched_messages"`
	SyncRecordsProcessed int    `json:"sync_records_processed"`
	MessagesBefore       int    `json:"messages_before"`
	MessagesAfter        int    `json:"messages_after"`
	MessagesAddedForChat int    `json:"messages_added_for_chat"`
	Exhausted            bool   `json:"exhausted"`
	RequestLimitReached  bool   `json:"request_limit_reached"`
	CoverageStatus       string `json:"coverage_status"`
	TerminalReason       string `json:"terminal_reason"`
}

type historyBackfillError struct {
	ConversationID string `json:"conversation_id"`
	Error          string `json:"error"`
}

type historyBackfillAttemptOutcome string

const (
	historyBackfillOutcomeExhausted       historyBackfillAttemptOutcome = "exhausted"
	historyBackfillOutcomePartial         historyBackfillAttemptOutcome = "partial"
	historyBackfillOutcomeFailed          historyBackfillAttemptOutcome = "failed"
	historyBackfillOutcomeTerminalFailure historyBackfillAttemptOutcome = "terminal_failure"
)

type historyBackfillAllResult struct {
	Conversations           int                     `json:"conversations"`
	Eligible                int                     `json:"eligible"`
	SkippedExhausted        int                     `json:"skipped_exhausted"`
	Selected                int                     `json:"selected"`
	Offset                  int                     `json:"offset,omitempty"`
	AfterConversationID     string                  `json:"after_conversation_id"`
	Attempted               int                     `json:"attempted"`
	NextOffset              int                     `json:"next_offset,omitempty"`
	NextAfterConversationID string                  `json:"next_after_conversation_id"`
	Completed               int                     `json:"completed"`
	Failed                  int                     `json:"failed"`
	Exhausted               int                     `json:"exhausted"`
	NeedsMore               int                     `json:"needs_more"`
	FetchedMessages         int                     `json:"fetched_messages"`
	MessagesAdded           int                     `json:"messages_added"`
	Results                 []historyBackfillResult `json:"results"`
	Errors                  []historyBackfillError  `json:"errors,omitempty"`
}

func historyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "history",
		Short: "Best-effort message history backfill",
		Long: "Fetch older messages for a conversation through the paired phone. " +
			"Like wacli, this is best-effort: Google may return partial history, " +
			"and the phone must be online.",
	}
	c.AddCommand(historyBackfillCmd(), historyBackfillAllCmd())
	return c
}

func historyBackfillAllCmd() *cobra.Command {
	var requests int
	var count int64
	var offset int
	var limit int
	var includeExhausted bool
	var afterConversationID string
	c := &cobra.Command{
		Use:   "backfill-all",
		Short: "Fetch older messages for every locally known conversation",
		Long: "Fetch older messages for every locally known conversation over one connection. " +
			"Conversations already proven source-exhausted are skipped by default. Failures are " +
			"recorded per conversation so the remaining archive can continue. Resume a bounded or " +
			"interrupted pass with next_after_conversation_id from JSON output. Conversation IDs are " +
			"ordered lexicographically, making the cursor stable when message timestamps or coverage " +
			"statuses change. Use `gmcli coverage verify` as the archive completeness gate.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if requests <= 0 {
				requests = 10
			}
			if count <= 0 {
				count = 50
			}
			if err := validateHistoryBackfillResume(offset, afterConversationID); err != nil {
				return err
			}
			res, err := runHistoryBackfillAll(requests, count, offset, limit, includeExhausted, afterConversationID)
			// A nil Results slice means the pass snapshot was never validated
			// (for example, a stale key cursor). Do not print a zero-attempt
			// summary that could be mistaken for a completed pass.
			if err != nil && res.Results == nil {
				return err
			}
			if flags.jsonOut {
				if outputErr := output.JSON(os.Stdout, res); outputErr != nil {
					return outputErr
				}
			} else {
				fmt.Fprintf(os.Stderr, "Backfilled %d/%d attempted conversation(s): fetched %d message record(s), added %d local message(s); %d exhausted, %d need more, %d failed, %d previously exhausted skipped; next after-conversation-id %q\n",
					res.Completed, res.Attempted, res.FetchedMessages, res.MessagesAdded, res.Exhausted, res.NeedsMore, res.Failed, res.SkippedExhausted, res.NextAfterConversationID)
				fmt.Fprintln(os.Stderr, "Run `gmcli coverage verify` to determine archive completeness.")
			}
			return err
		},
	}
	c.Flags().IntVar(&requests, "requests", 10, "max FetchMessages calls to make per conversation")
	c.Flags().Int64Var(&count, "count", 50, "max message records to request per FetchMessages call")
	c.Flags().IntVar(&offset, "offset", 0, "deprecated; nonzero offsets are rejected, use --after-conversation-id")
	c.Flags().StringVar(&afterConversationID, "after-conversation-id", "", "resume after this conversation ID from a prior pass")
	c.Flags().IntVar(&limit, "limit", 0, "process at most this many eligible conversations after the resume cursor (0 means all)")
	c.Flags().BoolVar(&includeExhausted, "include-exhausted", false, "include and recheck conversations already marked source-exhausted")
	return c
}

func historyBackfillCmd() *cobra.Command {
	var chat string
	var requests int
	var count int64
	c := &cobra.Command{
		Use:   "backfill",
		Short: "Fetch older messages for one conversation",
		Long: "Fetch older messages for one conversation. --requests limits how many " +
			"FetchMessages calls gmcli makes, and --count limits how many message " +
			"records each call asks the phone for. JSON output separates protocol " +
			"records processed from messages added to the target conversation.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if chat == "" {
				return fmt.Errorf("--chat is required")
			}
			if requests <= 0 {
				requests = 10
			}
			if count <= 0 {
				count = 50
			}
			res, err := runHistoryBackfill(chat, requests, count)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return output.JSON(os.Stdout, res)
			}
			fmt.Fprintf(os.Stderr, "Backfill for %s: fetched %d message record(s), chat messages %d -> %d (+%d), using %d request(s)\n",
				res.ConversationID, res.FetchedMessages, res.MessagesBefore, res.MessagesAfter, res.MessagesAddedForChat, res.Requests)
			return nil
		},
	}
	c.Flags().StringVar(&chat, "chat", "", "conversation_id to backfill")
	c.Flags().IntVar(&requests, "requests", 10, "max FetchMessages calls to make for the target conversation")
	c.Flags().Int64Var(&count, "count", 50, "max message records to request per FetchMessages call")
	return c
}

func runHistoryBackfill(chat string, requests int, count int64) (historyBackfillResult, error) {
	layout, err := resolveLayout()
	if err != nil {
		return historyBackfillResult{}, err
	}
	logger := newLogger()
	ctx, cancel := signalContext(context.Background())
	defer cancel()

	st, err := store.Open(ctx, layout.Database)
	if err != nil {
		return historyBackfillResult{}, fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	client, err := gm.Open(layout, logger)
	if err != nil {
		return historyBackfillResult{}, err
	}

	pump := gmsync.New(st, logger)
	client.Subscribe(pump.Handle)

	if err := client.Connect(); err != nil {
		return historyBackfillResult{}, fmt.Errorf("connect: %w", err)
	}
	defer client.Disconnect()

	return runHistoryBackfillConnected(ctx, st, client, pump, chat, requests, count)
}

func runHistoryBackfillAll(requests int, count int64, offset, limit int, includeExhausted bool, afterConversationID string) (historyBackfillAllResult, error) {
	if err := validateHistoryBackfillResume(offset, afterConversationID); err != nil {
		return historyBackfillAllResult{}, err
	}
	layout, err := resolveLayout()
	if err != nil {
		return historyBackfillAllResult{}, err
	}
	logger := newLogger()
	ctx, cancel := signalContext(context.Background())
	defer cancel()

	st, err := store.Open(ctx, layout.Database)
	if err != nil {
		return historyBackfillAllResult{}, fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	conversations, err := st.ListConversationsByID(ctx)
	if err != nil {
		return historyBackfillAllResult{}, err
	}
	exhaustedIDs, err := st.SourceExhaustedConversationIDs(ctx)
	if err != nil {
		return historyBackfillAllResult{}, fmt.Errorf("list source-exhausted conversations: %w", err)
	}
	selected, eligible, skippedExhausted, err := selectHistoryBackfillConversations(conversations, exhaustedIDs, includeExhausted, afterConversationID, limit)
	if err != nil {
		return historyBackfillAllResult{}, err
	}
	result := historyBackfillAllResult{
		Conversations:           len(conversations),
		Eligible:                eligible,
		SkippedExhausted:        skippedExhausted,
		Selected:                len(selected),
		AfterConversationID:     afterConversationID,
		NextAfterConversationID: afterConversationID,
		Results:                 make([]historyBackfillResult, 0, len(selected)),
	}
	if len(selected) == 0 {
		return result, nil
	}

	client, err := gm.Open(layout, logger)
	if err != nil {
		return result, err
	}
	pump := gmsync.New(st, logger)
	client.Subscribe(pump.Handle)
	if err := client.Connect(); err != nil {
		return result, fmt.Errorf("connect: %w", err)
	}
	defer client.Disconnect()

	for i, conversation := range selected {
		fmt.Fprintf(os.Stderr, "Backfilling selected conversation %d/%d (%s)\n", i+1, len(selected), conversation.ID)
		result.Attempted++
		res, err := runHistoryBackfillConnected(ctx, st, client, pump, conversation.ID, requests, count)
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, historyBackfillError{ConversationID: conversation.ID, Error: err.Error()})
			fmt.Fprintf(os.Stderr, "Backfill failed for %s: %v\n", conversation.ID, err)
			if isTerminalSessionError(err) {
				result.NextAfterConversationID = nextHistoryBackfillCursor(result.NextAfterConversationID, conversation.ID, historyBackfillOutcomeTerminalFailure)
				return result, fmt.Errorf("session expired at conversation %s; re-pair and resume %s: %w",
					conversation.ID, historyBackfillResumeHint(result.NextAfterConversationID), err)
			}
			result.NextAfterConversationID = nextHistoryBackfillCursor(result.NextAfterConversationID, conversation.ID, historyBackfillOutcomeFailed)
			continue
		}
		result.Completed++
		if res.Exhausted {
			result.Exhausted++
		} else {
			result.NeedsMore++
		}
		result.FetchedMessages += res.FetchedMessages
		result.MessagesAdded += res.MessagesAddedForChat
		result.Results = append(result.Results, res)
		outcome := historyBackfillOutcomePartial
		if res.Exhausted {
			outcome = historyBackfillOutcomeExhausted
		}
		result.NextAfterConversationID = nextHistoryBackfillCursor(result.NextAfterConversationID, conversation.ID, outcome)
	}
	if result.Failed > 0 || result.NeedsMore > 0 {
		return result, fmt.Errorf("history backfill pass incomplete: %d conversation(s) failed and %d need more history", result.Failed, result.NeedsMore)
	}
	return result, nil
}

func validateHistoryBackfillResume(offset int, afterConversationID string) error {
	if offset == 0 {
		return nil
	}
	if afterConversationID != "" {
		return fmt.Errorf("--offset and --after-conversation-id cannot be combined; nonzero --offset is no longer supported")
	}
	return fmt.Errorf("nonzero --offset is no longer supported because conversation ordering and eligibility can change; resume with --after-conversation-id from next_after_conversation_id")
}

func nextHistoryBackfillCursor(current, conversationID string, outcome historyBackfillAttemptOutcome) string {
	if outcome == historyBackfillOutcomeTerminalFailure {
		return current
	}
	return conversationID
}

func historyBackfillResumeHint(afterConversationID string) string {
	if afterConversationID == "" {
		return "without --after-conversation-id"
	}
	return fmt.Sprintf("with --after-conversation-id %q", afterConversationID)
}

func selectHistoryBackfillConversations(conversations []store.Conversation, exhaustedIDs map[string]struct{}, includeExhausted bool, afterConversationID string, limit int) ([]store.Conversation, int, int, error) {
	ordered := append([]store.Conversation(nil), conversations...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	if afterConversationID != "" {
		found := false
		for _, conversation := range ordered {
			if conversation.ID == afterConversationID {
				found = true
				break
			}
		}
		if !found {
			return nil, 0, 0, fmt.Errorf("after-conversation-id %q is not a locally known conversation; start a new pass without the stale cursor", afterConversationID)
		}
	}

	eligible := make([]store.Conversation, 0, len(ordered))
	skippedExhausted := 0
	for _, conversation := range ordered {
		_, exhausted := exhaustedIDs[conversation.ID]
		if exhausted && !includeExhausted {
			skippedExhausted++
			continue
		}
		eligible = append(eligible, conversation)
	}
	eligibleCount := len(eligible)
	selected := make([]store.Conversation, 0, len(eligible))
	for _, conversation := range eligible {
		if conversation.ID > afterConversationID {
			selected = append(selected, conversation)
		}
	}
	if limit > 0 && limit < len(selected) {
		selected = selected[:limit]
	}
	return selected, eligibleCount, skippedExhausted, nil
}

func runHistoryBackfillConnected(ctx context.Context, st *store.Store, client *gm.Client, pump *gmsync.Pump, chat string, requests int, count int64) (historyBackfillResult, error) {
	if conv, err := client.Underlying().GetConversation(chat); err == nil && conv != nil {
		pump.Handle(conv)
	} else if _, localErr := st.GetConversation(ctx, chat); localErr != nil {
		if err != nil {
			return historyBackfillResult{}, fmt.Errorf("get conversation %s: %w", chat, err)
		}
		return historyBackfillResult{}, fmt.Errorf("conversation %s is not in the local store; run `gmcli sync` first", chat)
	}
	if err := st.StartConversationCoverage(ctx, chat); err != nil {
		return historyBackfillResult{}, fmt.Errorf("start coverage: %w", err)
	}

	cursor, err := oldestCursor(ctx, st, chat)
	if err != nil {
		return historyBackfillResult{}, err
	}

	before, err := st.CountMessagesForConversation(ctx, chat)
	if err != nil {
		return historyBackfillResult{}, fmt.Errorf("count messages before backfill: %w", err)
	}

	res := historyBackfillResult{ConversationID: chat, Count: count, MessagesBefore: before, CoverageStatus: store.CoverageInProgress}
	for i := 0; i < requests; i++ {
		requestCursor := cursor
		resp, err := client.Underlying().FetchMessages(chat, count, cursor)
		if err != nil {
			res.CoverageStatus, res.TerminalReason = store.CoverageFailed, "error"
			_ = st.FinishConversationCoverage(ctx, chat, res.CoverageStatus, res.TerminalReason, nil, res.Requests, res.FetchedMessages, err.Error())
			return res, fmt.Errorf("fetch messages: %w", err)
		}
		res.Requests++
		msgs := resp.GetMessages()
		res.FetchedMessages += len(msgs)
		imported := pump.ImportMessages(ctx, msgs)
		res.SyncRecordsProcessed += imported
		next := resp.GetCursor()
		if len(msgs) == 0 {
			historyStart := int64(0)
			if requestCursor != nil {
				historyStart = normalizeHistoryTimestampMS(requestCursor.GetLastItemTimestamp())
				if historyStart > 0 {
					if err := st.RecordConversationCoveragePage(ctx, chat, 0, historyStart+1, res.Requests, res.FetchedMessages); err != nil {
						return res, fmt.Errorf("record exhausted coverage: %w", err)
					}
				}
			}
			res.CoverageStatus, res.TerminalReason = store.CoverageSourceExhausted, "empty_page"
			res.Exhausted = true
			if err := st.FinishConversationCoverage(ctx, chat, res.CoverageStatus, res.TerminalReason, &historyStart, res.Requests, res.FetchedMessages, ""); err != nil {
				return res, fmt.Errorf("finish exhausted coverage: %w", err)
			}
			break
		}
		startMS, endMS := historyPageBounds(msgs, requestCursor)
		if startMS < endMS {
			if err := st.RecordConversationCoveragePage(ctx, chat, startMS, endMS, res.Requests, res.FetchedMessages); err != nil {
				return res, fmt.Errorf("record coverage page: %w", err)
			}
		}
		if !cursorMovesBackward(cursor, next) {
			next = oldestMessageCursor(msgs)
		}
		if cursor != nil && (next == nil || !cursorMovesBackward(cursor, next)) {
			historyStart := normalizeHistoryTimestampMS(cursor.GetLastItemTimestamp())
			res.CoverageStatus, res.TerminalReason = store.CoverageSourceExhausted, "no_older_messages"
			res.Exhausted = true
			if err := st.FinishConversationCoverage(ctx, chat, res.CoverageStatus, res.TerminalReason, &historyStart, res.Requests, res.FetchedMessages, ""); err != nil {
				return res, err
			}
			break
		}
		if next == nil {
			res.CoverageStatus, res.TerminalReason = store.CoveragePartial, "missing_cursor"
			if err := st.FinishConversationCoverage(ctx, chat, res.CoverageStatus, res.TerminalReason, nil, res.Requests, res.FetchedMessages, ""); err != nil {
				return res, err
			}
			break
		}
		cursor = next
	}
	if res.CoverageStatus == store.CoverageInProgress {
		res.CoverageStatus, res.TerminalReason = store.CoveragePartial, "request_budget"
		res.RequestLimitReached = true
		if err := st.FinishConversationCoverage(ctx, chat, res.CoverageStatus, res.TerminalReason, nil, res.Requests, res.FetchedMessages, ""); err != nil {
			return res, err
		}
	}
	res.Exhausted = res.CoverageStatus == store.CoverageSourceExhausted
	after, err := st.CountMessagesForConversation(ctx, chat)
	if err != nil {
		return res, fmt.Errorf("count messages after backfill: %w", err)
	}
	res.MessagesAfter = after
	res.MessagesAddedForChat = after - before
	return res, nil
}

func historyPageBounds(messages []*gmproto.Message, cursor *gmproto.Cursor) (int64, int64) {
	var oldest, newest int64
	for _, message := range messages {
		if message == nil {
			continue
		}
		ts := normalizeHistoryTimestampMS(message.GetTimestamp())
		if ts <= 0 {
			continue
		}
		if oldest == 0 || ts < oldest {
			oldest = ts
		}
		if ts > newest {
			newest = ts
		}
	}
	if oldest == 0 {
		return 0, 0
	}
	end := newest + 1
	if cursor != nil {
		cursorEnd := normalizeHistoryTimestampMS(cursor.GetLastItemTimestamp()) + 1
		if cursorEnd > end {
			end = cursorEnd
		}
	}
	return oldest, end
}

func normalizeHistoryTimestampMS(ts int64) int64 {
	if ts > 100_000_000_000_000 {
		return ts / 1000
	}
	return ts
}

func isTerminalSessionError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "session_cookie_invalid") ||
		strings.Contains(message, "invalid authentication credentials") ||
		strings.Contains(message, "http 401") ||
		strings.Contains(message, "session expired")
}

func oldestCursor(ctx context.Context, st *store.Store, chat string) (*gmproto.Cursor, error) {
	msgs, err := st.ListMessages(ctx, store.ListMessageOpts{
		ConversationID: chat,
		Limit:          1,
		Order:          "asc",
	})
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	return &gmproto.Cursor{
		LastItemID:        msgs[0].ID,
		LastItemTimestamp: msgs[0].TimestampMS,
	}, nil
}

func sameCursor(a, b *gmproto.Cursor) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.GetLastItemID() == b.GetLastItemID() &&
		a.GetLastItemTimestamp() == b.GetLastItemTimestamp()
}

func oldestMessageCursor(messages []*gmproto.Message) *gmproto.Cursor {
	var oldest *gmproto.Message
	var oldestMS int64
	for _, message := range messages {
		if message == nil || message.GetMessageID() == "" {
			continue
		}
		ts := normalizeHistoryTimestampMS(message.GetTimestamp())
		if ts <= 0 || (oldest != nil && ts > oldestMS) {
			continue
		}
		oldest = message
		oldestMS = ts
	}
	if oldest == nil {
		return nil
	}
	return &gmproto.Cursor{LastItemID: oldest.GetMessageID(), LastItemTimestamp: oldestMS}
}

func cursorMovesBackward(current, next *gmproto.Cursor) bool {
	if next == nil || next.GetLastItemID() == "" {
		return false
	}
	if current == nil {
		return normalizeHistoryTimestampMS(next.GetLastItemTimestamp()) > 0
	}
	currentMS := normalizeHistoryTimestampMS(current.GetLastItemTimestamp())
	nextMS := normalizeHistoryTimestampMS(next.GetLastItemTimestamp())
	return nextMS < currentMS || (nextMS == currentMS && next.GetLastItemID() != current.GetLastItemID())
}
