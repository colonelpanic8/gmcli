package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/mdp/qrterminal/v3"
	"github.com/spf13/cobra"
	"rsc.io/qr"

	"github.com/fdsouvenir/gmcli/internal/gm"
)

func authCmd() *cobra.Command {
	var qrPNG string
	var method string
	var cookiesFile string
	c := &cobra.Command{
		Use:   "auth",
		Short: "Pair with Google Messages",
		Long: "Run once to pair gmcli with your phone. Google Account pairing prompts you to " +
			"tap a matching emoji on the phone and requires messages.google.com cookies supplied " +
			"as a JSON object. QR pairing remains available where Google Messages still supports it. " +
			"The session is saved to $STORE/session.json (mode 0600).",
		RunE: func(cmd *cobra.Command, args []string) error {
			layout, err := resolveLayout()
			if err != nil {
				return err
			}
			logger := newLogger()
			ctx, cancel := signalContext(context.Background())
			defer cancel()

			var res *gm.PairResult
			switch method {
			case "qr":
				fmt.Fprintln(os.Stderr, "Requesting QR pairing token from Google...")
				res, err = gm.Pair(ctx, layout, logger, func(qrURL string) {
					if qrPNG != "" {
						if err := writeQRPNG(qrPNG, qrURL); err != nil {
							fmt.Fprintf(os.Stderr, "Failed to write QR PNG: %v\n", err)
						} else {
							fmt.Fprintf(os.Stderr, "Wrote QR PNG: %s\n", qrPNG)
						}
					}
					fmt.Fprintln(os.Stderr, "Scan this QR code from Google Messages -> Device pairing:")
					fmt.Fprintln(os.Stderr)
					qrterminal.GenerateHalfBlock(qrURL, qrterminal.L, os.Stderr)
					fmt.Fprintln(os.Stderr)
					fmt.Fprintln(os.Stderr, "Or paste this URL into a QR generator:")
					fmt.Fprintln(os.Stderr, "  ", qrURL)
					fmt.Fprintln(os.Stderr)
					fmt.Fprintln(os.Stderr, "Waiting for pairing... (Ctrl-C to cancel)")
				})
			case "google":
				var cookies map[string]string
				cookies, err = readGoogleCookies(cookiesFile, os.Stdin)
				if err != nil {
					return err
				}
				fmt.Fprintln(os.Stderr, "Starting Google Account pairing...")
				res, err = gm.PairGoogle(ctx, layout, logger, cookies, func(emoji string) {
					fmt.Fprintln(os.Stderr, "Google Messages is asking for verification.")
					fmt.Fprintf(os.Stderr, "Tap this emoji on your phone: %s\n", emoji)
					fmt.Fprintln(os.Stderr, "Waiting for confirmation... (Ctrl-C to cancel)")
				})
			default:
				return fmt.Errorf("unknown auth method %q (want google or qr)", method)
			}
			if err != nil {
				return fmt.Errorf("pair: %w", err)
			}
			fmt.Fprintf(os.Stderr, "Paired. phone_id=%s session=%s\n", res.PhoneID, res.SessionPath)
			return nil
		},
	}
	c.Flags().StringVar(&method, "method", "google", "pairing method: google or qr")
	c.Flags().StringVar(&cookiesFile, "cookies-file", "", "Google cookie JSON file; use - to read from stdin (required for --method google)")
	c.Flags().StringVar(&qrPNG, "qr-png", "", "write pairing QR code to a PNG file")
	return c
}

var requiredGoogleCookies = []string{"SID", "HSID", "OSID", "SSID", "APISID", "SAPISID"}

func readGoogleCookies(path string, stdin io.Reader) (map[string]string, error) {
	if path == "" {
		return nil, fmt.Errorf("--cookies-file is required for Google Account pairing; use - to read JSON from stdin")
	}
	var r io.Reader
	if path == "-" {
		r = stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open cookies file: %w", err)
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return nil, fmt.Errorf("inspect cookies file: %w", err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("cookies file %s has permissions %04o; run `chmod 600 %s` or use --cookies-file -", path, info.Mode().Perm(), path)
		}
		r = f
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read cookie JSON: %w", err)
	}
	var cookies map[string]string
	if err := json.Unmarshal(data, &cookies); err != nil {
		var exported []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if arrayErr := json.Unmarshal(data, &exported); arrayErr != nil {
			return nil, fmt.Errorf("decode cookie JSON: expected an object of string values or an array of name/value objects")
		}
		cookies = make(map[string]string, len(exported))
		for _, cookie := range exported {
			if cookie.Name != "" {
				cookies[cookie.Name] = cookie.Value
			}
		}
	}
	missing := make([]string, 0)
	for _, name := range requiredGoogleCookies {
		if strings.TrimSpace(cookies[name]) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, fmt.Errorf("cookie JSON is missing required cookies: %s", strings.Join(missing, ", "))
	}
	return cookies, nil
}

func writeQRPNG(path, text string) error {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return fmt.Errorf("encode QR: %w", err)
	}
	if err := os.WriteFile(path, code.PNG(), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
