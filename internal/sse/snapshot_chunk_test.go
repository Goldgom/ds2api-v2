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

// Parts that carry whole fragment content are marked (both a `response` envelope
// and a `response/fragments` batch); path deltas are ordinary increments. Only
// the marked parts may open a replay alignment without tool-call markup, because
// the upstream resends a message one fragment at a time and the fragment that
// restarts it is often plain prose.
func TestStartParsedLinePumpMarksWholeFragmentParts(t *testing.T) {
	body := strings.Join([]string{
		contentLine(t, "第一个增量"),
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
	marked := make([]string, 0, 2)
	plain := make([]string, 0, 2)
	for _, p := range parts {
		if p.Snapshot {
			marked = append(marked, p.Text)
			continue
		}
		plain = append(plain, p.Text)
	}
	if len(marked) != 2 || marked[0] != "第二个片段" || marked[1] != "第三个片段" {
		t.Fatalf("expected the fragment batch and the envelope to be marked, got %#v (%#v)", marked, parts)
	}
	if len(plain) != 1 || plain[0] != "第一个增量" {
		t.Fatalf("expected the path delta to stay an increment, got %#v (%#v)", plain, parts)
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
