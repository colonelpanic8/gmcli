package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/fdsouvenir/gmcli/internal/androidtelephony"
	"github.com/fdsouvenir/gmcli/internal/hiddenfolders"
	"github.com/fdsouvenir/gmcli/internal/output"
)

func androidCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "android",
		Short: "Read data directly from a connected Android device",
	}
	c.AddCommand(androidExportTelephonyCmd(), androidVerifyTelephonyCmd(), androidVerifyHiddenFoldersCmd())
	return c
}

func androidVerifyHiddenFoldersCmd() *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use:   "verify-hidden-folders",
		Short: "Verify a supplemental Android Messages hidden-folder audit",
		Long: "Rechecks the audit manifest, safe unique paths and conversation IDs, exact byte sizes, " +
			"SHA-256 checksums, JSONL syntax and counts, record versions and types, and per-conversation ownership.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dir == "" {
				return fmt.Errorf("--dir is required")
			}
			result, err := hiddenfolders.Verify(dir)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return output.JSON(os.Stdout, result)
			}
			fmt.Fprintf(os.Stderr, "Verified %d hidden-folder conversations and %d JSONL records in %s\n",
				result.Conversations, result.Records, result.Path)
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "hidden-folder audit directory to verify (required)")
	return c
}

func androidVerifyTelephonyCmd() *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use:   "verify-telephony",
		Short: "Verify a segmented Android Telephony archive",
		Long: "Rechecks safe paths, private permissions, file completeness, byte sizes, SHA-256 checksums, " +
			"JSONL syntax and counts, per-thread ownership, and every content-addressed MMS media reference.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if dir == "" {
				return fmt.Errorf("--dir is required")
			}
			result, err := androidtelephony.Verify(dir)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return output.JSON(os.Stdout, result)
			}
			fmt.Fprintf(os.Stderr, "Verified %d Telephony threads, %d JSONL records, and %d MMS media files in %s\n",
				result.Threads, result.Records, result.MediaFiles, result.Path)
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "Telephony archive directory to verify (required)")
	return c
}

func androidExportTelephonyCmd() *cobra.Command {
	var out, adb, serial string
	var force, includePartData bool
	c := &cobra.Command{
		Use:   "export-telephony",
		Short: "Losslessly export the Android SMS/MMS provider over adb",
		Long: "Runs a bundled read-only ContentResolver helper under Android's shell UID. " +
			"The result has one JSONL file per Telephony thread, complete type-tagged provider rows, " +
			"MMS participants, canonical addresses, and content-addressed MMS attachments with SHA-256 checksums.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "" {
				return fmt.Errorf("--out is required")
			}
			result, err := androidtelephony.Export(cmd.Context(), androidtelephony.Options{
				ADB:             adb,
				Serial:          serial,
				OutputDirectory: out,
				Force:           force,
				IncludePartData: includePartData,
			})
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return output.JSON(os.Stdout, result)
			}
			fmt.Fprintf(os.Stderr, "Exported %d Telephony threads, %d JSONL records, and %d unique MMS media files to %s\n",
				result.Threads, result.Records, result.MediaFiles, result.Path)
			return nil
		},
	}
	c.Flags().StringVar(&out, "out", "", "destination archive directory (required)")
	c.Flags().StringVar(&adb, "adb", "adb", "adb executable")
	c.Flags().StringVar(&serial, "serial", "", "adb device serial (auto-detected when exactly one device is connected)")
	c.Flags().BoolVar(&force, "force", false, "atomically replace an existing output directory")
	c.Flags().BoolVar(&includePartData, "include-part-data", true, "include and verify all binary MMS attachments")
	return c
}
