package viewerapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fdsouvenir/gmcli/internal/viewer"
)

type fakeSource struct {
	lastConversationQuery viewer.ConversationQuery
}

func (f *fakeSource) Metadata(context.Context) (viewer.Meta, error) {
	return viewer.Meta{Conversations: 2, Messages: 12}, nil
}
func (f *fakeSource) ListConversations(_ context.Context, query viewer.ConversationQuery) (viewer.ConversationPage, error) {
	f.lastConversationQuery = query
	return viewer.ConversationPage{Conversations: []viewer.Conversation{{ID: "chat-1", Name: "Ada"}}, Total: 1, Sort: query.Sort}, nil
}
func (f *fakeSource) GetConversation(_ context.Context, id string) (viewer.Conversation, error) {
	if id == "missing" {
		return viewer.Conversation{}, viewer.ErrNotFound
	}
	return viewer.Conversation{ID: id}, nil
}
func (f *fakeSource) ListMessages(_ context.Context, id string, query viewer.MessageQuery) (viewer.MessagePage, error) {
	return viewer.MessagePage{Conversation: viewer.Conversation{ID: id}, AfterCursor: query.After, BeforeCursor: query.Before}, nil
}
func (f *fakeSource) SearchMessages(_ context.Context, query viewer.SearchQuery) (viewer.SearchPage, error) {
	return viewer.SearchPage{Query: query.Query, Offset: query.Offset, Limit: query.Limit}, nil
}
func (f *fakeSource) MessageContext(_ context.Context, conversationID, messageID string, _ viewer.ContextQuery) (viewer.MessageContext, error) {
	return viewer.MessageContext{Conversation: viewer.Conversation{ID: conversationID}, TargetID: messageID}, nil
}

func TestConversationsEndpoint(t *testing.T) {
	source := &fakeSource{}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/conversations?query=ada&sort=messages&limit=25&offset=5", nil)
	response := httptest.NewRecorder()
	New(source, Options{}).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if source.lastConversationQuery != (viewer.ConversationQuery{Query: "ada", Sort: viewer.ConversationSortMessages, Limit: 25, Offset: 5}) {
		t.Fatalf("query = %#v", source.lastConversationQuery)
	}
	var page viewer.ConversationPage
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Conversations) != 1 || page.Conversations[0].ID != "chat-1" {
		t.Fatalf("page = %#v", page)
	}
}

func TestBearerTokenAndSecurityHeaders(t *testing.T) {
	handler := New(&fakeSource{}, Options{BearerToken: "secret"})
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", unauthorized.Code)
	}
	wrongSchemeRequest := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	wrongSchemeRequest.Header.Set("Authorization", "Basic secret")
	wrongScheme := httptest.NewRecorder()
	handler.ServeHTTP(wrongScheme, wrongSchemeRequest)
	if wrongScheme.Code != http.StatusUnauthorized {
		t.Fatalf("wrong scheme status = %d", wrongScheme.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status = %d, headers = %#v", response.Code, response.Header())
	}
}

func TestInvalidParametersAndNotFound(t *testing.T) {
	handler := New(&fakeSource{}, Options{})
	for _, test := range []struct {
		path string
		want int
	}{
		{"/api/v1/conversations?sort=unknown", http.StatusBadRequest},
		{"/api/v1/conversations?limit=-1", http.StatusBadRequest},
		{"/api/v1/conversations/missing", http.StatusNotFound},
		{"/api/v1/conversations/chat/messages?before=one&after=two", http.StatusBadRequest},
		{"/api/v1/conversations/chat/messages?before=bad", http.StatusOK},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.want {
			t.Errorf("%s: status = %d, want %d", test.path, response.Code, test.want)
		}
	}
}
