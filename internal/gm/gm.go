// Package gm wraps go.mau.fi/mautrix-gmessages/pkg/libgm with the conventions
// gmcli needs: filesystem-backed AuthData persistence, an event subscriber
// model on top of libgm's single SetEventHandler, and helpers for the QR and
// Google Account pairing flows.
//
// Two entry points cover the lifecycle:
//
//	Pair(ctx, layout, render)         // QR first run: produces session.json
//	PairGoogle(ctx, layout, cookies)  // account/emoji first run
//	Open(layout, logger) -> *Client   // subsequent runs: ready to Connect()
//
// The wrapper does not own a goroutine of its own; libgm runs the long-poll.
// Subscribers must not block in their handlers.
package gm

import (
	"context"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"

	"github.com/fdsouvenir/gmcli/internal/paths"
)

// PairTimeout is the upper bound on how long we wait for a phone to scan the
// QR code. Google's relay drops unfinished pairings after a few minutes.
const PairTimeout = 5 * time.Minute

// sendMetadataTimeout bounds how long a write waits for the phone settings
// event that carries preferred SIM metadata before falling back to the legacy
// send request shape.
const sendMetadataTimeout = 10 * time.Second

// EventHandler is invoked for each event delivered by libgm. The argument
// type is one of the concrete types in pkg/libgm/events or pkg/libgm/gmproto.
type EventHandler func(evt any)

// SendMode identifies which Google Messages request shape was used.
type SendMode string

const (
	SendModeAuto     SendMode = "auto"
	SendModeSettings SendMode = "settings"
	SendModeLegacy   SendMode = "legacy"
)

// Client is a thin wrapper around *libgm.Client adding fan-out event
// subscription and persistence on AuthTokenRefreshed.
type Client struct {
	libgm  *libgm.Client
	auth   *libgm.AuthData
	layout paths.Layout
	logger zerolog.Logger

	mu          sync.RWMutex
	subscribers []EventHandler
	settings    *gmproto.Settings
	ready       bool

	sendMessageHook     func(*gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error)
	getConversationHook func(string) (*gmproto.Conversation, error)
	sendMetadataWait    time.Duration
}

// Open loads session.json and returns a connected-but-not-yet-Connect()'d
// Client. Returns an error if no session exists — the caller must run Pair
// first.
func Open(layout paths.Layout, logger zerolog.Logger) (*Client, error) {
	auth, err := loadAuth(layout.Session)
	if err != nil {
		return nil, err
	}
	if auth.Browser == nil {
		return nil, fmt.Errorf("session %s has no paired device; run `gmcli auth` first", layout.Session)
	}
	c := &Client{
		auth:   auth,
		layout: layout,
		logger: logger,
	}
	c.libgm = libgm.NewClient(auth, nil, logger)
	c.libgm.SetEventHandler(c.dispatch)
	return c, nil
}

// Subscribe registers a handler. Multiple subscribers receive each event in
// the order they were registered. Handlers must not block.
func (c *Client) Subscribe(h EventHandler) {
	c.mu.Lock()
	c.subscribers = append(c.subscribers, h)
	c.mu.Unlock()
}

// Connect opens the long-poll connection. Events flow to subscribers
// immediately. Returns when the initial sync completes; the connection
// continues running in a background goroutine inside libgm.
func (c *Client) Connect() error {
	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
	return c.libgm.Connect()
}

// Disconnect closes the long-poll and persists the final in-memory auth
// state. Safe to call multiple times. Call Close directly when the caller can
// surface a persistence error.
func (c *Client) Disconnect() {
	if err := c.Close(); err != nil {
		c.logger.Error().Err(err).Msg("Failed to persist auth data while disconnecting")
	}
}

// Close closes the long-poll and persists the final in-memory auth state.
// libgm updates Google Account cookies from HTTP responses without emitting an
// auth-refresh event, so saving at a clean command boundary is necessary for
// those rotated cookies to survive the next process start.
func (c *Client) Close() error {
	c.libgm.Disconnect()
	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
	if err := saveAuth(c.layout.Session, c.auth); err != nil {
		return fmt.Errorf("persist auth data while disconnecting: %w", err)
	}
	return nil
}

// IsConnected reports whether the long-poll is currently active.
func (c *Client) IsConnected() bool {
	return c.libgm.IsConnected()
}

// WaitForReady blocks until the libgm client emits *events.ClientReady or
// the context is cancelled. SendMessage and SendReaction need an established
// session before they can round-trip a response; ClientReady is the earliest
// signal that the session is up. The handler is removed before returning.
//
// Subscribe(c.WaitForReady...) is not the right idiom — this method
// installs and removes a single-fire subscriber for you.
func (c *Client) WaitForReady(ctx context.Context) error {
	c.mu.RLock()
	isReady := c.ready
	c.mu.RUnlock()
	if isReady {
		return nil
	}
	ready := make(chan struct{}, 1)
	var fired sync.Once

	c.mu.Lock()
	idx := len(c.subscribers)
	c.subscribers = append(c.subscribers, func(evt any) {
		if _, ok := evt.(*events.ClientReady); ok {
			fired.Do(func() { close(ready) })
		}
	})
	if c.ready {
		fired.Do(func() { close(ready) })
	}
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		// Remove only our subscriber; preserve any added concurrently.
		if idx < len(c.subscribers) {
			c.subscribers = append(c.subscribers[:idx], c.subscribers[idx+1:]...)
		}
		c.mu.Unlock()
	}()

	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Underlying returns the wrapped *libgm.Client for callers that need access
// to libgm methods we haven't surfaced yet (ListContacts, FetchMessages,
// etc.). Higher-level operations should prefer the typed wrappers below.
func (c *Client) Underlying() *libgm.Client { return c.libgm }

// SetSettings seeds the send metadata cache from persisted phone settings.
func (c *Client) SetSettings(settings *gmproto.Settings) {
	if settings == nil {
		return
	}
	cloned, ok := proto.Clone(settings).(*gmproto.Settings)
	if !ok {
		return
	}
	c.mu.Lock()
	c.settings = cloned
	c.mu.Unlock()
}

// RequestUpdates asks the phone for a fresh GET_UPDATES payload.
func (c *Client) RequestUpdates() error {
	return c.libgm.SetActiveSession()
}

// IsDefaultSMSApp asks the phone whether Google Messages is the default SMS app.
func (c *Client) IsDefaultSMSApp() (bool, error) {
	resp, err := c.libgm.IsBugleDefault()
	if err != nil {
		return false, err
	}
	return resp.GetSuccess(), nil
}

// SendTextResult describes a successful send.
type SendTextResult struct {
	MessageID      string
	ConversationID string
	TmpID          string
	SendMode       SendMode
}

// SendText sends a text message into the given conversation. ReplyToID is
// optional; when set, the new message is rendered as a quoted reply by the
// recipient's client. The libgm long-poll must be Connected; call
// WaitForReady first for fresh sessions.
func (c *Client) SendText(ctx context.Context, conversationID, body, replyToID string) (*SendTextResult, error) {
	return c.SendTextWithMode(ctx, conversationID, body, replyToID, SendModeAuto)
}

// SendTextWithMode is SendText with an explicit request-shape selection.
// SendModeAuto prefers Settings/SIM metadata, falls back to legacy when
// Settings are unavailable, and retries legacy when the phone rejects a
// settings-mode attempt with UNKNOWN.
func (c *Client) SendTextWithMode(ctx context.Context, conversationID, body, replyToID string, requested SendMode) (*SendTextResult, error) {
	if conversationID == "" {
		return nil, fmt.Errorf("conversation id is required")
	}
	if body == "" {
		return nil, fmt.Errorf("message body is required")
	}
	if !validRequestedSendMode(requested) {
		return nil, fmt.Errorf("unknown send mode %q", requested)
	}
	tmpID := uuid.NewString()
	req, mode, err := c.buildSendTextRequest(ctx, conversationID, body, replyToID, tmpID, requested)
	if err != nil {
		return nil, err
	}
	res, err := c.sendBuiltText(ctx, req, mode)
	if err == nil {
		return res, nil
	}
	var rejected sendRejectedError
	if requested == SendModeAuto && mode == SendModeSettings && errors.As(err, &rejected) && rejected.status == gmproto.SendMessageResponse_UNKNOWN {
		legacyReq := buildLegacySendTextRequest(conversationID, body, replyToID, uuid.NewString())
		legacyRes, legacyErr := c.sendBuiltText(ctx, legacyReq, SendModeLegacy)
		if legacyErr != nil {
			return nil, fmt.Errorf("%w; legacy fallback failed: %w", err, legacyErr)
		}
		return legacyRes, nil
	}
	return nil, err
}

func (c *Client) sendBuiltText(ctx context.Context, req *gmproto.SendMessageRequest, mode SendMode) (*SendTextResult, error) {
	waitEcho, unsubscribe := c.watchMessageEcho(req.GetTmpID())
	defer unsubscribe()

	resp, err := c.sendMessage(req)
	if err != nil {
		return nil, fmt.Errorf("libgm send: %w", err)
	}
	if resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
		return nil, sendRejectedError{
			status:  resp.GetStatus(),
			message: sendStatusMessage(resp),
		}
	}

	echo, err := waitEcho(ctx)
	if err != nil {
		return nil, fmt.Errorf("send accepted by phone, but no sent-message echo arrived for tmp_id %s: %w", req.GetTmpID(), err)
	}
	return &SendTextResult{
		MessageID:      echo.Message.GetMessageID(),
		ConversationID: echo.Message.GetConversationID(),
		TmpID:          req.GetTmpID(),
		SendMode:       mode,
	}, nil
}

type sendRejectedError struct {
	status  gmproto.SendMessageResponse_Status
	message string
}

func (e sendRejectedError) Error() string {
	return fmt.Sprintf("send rejected by phone: %s", e.message)
}

func validRequestedSendMode(mode SendMode) bool {
	switch mode {
	case SendModeAuto, SendModeSettings, SendModeLegacy:
		return true
	default:
		return false
	}
}

func (c *Client) buildSendTextRequest(ctx context.Context, conversationID, body, replyToID, tmpID string, requested SendMode) (*gmproto.SendMessageRequest, SendMode, error) {
	if requested == SendModeLegacy {
		return buildLegacySendTextRequest(conversationID, body, replyToID, tmpID), SendModeLegacy, nil
	}

	settingsCtx, cancel := context.WithTimeout(ctx, c.sendMetadataWaitDuration())
	defer cancel()
	if err := c.WaitForSettings(settingsCtx); err != nil {
		if ctx.Err() != nil {
			return nil, "", fmt.Errorf("wait for phone send settings: %w", err)
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			if requested == SendModeSettings {
				return nil, "", fmt.Errorf("wait for phone send settings: %w", err)
			}
			return buildLegacySendTextRequest(conversationID, body, replyToID, tmpID), SendModeLegacy, nil
		}
		return nil, "", fmt.Errorf("wait for phone send settings: %w", err)
	}

	req, err := c.buildSettingsSendTextRequest(conversationID, body, replyToID, tmpID)
	if err != nil {
		if requested == SendModeSettings {
			return nil, "", err
		}
		return nil, "", err
	}
	return req, SendModeSettings, nil
}

func (c *Client) buildSettingsSendTextRequest(conversationID, body, replyToID, tmpID string) (*gmproto.SendMessageRequest, error) {
	conv, err := c.getConversation(conversationID)
	if err != nil {
		return nil, fmt.Errorf("get conversation %s before send: %w", conversationID, err)
	}
	outgoingID := conv.GetDefaultOutgoingID()
	var sim *gmproto.SIMCard
	if outgoingID == "" {
		var err error
		outgoingID, sim, err = c.onlyUsableSIM()
		if err != nil {
			return nil, fmt.Errorf("conversation %s has no default outgoing participant: %w", conversationID, err)
		}
	} else {
		sim = c.simForParticipant(outgoingID)
		if sim == nil {
			return nil, fmt.Errorf("conversation %s uses outgoing participant %s, but no matching SIM metadata was received", conversationID, outgoingID)
		}
	}
	req := &gmproto.SendMessageRequest{
		ConversationID: conversationID,
		TmpID:          tmpID,
		ForceRCS:       forceRCSForConversation(conv, sim),
		MessagePayload: &gmproto.MessagePayload{
			ConversationID: conversationID,
			ParticipantID:  outgoingID,
			TmpID:          tmpID,
			TmpID2:         tmpID,
			MessageInfo: []*gmproto.MessageInfo{{
				Data: &gmproto.MessageInfo_MessageContent{
					MessageContent: &gmproto.MessageContent{Content: body},
				},
			}},
		},
	}
	req.SIMPayload = sim.GetSIMData().GetSIMPayload()
	if replyToID != "" {
		req.Reply = &gmproto.ReplyPayload{MessageID: replyToID}
	}
	return req, nil
}

func forceRCSForConversation(conv *gmproto.Conversation, sim *gmproto.SIMCard) bool {
	if !sim.GetRCSChats().GetEnabled() || conv.GetSendMode() != gmproto.ConversationSendMode_SEND_MODE_AUTO {
		return false
	}
	switch conv.GetType() {
	case gmproto.ConversationType_RCS, gmproto.ConversationType_UNKNOWN_CONVERSATION_TYPE:
		return true
	default:
		return false
	}
}

func buildLegacySendTextRequest(conversationID, body, replyToID, tmpID string) *gmproto.SendMessageRequest {
	req := &gmproto.SendMessageRequest{
		ConversationID: conversationID,
		TmpID:          tmpID,
		MessagePayload: &gmproto.MessagePayload{
			ConversationID: conversationID,
			TmpID:          tmpID,
			TmpID2:         tmpID,
			MessagePayloadContent: &gmproto.MessagePayloadContent{
				MessageContent: &gmproto.MessageContent{Content: body},
			},
		},
	}
	if replyToID != "" {
		req.Reply = &gmproto.ReplyPayload{MessageID: replyToID}
	}
	return req
}

func (c *Client) sendMessage(req *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
	if c.sendMessageHook != nil {
		return c.sendMessageHook(req)
	}
	return c.libgm.SendMessage(req)
}

func (c *Client) getConversation(conversationID string) (*gmproto.Conversation, error) {
	if c.getConversationHook != nil {
		return c.getConversationHook(conversationID)
	}
	return c.libgm.GetConversation(conversationID)
}

func (c *Client) sendMetadataWaitDuration() time.Duration {
	if c.sendMetadataWait > 0 {
		return c.sendMetadataWait
	}
	return sendMetadataTimeout
}

// WaitForSettings blocks until libgm emits the phone settings event. Send
// requests prefer its SIM metadata to match the browser client shape.
func (c *Client) WaitForSettings(ctx context.Context) error {
	c.mu.RLock()
	hasSettings := c.settings != nil
	c.mu.RUnlock()
	if hasSettings {
		return nil
	}

	ready := make(chan struct{}, 1)
	var fired sync.Once
	c.mu.Lock()
	idx := len(c.subscribers)
	c.subscribers = append(c.subscribers, func(evt any) {
		if _, ok := evt.(*gmproto.Settings); ok {
			fired.Do(func() { close(ready) })
		}
	})
	if c.settings != nil {
		fired.Do(func() { close(ready) })
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if idx < len(c.subscribers) {
			c.subscribers = append(c.subscribers[:idx], c.subscribers[idx+1:]...)
		}
		c.mu.Unlock()
	}()

	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) simForParticipant(participantID string) *gmproto.SIMCard {
	c.mu.RLock()
	settings := c.settings
	c.mu.RUnlock()
	if settings == nil {
		return nil
	}
	for _, sim := range settings.GetSIMCards() {
		if sim.GetSIMParticipant().GetID() == participantID {
			return sim
		}
	}
	return nil
}

func (c *Client) onlyUsableSIM() (string, *gmproto.SIMCard, error) {
	c.mu.RLock()
	settings := c.settings
	c.mu.RUnlock()
	if settings == nil {
		return "", nil, fmt.Errorf("no phone send settings are cached")
	}

	var only *gmproto.SIMCard
	for _, sim := range settings.GetSIMCards() {
		if sim.GetSIMParticipant().GetID() == "" || sim.GetSIMData().GetSIMPayload() == nil {
			continue
		}
		if only != nil {
			return "", nil, fmt.Errorf("multiple usable SIMs were received; cannot choose sender/SIM")
		}
		only = sim
	}
	if only == nil {
		return "", nil, fmt.Errorf("no usable SIM metadata was received")
	}
	return only.GetSIMParticipant().GetID(), only, nil
}

func (c *Client) watchMessageEcho(tmpID string) (func(context.Context) (*libgm.WrappedMessage, error), func()) {
	echo := make(chan *libgm.WrappedMessage, 1)
	c.mu.Lock()
	idx := len(c.subscribers)
	c.subscribers = append(c.subscribers, func(evt any) {
		w, ok := evt.(*libgm.WrappedMessage)
		if !ok || w.Message.GetTmpID() != tmpID {
			return
		}
		select {
		case echo <- w:
		default:
		}
	})
	c.mu.Unlock()

	unsubscribe := func() {
		c.mu.Lock()
		if idx < len(c.subscribers) {
			c.subscribers = append(c.subscribers[:idx], c.subscribers[idx+1:]...)
		}
		c.mu.Unlock()
	}
	wait := func(ctx context.Context) (*libgm.WrappedMessage, error) {
		select {
		case w := <-echo:
			return w, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return wait, unsubscribe
}

func sendStatusMessage(resp *gmproto.SendMessageResponse) string {
	switch resp.GetStatus() {
	case gmproto.SendMessageResponse_UNKNOWN:
		if resp.GetGoogleAccountSwitch() != nil {
			return "switch back to QR pairing or log in with Google account to send messages"
		}
		return "unknown status"
	case gmproto.SendMessageResponse_FAILURE_2:
		return "unknown permanent error"
	case gmproto.SendMessageResponse_FAILURE_3:
		return "unknown temporary error"
	case gmproto.SendMessageResponse_FAILURE_4:
		return "Google Messages is not your default SMS app"
	default:
		return resp.GetStatus().String()
	}
}

// ReactionAction selects ADD / REMOVE / SWITCH semantics on SendReaction.
type ReactionAction int

const (
	ReactionAdd ReactionAction = iota
	ReactionRemove
	ReactionSwitch
)

// SendReaction adds, removes, or switches a unicode reaction on a message.
func (c *Client) SendReaction(messageID, emoji string, action ReactionAction) error {
	if messageID == "" {
		return fmt.Errorf("message id is required")
	}
	if emoji == "" {
		return fmt.Errorf("emoji is required")
	}
	var act gmproto.SendReactionRequest_Action
	switch action {
	case ReactionAdd:
		act = gmproto.SendReactionRequest_ADD
	case ReactionRemove:
		act = gmproto.SendReactionRequest_REMOVE
	case ReactionSwitch:
		act = gmproto.SendReactionRequest_SWITCH
	default:
		return fmt.Errorf("unknown reaction action %v", action)
	}
	_, err := c.libgm.SendReaction(&gmproto.SendReactionRequest{
		MessageID:    messageID,
		Action:       act,
		ReactionData: &gmproto.ReactionData{Unicode: emoji},
	})
	if err != nil {
		return fmt.Errorf("libgm reaction: %w", err)
	}
	return nil
}

// DownloadMedia retrieves and decrypts the bytes for an attachment.
// The connection does not need to be in long-poll mode — DownloadMedia
// uses authenticated HTTP — but the AuthData's TachyonAuthToken must be
// fresh. Call Connect once before this if the session has been idle.
func (c *Client) DownloadMedia(mediaID string, key []byte) ([]byte, error) {
	if mediaID == "" {
		return nil, fmt.Errorf("media id is required")
	}
	return c.libgm.DownloadMedia(mediaID, key)
}

// AuthSnapshot returns a deep copy of the current AuthData by JSON
// round-trip. Useful for diagnostics; do not modify.
func (c *Client) AuthSnapshot() (*libgm.AuthData, error) {
	return cloneAuthData(c.auth)
}

// dispatch is the single libgm callback. It persists on token refresh and
// then fans out to subscribers.
func (c *Client) dispatch(evt any) {
	switch e := evt.(type) {
	case *events.AuthTokenRefreshed, *events.PairSuccessful:
		if err := saveAuth(c.layout.Session, c.auth); err != nil {
			c.logger.Error().Err(err).Msg("Failed to persist refreshed auth data")
		}
	case *events.ClientReady:
		c.mu.Lock()
		c.ready = true
		c.mu.Unlock()
	case *gmproto.Settings:
		c.mu.Lock()
		c.settings = e
		c.mu.Unlock()
	}
	c.mu.RLock()
	subs := append([]EventHandler(nil), c.subscribers...)
	c.mu.RUnlock()
	for _, h := range subs {
		h(evt)
	}
}

// PairResult is returned by Pair on success. PhoneID identifies the paired
// device; SessionPath is where the persisted AuthData lives.
type PairResult struct {
	PhoneID     string
	SessionPath string
}

// QRRenderer is invoked once Pair has the QR URL ready. The implementation
// is responsible for displaying it (terminal QR, plain URL, etc.).
type QRRenderer func(qrURL string)

// EmojiRenderer is invoked after Google Account pairing has started and the
// phone is waiting for verification. The user must tap this emoji on the
// Google Messages prompt.
type EmojiRenderer func(emoji string)

// Pair runs the QR pairing flow. It writes session.json on success and
// returns the paired phone ID. Cancellable via ctx; otherwise bounded by
// PairTimeout. Existing session.json (if any) is overwritten on success.
func Pair(ctx context.Context, layout paths.Layout, logger zerolog.Logger, render QRRenderer) (*PairResult, error) {
	if err := layout.EnsureDirs(); err != nil {
		return nil, err
	}
	auth := libgm.NewAuthData()
	cli := libgm.NewClient(auth, nil, logger)

	done := make(chan *events.PairSuccessful, 1)
	fatal := make(chan error, 1)
	cli.SetEventHandler(func(evt any) {
		switch e := evt.(type) {
		case *events.PairSuccessful:
			select {
			case done <- e:
			default:
			}
		case *events.ListenFatalError:
			select {
			case fatal <- fmt.Errorf("pairing transport failed: %w", e.Error):
			default:
			}
		}
	})

	qr, err := cli.StartLogin()
	if err != nil {
		return nil, fmt.Errorf("start login: %w", err)
	}
	render(qr)

	timeout := time.NewTimer(PairTimeout)
	defer timeout.Stop()

	select {
	case <-ctx.Done():
		cli.Disconnect()
		return nil, ctx.Err()
	case err := <-fatal:
		cli.Disconnect()
		return nil, err
	case <-timeout.C:
		cli.Disconnect()
		return nil, errors.New("pairing timed out — phone never scanned the QR code")
	case res := <-done:
		// libgm reconnects internally 2s after PairSuccessful; we're not
		// going to keep this client around, so close down cleanly.
		cli.Disconnect()
		if err := saveAuth(layout.Session, auth); err != nil {
			return nil, fmt.Errorf("persist session: %w", err)
		}
		return &PairResult{PhoneID: res.PhoneID, SessionPath: layout.Session}, nil
	}
}

// PairGoogle runs Google Account pairing. Google requires the authenticated
// messages.google.com browser cookies for the account that is enabled for
// pairing on the phone. The flow displays an emoji, waits for the matching
// emoji to be selected on the phone, and persists the resulting session.
func PairGoogle(ctx context.Context, layout paths.Layout, logger zerolog.Logger, cookies map[string]string, render EmojiRenderer) (*PairResult, error) {
	if err := layout.EnsureDirs(); err != nil {
		return nil, err
	}
	if len(cookies) == 0 {
		return nil, errors.New("Google Account cookies are required")
	}
	auth := libgm.NewAuthData()
	auth.SetCookies(cookies)
	cli := libgm.NewClient(auth, nil, logger)
	defer cli.Disconnect()

	if err := cli.FetchConfig(ctx); err != nil {
		return nil, fmt.Errorf("fetch Google Messages account config: %w", err)
	}
	emoji, session, err := cli.StartGaiaPairing(ctx)
	if err != nil {
		return nil, fmt.Errorf("start Google Account pairing: %w", err)
	}
	render(emoji)

	pairCtx, cancel := context.WithTimeout(ctx, PairTimeout)
	defer cancel()
	phoneID, err := cli.FinishGaiaPairing(pairCtx, session)
	if err != nil {
		return nil, fmt.Errorf("finish Google Account pairing: %w", err)
	}
	if err := saveAuth(layout.Session, auth); err != nil {
		return nil, fmt.Errorf("persist session: %w", err)
	}
	return &PairResult{PhoneID: phoneID, SessionPath: layout.Session}, nil
}

// RefreshGoogleCookies replaces the rotating Google Account browser cookies
// in an existing paired session. The detached candidate must successfully
// fetch Google's account configuration before it can replace session.json.
func RefreshGoogleCookies(ctx context.Context, layout paths.Layout, logger zerolog.Logger, cookies map[string]string) error {
	return refreshGoogleCookies(layout, cookies, func(candidate *libgm.AuthData) error {
		client := libgm.NewClient(candidate, nil, logger)
		defer client.Disconnect()
		if err := client.FetchConfig(ctx); err != nil {
			return fmt.Errorf("fetch Google Messages account config: %w", err)
		}
		return nil
	})
}

type googleCookieValidator func(*libgm.AuthData) error

func refreshGoogleCookies(layout paths.Layout, cookies map[string]string, validate googleCookieValidator) error {
	if len(cookies) == 0 {
		return errors.New("Google Account cookies are required")
	}
	existing, err := loadAuth(layout.Session)
	if err != nil {
		return err
	}
	candidate, err := cloneAuthData(existing)
	if err != nil {
		return fmt.Errorf("copy existing session: %w", err)
	}
	candidate.SetCookies(cookies)
	if validate == nil {
		return errors.New("Google Account cookie validator is required")
	}
	if err := validate(candidate); err != nil {
		return fmt.Errorf("validate refreshed Google Account cookies: %w", err)
	}
	if err := saveAuth(layout.Session, candidate); err != nil {
		return fmt.Errorf("persist validated Google Account cookies: %w", err)
	}
	return nil
}

func loadAuth(path string) (*libgm.AuthData, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no session at %s; run `gmcli auth` first", path)
		}
		return nil, fmt.Errorf("open session %s: %w", path, err)
	}
	defer f.Close()
	var auth libgm.AuthData
	if err := json.NewDecoder(f).Decode(&auth); err != nil {
		return nil, fmt.Errorf("decode session %s: %w", path, err)
	}
	return &auth, nil
}

func saveAuth(path string, auth *libgm.AuthData) error {
	data, err := marshalAuthData(auth, true)
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary session in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("secure temporary session %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write session: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync temporary session %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// RefreshSessionFromWebStorage rekeys an existing Google Account session from
// the current Messages for Web localStorage values. Google reuses the browser
// device ID when it is paired again, but rotates the Tachyon token, pairing ID,
// encryption keys, and refresh key. Keeping the stale values produces a 401 or
// messages that cannot be decrypted.
func RefreshSessionFromWebStorage(layout paths.Layout, values map[string]string, cryptoPrefix string) error {
	return refreshSessionFromWebStorage(layout, values, cryptoPrefix, validateAndRefreshWebStorageSession)
}

type webStorageSessionValidator func(*libgm.AuthData) error

func refreshSessionFromWebStorage(layout paths.Layout, values map[string]string, cryptoPrefix string, validate webStorageSessionValidator) error {
	existing, err := loadAuth(layout.Session)
	if err != nil {
		return err
	}
	candidate, err := applyWebStorageSession(existing, values, cryptoPrefix, time.Now().UTC())
	if err != nil {
		return err
	}
	if validate == nil {
		return errors.New("web storage session validator is required")
	}
	if err := validate(candidate); err != nil {
		return fmt.Errorf("validate refreshed Messages session: %w", err)
	}
	if len(candidate.TachyonAuthToken) == 0 || candidate.TachyonTTL <= 0 || !candidate.TachyonExpiry.After(time.Now().UTC()) {
		return errors.New("validate refreshed Messages session: signed refresh did not return a usable token")
	}
	if err := saveAuth(layout.Session, candidate); err != nil {
		return fmt.Errorf("persist validated Messages session: %w", err)
	}
	return nil

}

// applyWebStorageSession parses and applies browser storage to a detached copy
// of existing. It performs no I/O, allowing callers to validate the candidate
// before replacing the durable session.
func applyWebStorageSession(existing *libgm.AuthData, values map[string]string, cryptoPrefix string, now time.Time) (*libgm.AuthData, error) {
	if existing == nil || existing.SessionID == uuid.Nil || existing.DestRegID == uuid.Nil || existing.Browser == nil || existing.Mobile == nil || existing.RequestCrypto == nil || existing.RefreshKey == nil {
		return nil, errors.New("existing account session is incomplete; pair normally before refreshing it")
	}
	auth, err := cloneAuthData(existing)
	if err != nil {
		return nil, fmt.Errorf("copy existing session: %w", err)
	}
	decode := func(name string) ([]byte, error) {
		value := strings.TrimSpace(values[name])
		if value == "" {
			return nil, fmt.Errorf("web storage is missing %s", name)
		}
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", name, err)
		}
		return decoded, nil
	}
	deviceData, err := decode("bg_tachyon_auth_device_id")
	if err != nil {
		return nil, err
	}
	var deviceID gmproto.SignInGaiaRequest_Inner_DeviceID
	if err := proto.Unmarshal(deviceData, &deviceID); err != nil {
		return nil, fmt.Errorf("decode Messages browser device: %w", err)
	}
	expectedDeviceID := "messages-web-" + strings.ReplaceAll(auth.SessionID.String(), "-", "")
	if deviceID.DeviceID != expectedDeviceID {
		return nil, errors.New("web storage belongs to a different Messages browser device")
	}
	if deviceID.UnknownInt1 != 3 {
		return nil, errors.New("web storage contains an incomplete Messages browser device")
	}
	destRegID, err := uuid.Parse(values["bg_tachyon_dest_registration_id"])
	if err != nil {
		return nil, fmt.Errorf("parse destination registration id: %w", err)
	}
	if auth.DestRegID != uuid.Nil && auth.DestRegID != destRegID {
		return nil, errors.New("web storage targets a different phone registration")
	}
	pairingID, err := uuid.Parse(values["bg_tachyon_pairing_attempt_id"])
	if err != nil {
		return nil, fmt.Errorf("parse pairing attempt id: %w", err)
	}
	aesKey, err := decode(cryptoPrefix + "crypto_msg_enc_key")
	if err != nil {
		return nil, err
	}
	hmacKey, err := decode(cryptoPrefix + "crypto_msg_hmac")
	if err != nil {
		return nil, err
	}
	privateKey, err := decode("crypto_priv_key")
	if err != nil {
		return nil, err
	}
	publicKey, err := decode("crypto_pub_key")
	if err != nil {
		return nil, err
	}
	token, err := decode("bg_tachyon_auth_token")
	if err != nil {
		return nil, err
	}
	if len(aesKey) != 32 || len(hmacKey) != 32 || len(privateKey) != 32 || len(publicKey) != 65 || publicKey[0] != 4 || len(token) == 0 {
		return nil, errors.New("web storage contains malformed Messages session keys")
	}
	curve := elliptic.P256()
	publicX, publicY := elliptic.Unmarshal(curve, publicKey)
	privateScalar := new(big.Int).SetBytes(privateKey)
	if publicX == nil || privateScalar.Sign() <= 0 || privateScalar.Cmp(curve.Params().N) >= 0 {
		return nil, errors.New("web storage contains an invalid P-256 refresh key")
	}
	derivedX, derivedY := curve.ScalarBaseMult(privateKey)
	if derivedX.Cmp(publicX) != 0 || derivedY.Cmp(publicY) != 0 {
		return nil, errors.New("web storage refresh public key does not match its private key")
	}
	auth.RequestCrypto.AESKey = aesKey
	auth.RequestCrypto.HMACKey = hmacKey
	auth.RefreshKey.KeyType = "EC"
	auth.RefreshKey.Curve = "P-256"
	auth.RefreshKey.D = privateKey
	auth.RefreshKey.X = publicKey[1:33]
	auth.RefreshKey.Y = publicKey[33:65]
	auth.TachyonAuthToken = token
	// localStorage does not expose the token's issue time or TTL. Mark it due
	// now so Connect validates it through the signed refresh endpoint instead
	// of assuming a fresh 24-hour lifetime for a potentially old disk value.
	auth.TachyonTTL = (24 * time.Hour).Microseconds()
	// Force libgm to prove the token and refresh key against Tachyon before the
	// candidate can replace the durable session.
	auth.TachyonExpiry = now.Add(-libgm.RefreshTachyonBuffer)
	auth.DestRegID = destRegID
	auth.PairingID = pairingID
	return auth, nil
}

func cloneAuthData(auth *libgm.AuthData) (*libgm.AuthData, error) {
	data, err := marshalAuthData(auth, false)
	if err != nil {
		return nil, err
	}
	var cloned libgm.AuthData
	if err := json.Unmarshal(data, &cloned); err != nil {
		return nil, err
	}
	return &cloned, nil
}

// marshalAuthData serializes AuthData while holding libgm's cookie lock.
// libgm may replace response cookies from its long-poll goroutine while gmcli
// is persisting the session; encoding the map without the lock is a data race
// and can produce an inconsistent snapshot (or panic on concurrent writes).
func marshalAuthData(auth *libgm.AuthData, indent bool) ([]byte, error) {
	if auth == nil {
		return nil, errors.New("auth data is nil")
	}
	auth.CookiesLock.RLock()
	defer auth.CookiesLock.RUnlock()
	if indent {
		return json.MarshalIndent(auth, "", "  ")
	}
	return json.Marshal(auth)
}

func validateAndRefreshWebStorageSession(auth *libgm.AuthData) error {
	client := libgm.NewClient(auth, nil, zerolog.Nop())
	defer client.Disconnect()
	if err := client.Connect(); err != nil {
		return err
	}
	client.Disconnect()
	return nil
}
