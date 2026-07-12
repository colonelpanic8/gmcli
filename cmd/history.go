package cmd

import (
	"context"
	"fmt"
	"os"
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

type historyBackfillAllResult struct {
	Conversations   int                     `json:"conversations"`
	Offset          int                     `json:"offset"`
	Attempted       int                     `json:"attempted"`
	NextOffset      int                     `json:"next_offset"`
	Completed       int                     `json:"completed"`
	Failed          int                     `json:"failed"`
	Exhausted       int                     `json:"exhausted"`
	NeedsMore       int                     `json:"needs_more"`
	FetchedMessages int                     `json:"fetched_messages"`
	MessagesAdded   int                     `json:"messages_added"`
	Results         []historyBackfillResult `json:"results"`
	Errors          []historyBackfillError  `json:"errors,omitempty"`
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
	c := &cobra.Command{
		Use:   "backfill-all",
		Short: "Fetch older messages for every locally known conversation",
		Long: "Fetch older messages for every locally known conversation over one connection. " +
			"Failures are recorded per conversation so the remaining archive can continue.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if requests <= 0 {
				requests = 10
			}
			if count <= 0 {
				count = 50
			}
			res, err := runHistoryBackfillAll(requests, count, offset, limit)
			if flags.jsonOut {
				if outputErr := output.JSON(os.Stdout, res); outputErr != nil {
					return outputErr
				}
			} else {
				fmt.Fprintf(os.Stderr, "Backfilled %d/%d attempted conversation(s): fetched %d message record(s), added %d local message(s); %d exhausted, %d need more, %d failed; next offset %d\n",
					res.Completed, res.Attempted, res.FetchedMessages, res.MessagesAdded, res.Exhausted, res.NeedsMore, res.Failed, res.NextOffset)
			}
			return err
		},
	}
	c.Flags().IntVar(&requests, "requests", 10, "max FetchMessages calls to make per conversation")
	c.Flags().Int64Var(&count, "count", 50, "max message records to request per FetchMessages call")
	c.Flags().IntVar(&offset, "offset", 0, "skip this many locally known conversations (for resumable runs)")
	c.Flags().IntVar(&limit, "limit", 0, "process at most this many conversations after --offset (0 means all)")
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

func runHistoryBackfillAll(requests int, count int64, offset, limit int) (historyBackfillAllResult, error) {
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

	total, err := st.CountConversations(ctx)
	if err != nil {
		return historyBackfillAllResult{}, fmt.Errorf("count conversations: %w", err)
	}
	conversations, err := st.ListConversations(ctx, store.ListConversationOpts{Limit: total})
	if err != nil {
		return historyBackfillAllResult{}, err
	}
	if offset < 0 {
		return historyBackfillAllResult{}, fmt.Errorf("offset must be non-negative")
	}
	if offset > len(conversations) {
		offset = len(conversations)
	}
	selected := conversations[offset:]
	if limit > 0 && limit < len(selected) {
		selected = selected[:limit]
	}
	result := historyBackfillAllResult{
		Conversations: len(conversations),
		Offset:        offset,
		Attempted:     len(selected),
		NextOffset:    offset,
		Results:       make([]historyBackfillResult, 0, len(selected)),
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
		absoluteIndex := offset + i
		fmt.Fprintf(os.Stderr, "Backfilling conversation %d/%d (%s)\n", absoluteIndex+1, len(conversations), conversation.ID)
		res, err := runHistoryBackfillConnected(ctx, st, client, pump, conversation.ID, requests, count)
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, historyBackfillError{ConversationID: conversation.ID, Error: err.Error()})
			fmt.Fprintf(os.Stderr, "Backfill failed for %s: %v\n", conversation.ID, err)
			if isTerminalSessionError(err) {
				result.NextOffset = absoluteIndex
				return result, fmt.Errorf("session expired at conversation offset %d; re-pair and resume with --offset %d: %w", absoluteIndex, absoluteIndex, err)
			}
			result.NextOffset = absoluteIndex + 1
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
		result.NextOffset = absoluteIndex + 1
	}
	if result.Failed > 0 || result.NeedsMore > 0 {
		return result, fmt.Errorf("history backfill incomplete: %d conversation(s) failed and %d reached the request limit before exhaustion", result.Failed, result.NeedsMore)
	}
	return result, nil
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
		if sameCursor(cursor, next) {
			res.CoverageStatus, res.TerminalReason = store.CoveragePartial, "same_cursor"
			if err := st.FinishConversationCoverage(ctx, chat, res.CoverageStatus, res.TerminalReason, nil, res.Requests, res.FetchedMessages, ""); err != nil {
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
