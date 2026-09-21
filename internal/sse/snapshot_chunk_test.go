package sse

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A whole message state that the upstream re-sent has to stay a chunk of its
// own. Handling it together with the buffered increment that precedes it would
// hide that it carries the whole message, and consumers could then only guess
// from the text whether the message was replayed - a guess that corrupted
// tool-call arguments.
func TestStartParsedLinePumpKeepsSnapshotChunkSeparate(t *testing.T) {
	message := "我先说明一下思路，然后给出调用：<tool_calls><invoke name=\"search\"><parameter name=\"q\">golang</parameter></invoke></tool_calls>"
	increment := " 另外补充第一点。"
	body := strings.Join([]string{
		snapshotLine(t, message),
		contentLine(t, increment),
		snapshotLine(t, message+increment),
	}, "\n\n") + "\n\n"

	results, done := StartParsedLinePump(context.Background(), strings.NewReader(body), false, "text")

	var parts []ContentPart
	for result := range results {
		parts = append(parts, result.Parts...)
	}
	if err := <-done; err != nil {
		t.Fatalf("pump error: %v", err)
	}
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts (snapshot, increment, snapshot), got %d: %#v", len(parts), parts)
	}
	if parts[0].Text != message || !parts[0].Snapshot {
		t.Fatalf("expected the first part to be the snapshot, got %#v", parts[0])
	}
	if parts[1].Text != increment || parts[1].Snapshot {
		t.Fatalf("expected the increment to stay its own chunk, got %#v", parts[1])
	}
	if parts[2].Text != message+increment || !parts[2].Snapshot {
		t.Fatalf("expected the last part to be the grown snapshot, got %#v", parts[2])
	}
}

// Only the whole-message envelope is marked as a snapshot. Incremental content
// and a batch of new fragments are ordinary parts.
func TestStartParsedLinePumpMarksOnlyWholeMessageSnapshots(t *testing.T) {
	body := strings.Join([]string{
		contentLine(t, "第一个片段"),
		`data: {"p":"response/fragments","o":"APPEND","v":[{"id":2,"type":"RESPONSE","content":"第二个片段"}]}`,
		snapshotLine(t, "第三个片段"),
	}, "\n\n") + "\n\n"

	results, done := StartParsedLinePump(context.Background(), strings.NewReader(body), false, "text")

	var parts []ContentPart
	for result := range results {
		parts = append(parts, result.Parts...)
	}
	if err := <-done; err != nil {
		t.Fatalf("pump error: %v", err)
	}
	if len(parts) == 0 {
		t.Fatalf("expected parts, got none")
	}
	marked := 0
	var plain strings.Builder
	for _, p := range parts {
		if p.Snapshot {
			marked++
			if p.Text != "第三个片段" {
				t.Fatalf("expected only the whole-message envelope to be marked, got %#v", p)
			}
			continue
		}
		plain.WriteString(p.Text)
	}
	if marked != 1 {
		t.Fatalf("expected exactly one snapshot part, got %d: %#v", marked, parts)
	}
	if got := plain.String(); got != "第一个片段第二个片段" {
		t.Fatalf("expected the incremental parts unmarked, got %q", got)
	}
}

func contentLine(t *testing.T, text string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"p": "response/content", "v": text})
	if err != nil {
		t.Fatalf("marshal content line failed: %v", err)
	}
	return "data: " + string(b)
}

func snapshotLine(t *testing.T, text string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"v": map[string]any{"response": map[string]any{
		"fragments": []any{map[string]any{"id": 3, "type": "RESPONSE", "content": text}},
	}}})
	if err != nil {
		t.Fatalf("marshal snapshot line failed: %v", err)
	}
	return "data: " + string(b)
}
