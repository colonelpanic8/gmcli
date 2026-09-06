package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fdsouvenir/gmcli/internal/store"
	gmsync "github.com/fdsouvenir/gmcli/internal/sync"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

type failedSyncClient struct {
	contactError, folderError, messageError error
	opaque                                  bool
}

func (f failedSyncClient) ListContacts() (*gmproto.ListContactsResponse, error) {
	return &gmproto.ListContactsResponse{}, f.contactError
}
func (f failedSyncClient) ListConversationsWithCursor(int, gmproto.ListConversationsRequest_Folder, *gmproto.Cursor) (*gmproto.ListConversationsResponse, error) {
	r := &gmproto.ListConversationsResponse{Conversations: []*gmproto.Conversation{{ConversationID: "1"}}}
	if f.opaque {
		r.CursorBytes = []byte("unusable")
	}
	return r, f.folderError
}
func (f failedSyncClient) FetchMessages(string, int64, *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	return &gmproto.ListMessagesResponse{Messages: []*gmproto.Message{{MessageID: "1", ConversationID: "1"}}}, f.messageError
}

func TestInitialSyncReportsRetrievalAndStorageFailures(t *testing.T) {
	for _, test := range []struct {
		name, want    string
		client        failedSyncClient
		breakMessages bool
	}{
		{name: "healthy empty contacts", client: failedSyncClient{}},
		{name: "contacts fail", client: failedSyncClient{contactError: errors.New("HTTP 404")}, want: "contact import"},
		{name: "inbox fails", client: failedSyncClient{folderError: errors.New("HTTP 404")}, want: "INBOX discovery failed"},
		{name: "partial discovery", client: failedSyncClient{opaque: true}, want: "opaque_cursor"},
		{name: "messages fail", client: failedSyncClient{messageError: errors.New("offline")}, want: "recent messages"},
		{name: "storage fails", breakMessages: true, want: "disk full"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := store.Open(ctx, filepath.Join(t.TempDir(), "gmcli.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if test.breakMessages {
				if _, err := st.DB().Exec(`CREATE TRIGGER fail_message BEFORE INSERT ON messages BEGIN SELECT RAISE(FAIL, 'disk full'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err = runInitialSync(ctx, test.client, st, gmsync.New(st, zerolog.Nop()), zerolog.Nop(), 100, false, false)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want %s", err, test.want)
			}
			folders, err := st.ListFolderCoverage(ctx)
			if err != nil || len(folders) != 1 {
				t.Fatalf("coverage: %v, %v", folders, err)
			}
			if test.client.folderError != nil && folders[0].Status != store.CoverageFailed {
				t.Fatalf("failure not recorded: %+v", folders[0])
			}
		})
	}
}
