package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/fdsouvenir/gmcli/internal/archive"
	"github.com/fdsouvenir/gmcli/internal/output"
)

func exportCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "export",
		Short: "Export the local archive to portable formats",
	}
	c.AddCommand(exportJSONCmd(), exportJSONLCmd(), exportVerifyCmd())
	return c
}

func exportVerifyCmd() *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use:   "verify",
		Short: "Verify a segmented JSONL archive",
		Long: "Validates manifest metadata, SHA-256 checksums, JSON syntax, record counts, " +
			"safe paths, and per-conversation message ownership.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if dir == "" {
				return fmt.Errorf("--dir is required")
			}
			result, err := archive.VerifyJSONL(dir)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return output.JSON(os.Stdout, result)
			}
			fmt.Fprintf(os.Stderr, "Verified %d conversations, %d messages, %d contacts, and %d aliases in %s\n",
				result.Conversations, result.Messages, result.Contacts, result.Aliases, result.Path)
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "JSONL archive directory to verify (required)")
	return c
}

func exportJSONLCmd() *cobra.Command {
	var out string
	var force bool
	c := &cobra.Command{
		Use:   "jsonl",
		Short: "Export a segmented, line-oriented archive directory",
		Long: "Writes conversations.jsonl, keyed contacts.json, aliases.json, and coverage.json lookups, one messages/*.jsonl " +
			"file per conversation, and a checksummed manifest.json into one private directory. " +
			"Each conversation and message JSONL line is an independent JSON object.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "" {
				return fmt.Errorf("--out is required")
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			result, err := archive.WriteJSONL(cmd.Context(), st, out, force)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return output.JSON(os.Stdout, result)
			}
			fmt.Fprintf(os.Stderr, "Exported %d conversations, %d messages, %d contacts, and %d aliases as a segmented archive to %s\n",
				result.Conversations, result.Messages, result.Contacts, result.Aliases, result.Path)
			return nil
		},
	}
	c.Flags().StringVar(&out, "out", "", "destination directory (required)")
	c.Flags().BoolVar(&force, "force", false, "replace an existing output directory")
	return c
}

func exportJSONCmd() *cobra.Command {
	var out string
	var force bool
	c := &cobra.Command{
		Use:   "json",
		Short: "Export conversations, messages, contacts, and aliases as JSON",
		Long: "Writes one portable JSON document containing the complete local archive. " +
			"The export excludes media decryption keys and raw protocol buffers; downloaded media files are not embedded.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "" {
				return fmt.Errorf("--out is required")
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			result, err := archive.WriteJSON(cmd.Context(), st, out, force)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return output.JSON(os.Stdout, result)
			}
			fmt.Fprintf(os.Stderr, "Exported %d conversations, %d messages, %d contacts, and %d aliases to %s\n",
				result.Conversations, result.Messages, result.Contacts, result.Aliases, result.Path)
			return nil
		},
	}
	c.Flags().StringVar(&out, "out", "", "destination JSON file (required)")
	c.Flags().BoolVar(&force, "force", false, "replace an existing output file")
	return c
}
