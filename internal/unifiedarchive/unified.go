// Package unifiedarchive builds a provenance-preserving conversation view
// across gmcli's relay JSONL and Android Telephony provider archives.
package unifiedarchive

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fdsouvenir/gmcli/internal/androidtelephony"
	"github.com/fdsouvenir/gmcli/internal/archive"
)

const (
	Format        = "gmcli-unified-jsonl"
	FormatVersion = 1
	mergeWindowMS = int64(2000)
)

var e164Pattern = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// Options configures a unified archive export.
type Options struct {
	RelayDirectory     string
	TelephonyDirectory string
	OutputDirectory    string
	Force              bool
}

// Result describes an installed unified archive.
type Result struct {
	Path                    string `json:"path"`
	Format                  string `json:"format"`
	FormatVersion           int    `json:"format_version"`
	Conversations           int    `json:"conversations"`
	Messages                int    `json:"messages"`
	RelaySourceMessages     int    `json:"relay_source_messages"`
	TelephonySourceMessages int    `json:"telephony_source_messages"`
	CrossSourceMatches      int    `json:"cross_source_matches"`
}

type manifest struct {
	Format                  string                     `json:"format"`
	FormatVersion           int                        `json:"format_version"`
	GeneratedAt             time.Time                  `json:"generated_at"`
	SelfE164                string                     `json:"self_e164"`
	RelayManifestSHA256     string                     `json:"relay_manifest_sha256"`
	TelephonyManifestSHA256 string                     `json:"telephony_manifest_sha256"`
	ConversationsFile       manifestFile               `json:"conversations_file"`
	ConversationMessages    []manifestConversationFile `json:"conversation_messages"`
}

type manifestFile struct {
	Path    string `json:"path"`
	Records int    `json:"records"`
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256"`
}

type manifestConversationFile struct {
	CanonicalConversationID string   `json:"canonical_conversation_id"`
	Path                    string   `json:"path"`
	Messages                int      `json:"messages"`
	Bytes                   int64    `json:"bytes"`
	SHA256                  string   `json:"sha256"`
	Participants            []string `json:"participants"`
}

type relayManifest struct {
	Files map[string]struct {
		Path string `json:"path"`
	} `json:"files"`
	ConversationMessages []struct {
		ConversationID string `json:"conversation_id"`
		Path           string `json:"path"`
	} `json:"conversation_messages"`
}

type telephonyManifest struct {
	Threads []struct {
		ThreadID string `json:"thread_id"`
		Path     string `json:"path"`
	} `json:"threads"`
}

type relayParticipant struct {
	E164 string `json:"e164"`
	Name string `json:"name"`
	IsMe bool   `json:"is_me"`
}

type relayConversation struct {
	ConversationID string             `json:"conversation_id"`
	Name           string             `json:"name"`
	Participants   []relayParticipant `json:"participants"`
}

type relayMessage struct {
	MessageID      string  `json:"message_id"`
	ConversationID string  `json:"conversation_id"`
	Body           *string `json:"body"`
	TimestampMS    int64   `json:"timestamp_ms"`
	IsFromMe       bool    `json:"is_from_me"`
	MediaID        *string `json:"media_id"`
	MimeType       *string `json:"mime_type"`
}

type taggedValue struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

type telephonyRecord struct {
	RecordType string                 `json:"record_type"`
	MMSID      *int64                 `json:"mms_id,omitempty"`
	PartID     *int64                 `json:"part_id,omitempty"`
	MediaPath  string                 `json:"media_path,omitempty"`
	ByteLength int64                  `json:"byte_length,omitempty"`
	SHA256     string                 `json:"sha256,omitempty"`
	Values     map[string]taggedValue `json:"values,omitempty"`
}

// Participant is a canonical non-self conversation participant.
type Participant struct {
	E164 string `json:"e164"`
	Name string `json:"name,omitempty"`
}

// SourceRef points back to an immutable source record.
type SourceRef struct {
	Platform       string `json:"platform"`
	RecordType     string `json:"record_type"`
	ConversationID string `json:"conversation_id,omitempty"`
	ThreadID       string `json:"thread_id,omitempty"`
	RecordID       string `json:"record_id"`
	Path           string `json:"path"`
}

// Attachment describes source attachment metadata without embedding bytes.
type Attachment struct {
	Platform  string `json:"platform"`
	RecordID  string `json:"record_id,omitempty"`
	MediaID   string `json:"media_id,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
	MediaPath string `json:"media_path,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
}

// Message is one deduplicated, source-attributed message in the derived view.
type Message struct {
	RecordType              string       `json:"record_type"`
	FormatVersion           int          `json:"format_version"`
	CanonicalConversationID string       `json:"canonical_conversation_id"`
	UnifiedMessageID        string       `json:"unified_message_id"`
	TimestampMS             int64        `json:"timestamp_ms"`
	IsFromMe                bool         `json:"is_from_me"`
	Body                    *string      `json:"body"`
	Attachments             []Attachment `json:"attachments"`
	Sources                 []SourceRef  `json:"sources"`
}

// Conversation summarizes a canonical participant-set conversation.
type Conversation struct {
	RecordType              string        `json:"record_type"`
	FormatVersion           int           `json:"format_version"`
	CanonicalConversationID string        `json:"canonical_conversation_id"`
	Name                    string        `json:"name"`
	Participants            []Participant `json:"participants"`
	RelayConversationIDs    []string      `json:"relay_conversation_ids"`
	TelephonyThreadIDs      []string      `json:"telephony_thread_ids"`
	Messages                int           `json:"messages"`
	RelaySourceMessages     int           `json:"relay_source_messages"`
	TelephonySourceMessages int           `json:"telephony_source_messages"`
	CrossSourceMatches      int           `json:"cross_source_matches"`
	FirstMessageMS          int64         `json:"first_message_ms,omitempty"`
	LastMessageMS           int64         `json:"last_message_ms,omitempty"`
}

type conversationBuild struct {
	id                  string
	numbers             []string
	names               map[string]string
	conversationNames   map[string]struct{}
	relayIDs            map[string]struct{}
	threadIDs           map[string]struct{}
	messages            []Message
	relaySourceMessages int
	telephonyMessages   int
	crossSourceMatches  int
}

type sourceMessage struct {
	canonicalID string
	numbers     []string
	names       map[string]string
	message     Message
}

// Write verifies both source archives, builds a derived canonical view, and
// atomically installs it. Source archives are never modified.
func Write(options Options) (Result, error) {
	if options.RelayDirectory == "" || options.TelephonyDirectory == "" || options.OutputDirectory == "" {
		return Result{}, errors.New("relay, telephony, and output directories are required")
	}
	if _, err := archive.VerifyJSONL(options.RelayDirectory); err != nil {
		return Result{}, fmt.Errorf("verify relay archive: %w", err)
	}
	if _, err := androidtelephony.Verify(options.TelephonyDirectory); err != nil {
		return Result{}, fmt.Errorf("verify telephony archive: %w", err)
	}
	relayDir, err := filepath.Abs(options.RelayDirectory)
	if err != nil {
		return Result{}, err
	}
	telephonyDir, err := filepath.Abs(options.TelephonyDirectory)
	if err != nil {
		return Result{}, err
	}
	out, err := filepath.Abs(options.OutputDirectory)
	if err != nil {
		return Result{}, err
	}
	if !options.Force {
		if _, err := os.Stat(out); err == nil {
			return Result{}, fmt.Errorf("output already exists: %s (use --force to replace it)", out)
		} else if !os.IsNotExist(err) {
			return Result{}, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return Result{}, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(out), ".gmcli-unified-*")
	if err != nil {
		return Result{}, err
	}
	installed := false
	defer func() {
		if !installed {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := os.Chmod(tmp, 0o700); err != nil {
		return Result{}, err
	}

	relayManifestPath := filepath.Join(relayDir, "manifest.json")
	telephonyManifestPath := filepath.Join(telephonyDir, "manifest.json")
	var rm relayManifest
	if err := readJSONFile(relayManifestPath, &rm); err != nil {
		return Result{}, err
	}
	var tm telephonyManifest
	if err := readJSONFile(telephonyManifestPath, &tm); err != nil {
		return Result{}, err
	}
	conversationsPath := "conversations.jsonl"
	if entry, ok := rm.Files["conversations"]; ok && entry.Path != "" {
		conversationsPath = entry.Path
	}
	relayConversations, selfE164, err := loadRelayConversations(filepath.Join(relayDir, filepath.FromSlash(conversationsPath)))
	if err != nil {
		return Result{}, err
	}
	builds := make(map[string]*conversationBuild)
	if err := loadRelayMessages(relayDir, rm, relayConversations, selfE164, builds); err != nil {
		return Result{}, err
	}
	if err := loadTelephonyMessages(telephonyDir, tm, selfE164, relayNameLookup(relayConversations), builds); err != nil {
		return Result{}, err
	}

	result, outputManifest, err := writeOutput(tmp, builds, selfE164)
	if err != nil {
		return Result{}, err
	}
	outputManifest.RelayManifestSHA256, _, err = fileDigest(relayManifestPath)
	if err != nil {
		return Result{}, err
	}
	outputManifest.TelephonyManifestSHA256, _, err = fileDigest(telephonyManifestPath)
	if err != nil {
		return Result{}, err
	}
	if err := writeJSONFile(filepath.Join(tmp, "manifest.json"), outputManifest); err != nil {
		return Result{}, err
	}
	if err := installDirectory(tmp, out, options.Force); err != nil {
		return Result{}, err
	}
	installed = true
	result.Path = out
	return result, nil
}

func loadRelayConversations(path string) (map[string]relayConversation, string, error) {
	conversations := make(map[string]relayConversation)
	self := make(map[string]struct{})
	err := readJSONL(path, func(raw []byte) error {
		var conversation relayConversation
		if err := json.Unmarshal(raw, &conversation); err != nil {
			return err
		}
		if conversation.ConversationID == "" {
			return errors.New("relay conversation lacks conversation_id")
		}
		conversations[conversation.ConversationID] = conversation
		for _, participant := range conversation.Participants {
			if participant.IsMe && validE164(participant.E164) {
				self[participant.E164] = struct{}{}
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("read relay conversations: %w", err)
	}
	if len(self) != 1 {
		return nil, "", fmt.Errorf("relay archive identifies %d self phone numbers, want exactly one", len(self))
	}
	var selfE164 string
	for value := range self {
		selfE164 = value
	}
	return conversations, selfE164, nil
}

func loadRelayMessages(dir string, rm relayManifest, conversations map[string]relayConversation, self string, builds map[string]*conversationBuild) error {
	for _, entry := range rm.ConversationMessages {
		conversation, ok := conversations[entry.ConversationID]
		if !ok {
			return fmt.Errorf("relay messages reference unknown conversation %q", entry.ConversationID)
		}
		numbers, names := relayIdentity(conversation, self)
		canonicalID := canonicalConversationID(numbers, "relay:"+entry.ConversationID)
		build := ensureBuild(builds, canonicalID, numbers, names)
		build.relayIDs[entry.ConversationID] = struct{}{}
		if conversation.Name != "" {
			build.conversationNames[conversation.Name] = struct{}{}
		}
		path := filepath.Join(dir, filepath.FromSlash(entry.Path))
		line := 0
		if err := readJSONL(path, func(raw []byte) error {
			line++
			var value relayMessage
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			attachments := []Attachment{}
			if value.MediaID != nil && *value.MediaID != "" {
				attachment := Attachment{Platform: "gm", MediaID: *value.MediaID}
				if value.MimeType != nil {
					attachment.MimeType = *value.MimeType
				}
				attachments = append(attachments, attachment)
			}
			message := Message{
				RecordType: "unified_message", FormatVersion: FormatVersion,
				CanonicalConversationID: canonicalID, TimestampMS: value.TimestampMS,
				IsFromMe: value.IsFromMe, Body: value.Body, Attachments: attachments,
				Sources: []SourceRef{{Platform: "gm", RecordType: "message", ConversationID: entry.ConversationID, RecordID: value.MessageID, Path: entry.Path}},
			}
			message.UnifiedMessageID = unifiedMessageID(canonicalID, message.Sources[0])
			build.messages = append(build.messages, message)
			build.relaySourceMessages++
			return nil
		}); err != nil {
			return fmt.Errorf("read relay messages %q: %w", entry.Path, err)
		}
	}
	return nil
}

func loadTelephonyMessages(dir string, tm telephonyManifest, self string, knownNames map[string]string, builds map[string]*conversationBuild) error {
	for _, entry := range tm.Threads {
		path := filepath.Join(dir, filepath.FromSlash(entry.Path))
		records, err := readTelephonyRecords(path)
		if err != nil {
			return fmt.Errorf("read telephony thread %q: %w", entry.ThreadID, err)
		}
		mms := make(map[int64]telephonyRecord)
		addresses := make(map[int64][]string)
		parts := make(map[int64][]telephonyRecord)
		partData := make(map[int64][]telephonyRecord)
		for _, record := range records {
			switch record.RecordType {
			case "mms":
				id, err := taggedInt(record.Values, "_id")
				if err != nil {
					return err
				}
				mms[id] = record
			case "mms_address":
				if record.MMSID != nil {
					if address, ok := taggedString(record.Values, "address"); ok {
						addresses[*record.MMSID] = append(addresses[*record.MMSID], address)
					}
				}
			case "mms_part":
				if record.MMSID != nil {
					parts[*record.MMSID] = append(parts[*record.MMSID], record)
				}
			case "mms_part_data":
				if record.MMSID != nil {
					partData[*record.MMSID] = append(partData[*record.MMSID], record)
				}
			}
		}
		for _, record := range records {
			if record.RecordType != "sms" {
				continue
			}
			address, ok := taggedString(record.Values, "address")
			if !ok {
				continue
			}
			numbers := []string(nil)
			if validE164(address) && address != self {
				numbers = []string{address}
			}
			id, err := taggedInt(record.Values, "_id")
			if err != nil {
				return err
			}
			date, err := taggedInt(record.Values, "date")
			if err != nil {
				return err
			}
			typeValue, err := taggedInt(record.Values, "type")
			if err != nil {
				return err
			}
			bodyValue, bodyOK := taggedString(record.Values, "body")
			var body *string
			if bodyOK {
				body = &bodyValue
			}
			addTelephonyMessage(builds, numbers, knownNames, entry, Message{
				RecordType: "unified_message", FormatVersion: FormatVersion,
				TimestampMS: date, IsFromMe: typeValue != 1, Body: body, Attachments: []Attachment{},
				Sources: []SourceRef{{Platform: "android_telephony", RecordType: "sms", ThreadID: entry.ThreadID, RecordID: strconv.FormatInt(id, 10), Path: entry.Path}},
			})
		}
		mmsIDs := make([]int64, 0, len(mms))
		for id := range mms {
			mmsIDs = append(mmsIDs, id)
		}
		sort.Slice(mmsIDs, func(i, j int) bool { return mmsIDs[i] < mmsIDs[j] })
		for _, id := range mmsIDs {
			record := mms[id]
			numbers := canonicalNumbers(addresses[id], self)
			date, err := taggedInt(record.Values, "date")
			if err != nil {
				return err
			}
			if date > 0 && date < 100_000_000_000 {
				date *= 1000
			}
			box, err := taggedInt(record.Values, "msg_box")
			if err != nil {
				return err
			}
			body, attachments := mmsContent(parts[id], partData[id])
			addTelephonyMessage(builds, numbers, knownNames, entry, Message{
				RecordType: "unified_message", FormatVersion: FormatVersion,
				TimestampMS: date, IsFromMe: box != 1, Body: body, Attachments: attachments,
				Sources: []SourceRef{{Platform: "android_telephony", RecordType: "mms", ThreadID: entry.ThreadID, RecordID: strconv.FormatInt(id, 10), Path: entry.Path}},
			})
		}
	}
	for _, build := range builds {
		mergeCrossSource(build)
	}
	return nil
}

func addTelephonyMessage(builds map[string]*conversationBuild, numbers []string, knownNames map[string]string, entry struct {
	ThreadID string `json:"thread_id"`
	Path     string `json:"path"`
}, message Message) {
	canonicalID := canonicalConversationID(numbers, "android-thread:"+entry.ThreadID)
	names := make(map[string]string)
	for _, number := range numbers {
		if knownNames[number] != "" {
			names[number] = knownNames[number]
		}
	}
	build := ensureBuild(builds, canonicalID, numbers, names)
	build.threadIDs[entry.ThreadID] = struct{}{}
	message.CanonicalConversationID = canonicalID
	message.UnifiedMessageID = unifiedMessageID(canonicalID, message.Sources[0])
	build.messages = append(build.messages, message)
	build.telephonyMessages++
}

func mergeCrossSource(build *conversationBuild) {
	relay := make([]Message, 0, len(build.messages))
	telephony := make([]Message, 0, len(build.messages))
	for _, message := range build.messages {
		if message.Sources[0].Platform == "gm" {
			relay = append(relay, message)
		} else {
			telephony = append(telephony, message)
		}
	}
	sortMessages(relay)
	sortMessages(telephony)
	index := make(map[string][]int)
	for i := range relay {
		key := mergeKey(relay[i])
		index[key] = append(index[key], i)
	}
	used := make(map[int]bool)
	for _, candidate := range telephony {
		matches := index[mergeKey(candidate)]
		position, ok := closestMatch(relay, matches, candidate.TimestampMS, used)
		if !ok {
			relay = append(relay, candidate)
			continue
		}
		used[position] = true
		relay[position].Sources = append(relay[position].Sources, candidate.Sources...)
		relay[position].Attachments = appendUniqueAttachments(relay[position].Attachments, candidate.Attachments)
		build.crossSourceMatches++
	}
	sortMessages(relay)
	build.messages = relay
}

func closestMatch(messages []Message, positions []int, timestamp int64, used map[int]bool) (int, bool) {
	best, bestDistance, ties := -1, int64(0), 0
	for _, position := range positions {
		if used[position] {
			continue
		}
		distance := messages[position].TimestampMS - timestamp
		if distance < 0 {
			distance = -distance
		}
		if distance > mergeWindowMS {
			continue
		}
		if best == -1 || distance < bestDistance {
			best, bestDistance, ties = position, distance, 1
		} else if distance == bestDistance {
			ties++
		}
	}
	return best, best >= 0 && ties == 1
}

func mergeKey(message Message) string {
	body := ""
	if message.Body != nil {
		body = strings.ReplaceAll(*message.Body, "\r\n", "\n")
	}
	// The relay export does not always retain media metadata for an MMS that is
	// still present in the Telephony provider. Direction, exact normalized body,
	// and a unique closest timestamp are the cross-source identity contract;
	// provider attachment metadata is then added to the merged record.
	return strconv.FormatBool(message.IsFromMe) + "\x00" + body
}

func appendUniqueAttachments(existing, additions []Attachment) []Attachment {
	seen := make(map[string]bool)
	for _, attachment := range existing {
		seen[attachmentKey(attachment)] = true
	}
	for _, attachment := range additions {
		key := attachmentKey(attachment)
		if !seen[key] {
			existing = append(existing, attachment)
			seen[key] = true
		}
	}
	return existing
}

func attachmentKey(value Attachment) string {
	return value.Platform + "\x00" + value.RecordID + "\x00" + value.MediaID + "\x00" + value.MediaPath + "\x00" + value.SHA256
}

func mmsContent(parts, data []telephonyRecord) (*string, []Attachment) {
	sort.Slice(parts, func(i, j int) bool {
		a, _ := taggedInt(parts[i].Values, "seq")
		b, _ := taggedInt(parts[j].Values, "seq")
		if a != b {
			return a < b
		}
		return partRecordID(parts[i]) < partRecordID(parts[j])
	})
	texts := make([]string, 0)
	attachments := make([]Attachment, 0)
	dataByPart := make(map[int64]telephonyRecord)
	for _, record := range data {
		if record.PartID != nil {
			dataByPart[*record.PartID] = record
		}
	}
	for _, part := range parts {
		id := partRecordID(part)
		mime, _ := taggedString(part.Values, "ct")
		text, hasText := taggedString(part.Values, "text")
		if mime == "text/plain" && hasText {
			texts = append(texts, text)
			continue
		}
		if mime == "application/smil" {
			continue
		}
		attachment := Attachment{Platform: "android_telephony", RecordID: strconv.FormatInt(id, 10), MimeType: mime}
		if record, ok := dataByPart[id]; ok {
			attachment.MediaPath = record.MediaPath
			attachment.SHA256 = record.SHA256
			attachment.Bytes = record.ByteLength
		}
		attachments = append(attachments, attachment)
	}
	if len(texts) == 0 {
		return nil, attachments
	}
	body := strings.Join(texts, "\n")
	return &body, attachments
}

func partRecordID(record telephonyRecord) int64 {
	value, _ := taggedInt(record.Values, "_id")
	return value
}

func relayIdentity(conversation relayConversation, self string) ([]string, map[string]string) {
	values := make([]string, 0)
	names := make(map[string]string)
	for _, participant := range conversation.Participants {
		if participant.IsMe || participant.E164 == self || !validE164(participant.E164) {
			continue
		}
		values = append(values, participant.E164)
		if participant.Name != "" {
			names[participant.E164] = participant.Name
		}
	}
	return canonicalNumbers(values, self), names
}

func relayNameLookup(conversations map[string]relayConversation) map[string]string {
	names := make(map[string]string)
	for _, conversation := range conversations {
		for _, participant := range conversation.Participants {
			if !participant.IsMe && validE164(participant.E164) && participant.Name != "" {
				names[participant.E164] = participant.Name
			}
		}
	}
	return names
}

func canonicalNumbers(values []string, self string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == self || !validE164(value) || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func validE164(value string) bool { return e164Pattern.MatchString(value) }

func canonicalConversationID(numbers []string, fallback string) string {
	if len(numbers) == 0 {
		return fallback
	}
	if len(numbers) == 1 {
		return "e164:" + numbers[0]
	}
	sum := sha256.Sum256([]byte(strings.Join(numbers, ",")))
	return "group:" + hex.EncodeToString(sum[:16])
}

func ensureBuild(builds map[string]*conversationBuild, id string, numbers []string, names map[string]string) *conversationBuild {
	build, ok := builds[id]
	if !ok {
		build = &conversationBuild{id: id, numbers: append([]string(nil), numbers...), names: make(map[string]string), conversationNames: make(map[string]struct{}), relayIDs: make(map[string]struct{}), threadIDs: make(map[string]struct{})}
		builds[id] = build
	}
	for number, name := range names {
		if name != "" {
			build.names[number] = name
		}
	}
	return build
}

func unifiedMessageID(canonicalID string, source SourceRef) string {
	sum := sha256.Sum256([]byte(canonicalID + "\x00" + source.Platform + "\x00" + source.RecordType + "\x00" + source.RecordID))
	return hex.EncodeToString(sum[:16])
}

func writeOutput(dir string, builds map[string]*conversationBuild, self string) (Result, manifest, error) {
	messagesDir := filepath.Join(dir, "messages")
	if err := os.Mkdir(messagesDir, 0o700); err != nil {
		return Result{}, manifest{}, err
	}
	ids := make([]string, 0, len(builds))
	for id := range builds {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := Result{Format: Format, FormatVersion: FormatVersion}
	conversationRecords := make([]Conversation, 0, len(ids))
	conversationFiles := make([]manifestConversationFile, 0, len(ids))
	for _, id := range ids {
		build := builds[id]
		sortMessages(build.messages)
		participants := make([]Participant, 0, len(build.numbers))
		for _, number := range build.numbers {
			participants = append(participants, Participant{E164: number, Name: build.names[number]})
		}
		conversation := Conversation{
			RecordType: "unified_conversation", FormatVersion: FormatVersion,
			CanonicalConversationID: id, Name: conversationName(build), Participants: participants,
			RelayConversationIDs: sortedKeys(build.relayIDs), TelephonyThreadIDs: sortedKeys(build.threadIDs),
			Messages: len(build.messages), RelaySourceMessages: build.relaySourceMessages,
			TelephonySourceMessages: build.telephonyMessages, CrossSourceMatches: build.crossSourceMatches,
		}
		if len(build.messages) > 0 {
			for _, message := range build.messages {
				if message.TimestampMS > 0 {
					conversation.FirstMessageMS = message.TimestampMS
					break
				}
			}
			conversation.LastMessageMS = build.messages[len(build.messages)-1].TimestampMS
		}
		conversationRecords = append(conversationRecords, conversation)
		name := base64.RawURLEncoding.EncodeToString([]byte(id)) + ".jsonl"
		relativePath := filepath.ToSlash(filepath.Join("messages", name))
		file, err := writeJSONL(filepath.Join(dir, filepath.FromSlash(relativePath)), build.messages)
		if err != nil {
			return Result{}, manifest{}, err
		}
		conversationFiles = append(conversationFiles, manifestConversationFile{CanonicalConversationID: id, Path: relativePath, Messages: len(build.messages), Bytes: file.Bytes, SHA256: file.SHA256, Participants: append([]string(nil), build.numbers...)})
		result.Conversations++
		result.Messages += len(build.messages)
		result.RelaySourceMessages += build.relaySourceMessages
		result.TelephonySourceMessages += build.telephonyMessages
		result.CrossSourceMatches += build.crossSourceMatches
	}
	conversationsFile, err := writeJSONL(filepath.Join(dir, "conversations.jsonl"), conversationRecords)
	if err != nil {
		return Result{}, manifest{}, err
	}
	return result, manifest{Format: Format, FormatVersion: FormatVersion, GeneratedAt: time.Now().UTC(), SelfE164: self, ConversationsFile: manifestFile{Path: "conversations.jsonl", Records: len(conversationRecords), Bytes: conversationsFile.Bytes, SHA256: conversationsFile.SHA256}, ConversationMessages: conversationFiles}, nil
}

func conversationName(build *conversationBuild) string {
	if len(build.numbers) == 1 && build.names[build.numbers[0]] != "" {
		return build.names[build.numbers[0]]
	}
	names := sortedKeys(build.conversationNames)
	if len(names) > 0 {
		return names[len(names)-1]
	}
	labels := make([]string, 0, len(build.numbers))
	for _, number := range build.numbers {
		if build.names[number] != "" {
			labels = append(labels, build.names[number])
		} else {
			labels = append(labels, number)
		}
	}
	return strings.Join(labels, ", ")
}

func sortMessages(messages []Message) {
	sort.SliceStable(messages, func(i, j int) bool {
		if messages[i].TimestampMS != messages[j].TimestampMS {
			return messages[i].TimestampMS < messages[j].TimestampMS
		}
		return messages[i].UnifiedMessageID < messages[j].UnifiedMessageID
	})
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func readTelephonyRecords(path string) ([]telephonyRecord, error) {
	values := make([]telephonyRecord, 0)
	err := readJSONL(path, func(raw []byte) error {
		var value telephonyRecord
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		values = append(values, value)
		return nil
	})
	return values, err
}

func taggedInt(values map[string]taggedValue, name string) (int64, error) {
	value, ok := values[name]
	if !ok || value.Type == "null" {
		return 0, fmt.Errorf("missing integer field %q", name)
	}
	var number int64
	if err := json.Unmarshal(value.Value, &number); err != nil {
		return 0, fmt.Errorf("decode integer field %q: %w", name, err)
	}
	return number, nil
}

func taggedString(values map[string]taggedValue, name string) (string, bool) {
	value, ok := values[name]
	if !ok || value.Type == "null" {
		return "", false
	}
	var text string
	if err := json.Unmarshal(value.Value, &text); err != nil {
		return "", false
	}
	return text, true
}

func readJSONL(path string, visit func([]byte) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<20)
	line := 0
	for {
		raw, readErr := reader.ReadBytes('\n')
		if len(raw) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		line++
		if len(strings.TrimSpace(string(raw))) == 0 || !json.Valid(raw) {
			return fmt.Errorf("invalid JSONL at line %d", line)
		}
		if err := visit(raw); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return readErr
		}
	}
	return nil
}

type writtenFile struct {
	Bytes  int64
	SHA256 string
}

func writeJSONL[T any](path string, values []T) (writtenFile, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return writtenFile{}, err
	}
	writer := bufio.NewWriterSize(file, 1<<20)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	for _, value := range values {
		if err := encoder.Encode(value); err != nil {
			_ = file.Close()
			return writtenFile{}, err
		}
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		return writtenFile{}, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return writtenFile{}, err
	}
	if err := file.Close(); err != nil {
		return writtenFile{}, err
	}
	digest, bytes, err := fileDigest(path)
	return writtenFile{Bytes: bytes, SHA256: digest}, err
}

func writeJSONFile(path string, value any) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func readJSONFile(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func fileDigest(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	bytes, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), bytes, nil
}

func installDirectory(tmp, destination string, force bool) error {
	if !force {
		return os.Rename(tmp, destination)
	}
	backup := destination + ".old"
	_ = os.RemoveAll(backup)
	if err := os.Rename(destination, backup); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmp, destination); err != nil {
		_ = os.Rename(backup, destination)
		return err
	}
	return os.RemoveAll(backup)
}
