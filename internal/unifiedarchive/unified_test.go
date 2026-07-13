package unifiedarchive

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalConversationIDUsesParticipantSet(t *testing.T) {
	first := canonicalConversationID([]string{"+12025550101", "+12025550102"}, "fallback")
	second := canonicalConversationID([]string{"+12025550101", "+12025550102"}, "other")
	if first != second || !strings.HasPrefix(first, "group:") {
		t.Fatalf("group IDs are not stable: %q %q", first, second)
	}
	if got := canonicalConversationID([]string{"+12025550101"}, "fallback"); got != "e164:+12025550101" {
		t.Fatalf("direct canonical ID = %q", got)
	}
	if got := canonicalConversationID(nil, "relay:7"); got != "relay:7" {
		t.Fatalf("fallback canonical ID = %q", got)
	}
}

func TestMergeCrossSourcePreservesProvenanceAndAttachments(t *testing.T) {
	body := "same body"
	build := testBuild("e164:+12025550101")
	build.relaySourceMessages = 2
	build.telephonyMessages = 2
	build.messages = []Message{
		testMessage(build.id, 10_000, false, &body, SourceRef{Platform: "gm", RecordType: "message", RecordID: "gm-1", Path: "messages/one.jsonl"}),
		testMessage(build.id, 20_000, false, nil, SourceRef{Platform: "gm", RecordType: "message", RecordID: "gm-2", Path: "messages/one.jsonl"}),
		testMessage(build.id, 10_900, false, &body, SourceRef{Platform: "android_telephony", RecordType: "mms", RecordID: "7", Path: "threads/one.jsonl"}),
		testMessage(build.id, 1_000, true, stringPointer("history"), SourceRef{Platform: "android_telephony", RecordType: "sms", RecordID: "8", Path: "threads/old.jsonl"}),
	}
	build.messages[2].Attachments = []Attachment{{Platform: "android_telephony", RecordID: "part-1", MimeType: "image/jpeg"}}

	mergeCrossSource(build)
	if len(build.messages) != 3 || build.crossSourceMatches != 1 {
		t.Fatalf("merge produced %d messages and %d matches", len(build.messages), build.crossSourceMatches)
	}
	var merged *Message
	for i := range build.messages {
		if build.messages[i].TimestampMS == 10_000 {
			merged = &build.messages[i]
		}
	}
	if merged == nil || len(merged.Sources) != 2 || len(merged.Attachments) != 1 {
		t.Fatalf("merged message lost provenance or attachments: %#v", merged)
	}
}

func TestMergeCrossSourceRejectsAmbiguousOrDifferentBodies(t *testing.T) {
	one, two := "one", "two"
	build := testBuild("e164:+12025550101")
	build.messages = []Message{
		testMessage(build.id, 9_000, false, &one, SourceRef{Platform: "gm", RecordType: "message", RecordID: "gm-1", Path: "messages/one.jsonl"}),
		testMessage(build.id, 11_000, false, &one, SourceRef{Platform: "gm", RecordType: "message", RecordID: "gm-2", Path: "messages/one.jsonl"}),
		testMessage(build.id, 10_000, false, &one, SourceRef{Platform: "android_telephony", RecordType: "sms", RecordID: "7", Path: "threads/one.jsonl"}),
		testMessage(build.id, 9_000, false, &two, SourceRef{Platform: "android_telephony", RecordType: "sms", RecordID: "8", Path: "threads/one.jsonl"}),
	}
	mergeCrossSource(build)
	if len(build.messages) != 4 || build.crossSourceMatches != 0 {
		t.Fatalf("ambiguous/different messages were merged: messages=%d matches=%d", len(build.messages), build.crossSourceMatches)
	}
}

func TestMMSContentSeparatesTextAndMedia(t *testing.T) {
	parts := []telephonyRecord{
		{RecordType: "mms_part", Values: taggedValues(map[string]any{"_id": int64(2), "seq": int64(1), "ct": "image/jpeg"})},
		{RecordType: "mms_part", Values: taggedValues(map[string]any{"_id": int64(1), "seq": int64(0), "ct": "text/plain", "text": "caption"})},
		{RecordType: "mms_part", Values: taggedValues(map[string]any{"_id": int64(3), "seq": int64(-1), "ct": "application/smil", "text": "ignored"})},
	}
	partID := int64(2)
	data := []telephonyRecord{{RecordType: "mms_part_data", PartID: &partID, MediaPath: "media/hash", SHA256: "hash", ByteLength: 4}}
	body, attachments := mmsContent(parts, data)
	if body == nil || *body != "caption" || len(attachments) != 1 || attachments[0].MediaPath != "media/hash" {
		t.Fatalf("unexpected MMS content: body=%v attachments=%#v", body, attachments)
	}
}

func TestWriteOutputAndVerify(t *testing.T) {
	dir := t.TempDir()
	build := testBuild("e164:+12025550101")
	build.numbers = []string{"+12025550101"}
	build.names["+12025550101"] = "Friend"
	build.relayIDs["relay-1"] = struct{}{}
	build.threadIDs["42"] = struct{}{}
	build.relaySourceMessages = 1
	build.telephonyMessages = 1
	body := "hello"
	message := testMessage(build.id, 1000, false, &body, SourceRef{Platform: "gm", RecordType: "message", ConversationID: "relay-1", RecordID: "gm-1", Path: "messages/one.jsonl"})
	message.Sources = append(message.Sources, SourceRef{Platform: "android_telephony", RecordType: "sms", ThreadID: "42", RecordID: "7", Path: "threads/NDI.jsonl"})
	build.messages = []Message{message}
	build.crossSourceMatches = 1

	_, manifestValue, err := writeOutput(dir, map[string]*conversationBuild{build.id: build}, "+12025550000")
	if err != nil {
		t.Fatal(err)
	}
	manifestValue.RelayManifestSHA256 = strings.Repeat("a", 64)
	manifestValue.TelephonyManifestSHA256 = strings.Repeat("b", 64)
	if err := writeJSONFile(filepath.Join(dir, "manifest.json"), manifestValue); err != nil {
		t.Fatal(err)
	}
	verified, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Conversations != 1 || verified.Messages != 1 || verified.CrossSourceMatches != 1 {
		t.Fatalf("unexpected verification result: %+v", verified)
	}

	messagePath := filepath.Join(dir, filepath.FromSlash(manifestValue.ConversationMessages[0].Path))
	file, err := os.OpenFile(messagePath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{}\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := Verify(dir); err == nil || !strings.Contains(err.Error(), "integrity mismatch") {
		t.Fatalf("corrupt archive verification error = %v", err)
	}
}

func testBuild(id string) *conversationBuild {
	return &conversationBuild{id: id, names: map[string]string{}, conversationNames: map[string]struct{}{}, relayIDs: map[string]struct{}{}, threadIDs: map[string]struct{}{}}
}

func testMessage(id string, timestamp int64, fromMe bool, body *string, source SourceRef) Message {
	message := Message{RecordType: "unified_message", FormatVersion: FormatVersion, CanonicalConversationID: id, TimestampMS: timestamp, IsFromMe: fromMe, Body: body, Attachments: []Attachment{}, Sources: []SourceRef{source}}
	message.UnifiedMessageID = unifiedMessageID(id, source)
	return message
}

func stringPointer(value string) *string { return &value }

func taggedValues(values map[string]any) map[string]taggedValue {
	out := make(map[string]taggedValue, len(values))
	for key, value := range values {
		raw, _ := json.Marshal(value)
		typeName := "string"
		if _, ok := value.(int64); ok {
			typeName = "integer"
		}
		out[key] = taggedValue{Type: typeName, Value: raw}
	}
	return out
}
