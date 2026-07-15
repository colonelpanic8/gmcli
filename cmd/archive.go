package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/fdsouvenir/gmcli/internal/output"
	"github.com/fdsouvenir/gmcli/internal/unifiedarchive"
	archiveview "github.com/fdsouvenir/gmcli/internal/viewer"
	"github.com/fdsouvenir/gmcli/internal/viewerapi"
)

type archiveFlags struct {
	dir       string
	cachePath string
	rebuild   bool
}

type archiveSyncer struct {
	archive    *archiveview.Archive
	archiveDir string
	mu         sync.Mutex
}

func (s *archiveSyncer) Sync(ctx context.Context) (viewerapi.SyncResult, error) {
	if !s.mu.TryLock() {
		return viewerapi.SyncResult{}, viewerapi.ErrSyncInProgress
	}
	defer s.mu.Unlock()
	executable, err := os.Executable()
	if err != nil {
		return viewerapi.SyncResult{}, fmt.Errorf("locate gmcli executable: %w", err)
	}
	for _, arguments := range [][]string{
		{"sync"},
		{"export", "jsonl", "--out", s.archiveDir, "--force"},
	} {
		var commandErrors bytes.Buffer
		command := exec.CommandContext(ctx, executable, arguments...)
		command.Stdout = os.Stderr
		command.Stderr = io.MultiWriter(os.Stderr, &commandErrors)
		if err := command.Run(); err != nil {
			detail := strings.TrimSpace(commandErrors.String())
			if len(detail) > 2_000 {
				detail = detail[len(detail)-2_000:]
			}
			if detail != "" {
				return viewerapi.SyncResult{}, fmt.Errorf("gmcli %s failed: %s", strings.Join(arguments, " "), detail)
			}
			return viewerapi.SyncResult{}, fmt.Errorf("gmcli %s: %w", strings.Join(arguments, " "), err)
		}
	}
	if err := s.archive.Refresh(ctx); err != nil {
		return viewerapi.SyncResult{}, fmt.Errorf("refresh archive cache: %w", err)
	}
	metadata, err := s.archive.Metadata(ctx)
	if err != nil {
		return viewerapi.SyncResult{}, fmt.Errorf("read refreshed archive metadata: %w", err)
	}
	return viewerapi.SyncResult{
		Conversations: metadata.Conversations,
		Messages:      metadata.Messages,
		ExportedAt:    metadata.ExportedAt.Format(time.RFC3339Nano),
	}, nil
}

func archiveCmd() *cobra.Command {
	var options archiveFlags
	c := &cobra.Command{Use: "archive", Short: "Query portable JSONL archives"}
	c.PersistentFlags().StringVar(&options.dir, "dir", "", "authoritative JSONL archive directory (required)")
	c.PersistentFlags().StringVar(&options.cachePath, "cache", "", "SQLite cache path (default: $XDG_CACHE_HOME/gmcli/archives/<fingerprint>.sqlite)")
	c.PersistentFlags().BoolVar(&options.rebuild, "rebuild-cache", false, "discard and rebuild the disposable SQLite cache")
	c.AddCommand(archiveMetaCmd(&options), archiveConversationsCmd(&options), archiveMessagesCmd(&options), archiveSearchCmd(&options), archiveContextCmd(&options), archiveServeCmd(&options), archiveUnifiedCmd(&options))
	return c
}

func archiveServeCmd(options *archiveFlags) *cobra.Command {
	var listenAddress string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Serve the local archive query and sync HTTP API",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			archive, err := openArchive(cmd, options)
			if err != nil {
				return err
			}
			defer archive.Close()
			listener, err := net.Listen("tcp", listenAddress)
			if err != nil {
				return fmt.Errorf("listen for archive API: %w", err)
			}
			defer listener.Close()
			if tcp, ok := listener.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
				return fmt.Errorf("archive API must listen on a loopback address, got %s", listener.Addr())
			}
			token := os.Getenv("GMCLI_ARCHIVE_API_TOKEN")
			syncer := &archiveSyncer{archive: archive, archiveDir: options.dir}
			server := &http.Server{
				Handler:           viewerapi.New(archive, viewerapi.Options{BearerToken: token, Syncer: syncer}),
				ReadHeaderTimeout: 5 * time.Second,
				IdleTimeout:       60 * time.Second,
			}
			ctx, cancel := signalContext(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				select {
				case <-ctx.Done():
					shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer shutdownCancel()
					_ = server.Shutdown(shutdownCtx)
				case <-done:
				}
			}()
			defer close(done)
			fmt.Fprintf(os.Stderr, "archive API listening at http://%s\n", listener.Addr())
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("serve archive API: %w", err)
			}
			return nil
		},
	}
	c.Flags().StringVar(&listenAddress, "listen", "127.0.0.1:7878", "loopback address for the HTTP API")
	return c
}

type unifiedArchiveFlags struct {
	telephonyDir string
}

func archiveUnifiedCmd(options *archiveFlags) *cobra.Command {
	var unified unifiedArchiveFlags
	c := &cobra.Command{
		Use:   "unified",
		Short: "Dynamically unify relay and Android conversation fragments",
		Long: "Verifies both source archives and computes a canonical participant-set view in memory. " +
			"It does not write or cache a third archive.",
	}
	c.PersistentFlags().StringVar(&unified.telephonyDir, "telephony-dir", "", "authoritative Android Telephony archive directory (required)")
	c.AddCommand(archiveUnifiedMetaCmd(options, &unified), archiveUnifiedConversationsCmd(options, &unified), archiveUnifiedMessagesCmd(options, &unified))
	return c
}

func openUnifiedArchive(options *archiveFlags, unified *unifiedArchiveFlags) (*unifiedarchive.Dataset, error) {
	if options.dir == "" {
		return nil, errors.New("--dir is required for the relay archive")
	}
	if unified.telephonyDir == "" {
		return nil, errors.New("--telephony-dir is required")
	}
	return unifiedarchive.Open(options.dir, unified.telephonyDir)
}

func archiveUnifiedMetaCmd(options *archiveFlags, unified *unifiedArchiveFlags) *cobra.Command {
	return &cobra.Command{Use: "meta", Short: "Summarize the dynamic canonical view", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dataset, err := openUnifiedArchive(options, unified)
		if err != nil {
			return err
		}
		result := dataset.Result()
		if flags.jsonOut {
			return output.JSON(os.Stdout, result)
		}
		fmt.Printf("conversations:             %d\nmessages:                  %d\nrelay source messages:     %d\nTelephony source messages: %d\ncross-source matches:      %d\n", result.Conversations, result.Messages, result.RelaySourceMessages, result.TelephonySourceMessages, result.CrossSourceMatches)
		return nil
	}}
}

type unifiedConversationPage struct {
	Conversations []unifiedarchive.Conversation `json:"conversations"`
	Total         int                           `json:"total"`
	Limit         int                           `json:"limit"`
	Offset        int                           `json:"offset"`
}

func archiveUnifiedConversationsCmd(options *archiveFlags, unified *unifiedArchiveFlags) *cobra.Command {
	var limit, offset int
	var sortOrder string
	c := &cobra.Command{Use: "conversations [query]", Short: "List canonical conversations computed from both sources", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if limit <= 0 || offset < 0 {
			return errors.New("--limit must be positive and --offset must be non-negative")
		}
		if sortOrder != "recent" && sortOrder != "messages" {
			return fmt.Errorf("unsupported --sort %q (want recent or messages)", sortOrder)
		}
		dataset, err := openUnifiedArchive(options, unified)
		if err != nil {
			return err
		}
		values := dataset.Conversations()
		query := ""
		if len(args) == 1 {
			query = strings.ToLower(args[0])
		}
		if query != "" {
			filtered := values[:0]
			for _, conversation := range values {
				haystack := strings.ToLower(conversation.CanonicalConversationID + " " + conversation.Name)
				for _, participant := range conversation.Participants {
					haystack += " " + strings.ToLower(participant.Name+" "+participant.E164)
				}
				if strings.Contains(haystack, query) {
					filtered = append(filtered, conversation)
				}
			}
			values = filtered
		}
		sort.SliceStable(values, func(i, j int) bool {
			if sortOrder == "messages" {
				if values[i].Messages != values[j].Messages {
					return values[i].Messages > values[j].Messages
				}
			} else if values[i].LastMessageMS != values[j].LastMessageMS {
				return values[i].LastMessageMS > values[j].LastMessageMS
			}
			return values[i].CanonicalConversationID < values[j].CanonicalConversationID
		})
		total := len(values)
		if offset > total {
			offset = total
		}
		end := offset + limit
		if end > total {
			end = total
		}
		page := unifiedConversationPage{Conversations: values[offset:end], Total: total, Limit: limit, Offset: offset}
		if flags.jsonOut {
			return output.JSON(os.Stdout, page)
		}
		rows := make([][]string, 0, len(page.Conversations))
		for _, conversation := range page.Conversations {
			participants := make([]string, 0, len(conversation.Participants))
			for _, participant := range conversation.Participants {
				label := participant.Name
				if label == "" {
					label = participant.E164
				}
				participants = append(participants, label)
			}
			rows = append(rows, []string{output.FormatTime(conversation.LastMessageMS), fmt.Sprint(conversation.Messages), fmt.Sprint(conversation.RelaySourceMessages), fmt.Sprint(conversation.TelephonySourceMessages), truncate(strings.Join(participants, ", "), 38), truncate(conversation.Name, 36), conversation.CanonicalConversationID})
		}
		if len(rows) == 0 {
			fmt.Fprintln(os.Stderr, "(no conversations)")
			return nil
		}
		return output.Table(os.Stdout, []string{"last_msg", "messages", "relay", "android", "participants", "name", "canonical_id"}, rows)
	}}
	c.Flags().IntVar(&limit, "limit", 100, "max rows")
	c.Flags().IntVar(&offset, "offset", 0, "rows to skip")
	c.Flags().StringVar(&sortOrder, "sort", "recent", "sort order: recent or messages")
	return c
}

type unifiedMessagePage struct {
	CanonicalConversationID string                   `json:"canonical_conversation_id"`
	Messages                []unifiedarchive.Message `json:"messages"`
	Total                   int                      `json:"total"`
	Limit                   int                      `json:"limit"`
	Offset                  int                      `json:"offset"`
}

func archiveUnifiedMessagesCmd(options *archiveFlags, unified *unifiedArchiveFlags) *cobra.Command {
	var limit, offset int
	var sinceText, untilText string
	c := &cobra.Command{Use: "messages <canonical-conversation-id>", Short: "List dynamically unified messages with source provenance", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if limit <= 0 || offset < 0 {
			return errors.New("--limit must be positive and --offset must be non-negative")
		}
		since, err := parseFlagTime(sinceText)
		if err != nil {
			return err
		}
		until, err := parseFlagTime(untilText)
		if err != nil {
			return err
		}
		dataset, err := openUnifiedArchive(options, unified)
		if err != nil {
			return err
		}
		messages, ok := dataset.Messages(args[0])
		if !ok {
			return fmt.Errorf("canonical conversation %q not found", args[0])
		}
		filtered := messages[:0]
		for _, message := range messages {
			when := time.UnixMilli(message.TimestampMS)
			if !since.IsZero() && when.Before(since) {
				continue
			}
			if !until.IsZero() && !when.Before(until) {
				continue
			}
			filtered = append(filtered, message)
		}
		total := len(filtered)
		if offset > total {
			offset = total
		}
		end := offset + limit
		if end > total {
			end = total
		}
		page := unifiedMessagePage{CanonicalConversationID: args[0], Messages: filtered[offset:end], Total: total, Limit: limit, Offset: offset}
		if flags.jsonOut {
			return output.JSON(os.Stdout, page)
		}
		rows := make([][]string, 0, len(page.Messages))
		for _, message := range page.Messages {
			direction := "<-"
			if message.IsFromMe {
				direction = "->"
			}
			body := ""
			if message.Body != nil {
				body = *message.Body
			}
			platforms := make([]string, 0, len(message.Sources))
			for _, source := range message.Sources {
				platforms = append(platforms, source.Platform)
			}
			rows = append(rows, []string{output.FormatTime(message.TimestampMS), direction, strings.Join(platforms, "+"), message.UnifiedMessageID, truncate(body, 96)})
		}
		return output.Table(os.Stdout, []string{"time", "dir", "sources", "message_id", "body"}, rows)
	}}
	c.Flags().IntVar(&limit, "limit", 200, "max messages")
	c.Flags().IntVar(&offset, "offset", 0, "messages to skip")
	c.Flags().StringVar(&sinceText, "since", "", "lower time bound (YYYY-MM-DD or RFC3339)")
	c.Flags().StringVar(&untilText, "until", "", "exclusive upper time bound (YYYY-MM-DD or RFC3339)")
	return c
}

func openArchive(cmd *cobra.Command, options *archiveFlags) (*archiveview.Archive, error) {
	if options.dir == "" {
		return nil, errors.New("--dir is required")
	}
	return archiveview.Open(cmd.Context(), options.dir, archiveview.OpenOptions{CachePath: options.cachePath, Rebuild: options.rebuild})
}

func archiveMetaCmd(options *archiveFlags) *cobra.Command {
	return &cobra.Command{Use: "meta", Short: "Show archive and cache metadata", RunE: func(cmd *cobra.Command, _ []string) error {
		archive, err := openArchive(cmd, options)
		if err != nil {
			return err
		}
		defer archive.Close()
		meta, err := archive.Metadata(cmd.Context())
		if err != nil {
			return err
		}
		if flags.jsonOut {
			return output.JSON(os.Stdout, meta)
		}
		fmt.Printf("format_version: %d\nexported_at:    %s\nconversations:  %d\nmessages:       %d\ncache:          %s\n", meta.FormatVersion, meta.ExportedAt.Format("2006-01-02 15:04:05Z07:00"), meta.Conversations, meta.Messages, meta.CachePath)
		return nil
	}}
}

func archiveConversationsCmd(options *archiveFlags) *cobra.Command {
	var limit, offset int
	var sortOrder string
	c := &cobra.Command{Use: "conversations [query]", Short: "List or filter archived conversations", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		archive, err := openArchive(cmd, options)
		if err != nil {
			return err
		}
		defer archive.Close()
		query := ""
		if len(args) == 1 {
			query = args[0]
		}
		page, err := archive.ListConversations(cmd.Context(), archiveview.ConversationQuery{Query: query, Sort: archiveview.ConversationSort(sortOrder), Limit: limit, Offset: offset})
		if err != nil {
			return err
		}
		if flags.jsonOut {
			return output.JSON(os.Stdout, page)
		}
		rows := make([][]string, 0, len(page.Conversations))
		for _, conversation := range page.Conversations {
			kind := "1:1"
			if conversation.IsGroup {
				kind = "grp"
			}
			rows = append(rows, []string{output.FormatTime(conversation.LastMessageTimeMS), kind, fmt.Sprint(conversation.MessageCount), truncate(archiveParticipantSummary(conversation.Participants), 32), truncate(conversation.Name, 36), conversation.ID})
		}
		if len(rows) == 0 {
			fmt.Fprintln(os.Stderr, "(no conversations)")
			return nil
		}
		return output.Table(os.Stdout, []string{"last_msg", "kind", "messages", "participants", "name", "conv_id"}, rows)
	}}
	c.Flags().IntVar(&limit, "limit", 100, "max rows")
	c.Flags().IntVar(&offset, "offset", 0, "rows to skip")
	c.Flags().StringVar(&sortOrder, "sort", string(archiveview.ConversationSortRecent), "sort order: recent or messages")
	return c
}

func archiveMessagesCmd(options *archiveFlags) *cobra.Command {
	var before, after string
	var limit int
	c := &cobra.Command{Use: "messages <conversation-id>", Short: "Page through one conversation chronologically", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		archive, err := openArchive(cmd, options)
		if err != nil {
			return err
		}
		defer archive.Close()
		page, err := archive.ListMessages(cmd.Context(), args[0], archiveview.MessageQuery{Before: archiveview.Cursor(before), After: archiveview.Cursor(after), Limit: limit})
		if err != nil {
			return err
		}
		if flags.jsonOut {
			return output.JSON(os.Stdout, page)
		}
		if err := renderArchiveMessages(page.Messages); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "before_cursor=%s after_cursor=%s has_older=%v has_newer=%v\n", page.BeforeCursor, page.AfterCursor, page.HasOlder, page.HasNewer)
		return nil
	}}
	c.Flags().StringVar(&before, "before", "", "exclusive opaque cursor for older messages")
	c.Flags().StringVar(&after, "after", "", "exclusive opaque cursor for newer messages")
	c.Flags().IntVar(&limit, "limit", 200, "max messages")
	return c
}

func archiveSearchCmd(options *archiveFlags) *cobra.Command {
	var conversationID string
	var limit, offset int
	c := &cobra.Command{Use: "search <fts-query>", Short: "Full-text search archived message bodies", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		archive, err := openArchive(cmd, options)
		if err != nil {
			return err
		}
		defer archive.Close()
		page, err := archive.SearchMessages(cmd.Context(), archiveview.SearchQuery{Query: strings.Join(args, " "), ConversationID: conversationID, Limit: limit, Offset: offset})
		if err != nil {
			return err
		}
		if flags.jsonOut {
			return output.JSON(os.Stdout, page)
		}
		rows := make([][]string, 0, len(page.Hits))
		for _, hit := range page.Hits {
			direction := "<-"
			if hit.Message.IsFromMe {
				direction = "->"
			}
			body := ""
			if hit.Message.Body != nil {
				body = *hit.Message.Body
			}
			rows = append(rows, []string{output.FormatTime(hit.Message.TimestampMS), direction, hit.Message.ID, hit.ConversationID, truncate(body, 90)})
		}
		if len(rows) == 0 {
			fmt.Fprintln(os.Stderr, "(no matches)")
			return nil
		}
		return output.Table(os.Stdout, []string{"time", "dir", "msg_id", "conv_id", "body"}, rows)
	}}
	c.Flags().StringVar(&conversationID, "conversation", "", "restrict search to one conversation ID")
	c.Flags().IntVar(&limit, "limit", 100, "max results")
	c.Flags().IntVar(&offset, "offset", 0, "results to skip")
	return c
}

func archiveContextCmd(options *archiveFlags) *cobra.Command {
	var before, after int
	c := &cobra.Command{Use: "context <conversation-id> <message-id>", Short: "Show messages surrounding an exact archived message", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		archive, err := openArchive(cmd, options)
		if err != nil {
			return err
		}
		defer archive.Close()
		result, err := archive.MessageContext(cmd.Context(), args[0], args[1], archiveview.ContextQuery{Before: before, After: after})
		if err != nil {
			return err
		}
		if flags.jsonOut {
			return output.JSON(os.Stdout, result)
		}
		return renderArchiveMessages(result.Messages)
	}}
	c.Flags().IntVar(&before, "before", 20, "messages before the target")
	c.Flags().IntVar(&after, "after", 20, "messages after the target")
	return c
}

func renderArchiveMessages(messages []archiveview.Message) error {
	if len(messages) == 0 {
		fmt.Fprintln(os.Stderr, "(no messages)")
		return nil
	}
	rows := make([][]string, 0, len(messages))
	for _, message := range messages {
		direction := "<-"
		if message.IsFromMe {
			direction = "->"
		}
		body := ""
		if message.Body != nil {
			body = *message.Body
		}
		if body == "" && message.MimeType != nil {
			body = "[" + *message.MimeType + "]"
		}
		rows = append(rows, []string{output.FormatTime(message.TimestampMS), direction, truncate(message.SenderName, 24), message.ID, truncate(body, 96)})
	}
	return output.Table(os.Stdout, []string{"time", "dir", "sender", "msg_id", "body"}, rows)
}

func archiveParticipantSummary(participants []archiveview.Participant) string {
	var names []string
	for _, participant := range participants {
		if participant.IsMe {
			continue
		}
		name := participant.Name
		if name == "" {
			name = participant.FormattedNumber
		}
		if name == "" {
			name = participant.E164
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}
