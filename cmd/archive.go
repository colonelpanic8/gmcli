package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/fdsouvenir/gmcli/internal/output"
	archiveview "github.com/fdsouvenir/gmcli/internal/viewer"
)

type archiveFlags struct {
	dir       string
	cachePath string
	rebuild   bool
}

func archiveCmd() *cobra.Command {
	var options archiveFlags
	c := &cobra.Command{Use: "archive", Short: "Query portable JSONL archives"}
	c.PersistentFlags().StringVar(&options.dir, "dir", "", "authoritative JSONL archive directory (required)")
	c.PersistentFlags().StringVar(&options.cachePath, "cache", "", "SQLite cache path (default: $XDG_CACHE_HOME/gmcli/archives/<fingerprint>.sqlite)")
	c.PersistentFlags().BoolVar(&options.rebuild, "rebuild-cache", false, "discard and rebuild the disposable SQLite cache")
	c.AddCommand(archiveMetaCmd(&options), archiveConversationsCmd(&options), archiveMessagesCmd(&options), archiveSearchCmd(&options), archiveContextCmd(&options))
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
		page, err := archive.ListConversations(cmd.Context(), archiveview.ConversationQuery{Query: query, Limit: limit, Offset: offset})
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
