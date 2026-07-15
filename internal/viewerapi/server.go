// Package viewerapi exposes the renderer-independent archive query surface as
// a read-only HTTP API for desktop, web, and other headless clients.
package viewerapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/fdsouvenir/gmcli/internal/viewer"
)

// Options controls access to the local archive API.
type Options struct {
	BearerToken string
}

// New returns a read-only HTTP handler backed by source.
func New(source viewer.Source, options Options) http.Handler {
	api := &server{source: source, token: options.BearerToken}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", api.health)
	mux.HandleFunc("GET /api/v1/meta", api.metadata)
	mux.HandleFunc("GET /api/v1/conversations", api.conversations)
	mux.HandleFunc("GET /api/v1/conversations/{conversation_id}", api.conversation)
	mux.HandleFunc("GET /api/v1/conversations/{conversation_id}/messages", api.messages)
	mux.HandleFunc("GET /api/v1/conversations/{conversation_id}/messages/{message_id}/context", api.context)
	mux.HandleFunc("GET /api/v1/search", api.search)
	return api.secure(mux)
}

type server struct {
	source viewer.Source
	token  string
}

type errorResponse struct {
	Error string `json:"error"`
}

func (s *server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if s.token != "" {
			authorization := r.Header.Get("Authorization")
			provided, bearer := strings.CutPrefix(authorization, "Bearer ")
			if !bearer || len(provided) != len(s.token) || subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) != 1 {
				writeError(w, http.StatusUnauthorized, errors.New("missing or invalid bearer token"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) metadata(w http.ResponseWriter, r *http.Request) {
	value, err := s.source.Metadata(r.Context())
	writeResult(w, value, err)
}

func (s *server) conversations(w http.ResponseWriter, r *http.Request) {
	sortOrder := viewer.ConversationSort(r.URL.Query().Get("sort"))
	if sortOrder != "" && sortOrder != viewer.ConversationSortRecent && sortOrder != viewer.ConversationSortMessages {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported sort %q (want recent or messages)", sortOrder))
		return
	}
	limit, err := optionalInt(r, "limit")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	offset, err := optionalInt(r, "offset")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	value, err := s.source.ListConversations(r.Context(), viewer.ConversationQuery{
		Query: r.URL.Query().Get("query"), Sort: sortOrder, Limit: limit, Offset: offset,
	})
	writeResult(w, value, err)
}

func (s *server) conversation(w http.ResponseWriter, r *http.Request) {
	value, err := s.source.GetConversation(r.Context(), r.PathValue("conversation_id"))
	writeResult(w, value, err)
}

func (s *server) messages(w http.ResponseWriter, r *http.Request) {
	limit, err := optionalInt(r, "limit")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	before, after := viewer.Cursor(r.URL.Query().Get("before")), viewer.Cursor(r.URL.Query().Get("after"))
	if before != "" && after != "" {
		writeError(w, http.StatusBadRequest, errors.New("before and after are mutually exclusive"))
		return
	}
	value, err := s.source.ListMessages(r.Context(), r.PathValue("conversation_id"), viewer.MessageQuery{
		Before: before,
		After:  after,
		Limit:  limit,
	})
	writeResult(w, value, err)
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	limit, err := optionalInt(r, "limit")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	offset, err := optionalInt(r, "offset")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	value, err := s.source.SearchMessages(r.Context(), viewer.SearchQuery{
		Query:          r.URL.Query().Get("query"),
		ConversationID: r.URL.Query().Get("conversation_id"),
		Limit:          limit,
		Offset:         offset,
	})
	writeResult(w, value, err)
}

func (s *server) context(w http.ResponseWriter, r *http.Request) {
	before, err := optionalInt(r, "before")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	after, err := optionalInt(r, "after")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	value, err := s.source.MessageContext(r.Context(), r.PathValue("conversation_id"), r.PathValue("message_id"), viewer.ContextQuery{Before: before, After: after})
	writeResult(w, value, err)
}

func optionalInt(r *http.Request, name string) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return value, nil
}

func writeResult(w http.ResponseWriter, value any, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, value)
		return
	}
	status := http.StatusInternalServerError
	if errors.Is(err, viewer.ErrNotFound) {
		status = http.StatusNotFound
	} else if errors.Is(err, viewer.ErrInvalidCursor) {
		status = http.StatusBadRequest
	}
	writeError(w, status, err)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorResponse{Error: err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
