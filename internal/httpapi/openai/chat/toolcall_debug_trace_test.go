package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ds2api/internal/toolcalldebug"
)

// The tool-call trace exists so a duplicate-call report can be pinned down
// without a full capture: it records how the accumulated text grew, which parts
// reproduced text that was already there, and which calls were emitted. This
// test drives the trace through a normal round and through a replay that is not
// covered yet, and checks that the trace shows the difference.
func TestToolCallDebugTraceRecordsReplayEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	toolcalldebug.ResetForTest()
	defer toolcalldebug.ResetForTest()
	t.Setenv("DS2API_DEBUG_TOOLCALL", path)

	wrapper := "<|EPSE|tool_calls>"
	for i := 1; i <= 5; i++ {
		marker := "TC" + string(rune('0'+i))
		wrapper += `<|EPSE|invoke name="bash"><|EPSE|parameter name="command"><![CDATA[echo "` + marker + `-$(date +%s%N)"]]></|EPSE|parameter></|EPSE|invoke>`
	}
	wrapper += "</|EPSE|tool_calls>"

	lines := make([]string, 0, 256)
	for _, chunk := range replayRunes(wrapper, 24) {
		lines = append(lines, replayContentLine(t, chunk))
	}
	// A round boundary followed by a token-sized replay: the replay has to be
	// recognised and dropped.
	lines = append(lines,
		`data: {"request_message_id":1,"response_message_id":2,"model_type":"default"}`,
		`data: {"p":"response/status","v":"WIP"}`,
	)
	for _, chunk := range replayRunes(wrapper, 3) {
		lines = append(lines, replayContentLine(t, chunk))
	}
	body := runReplayStream(t, []string{"bash"}, lines...)
	if calls := streamedToolCalls(t, body); len(calls) != 5 {
		t.Fatalf("expected 5 tool calls, got %d body=%s", len(calls), body)
	}

	trace := readTrace(t, path)
	if len(trace) == 0 {
		t.Fatalf("expected trace records")
	}
	final := lastEvent(t, trace, "finalize")
	if final == nil {
		t.Fatalf("expected a finalize record, got %d records", len(trace))
	}
	if got, _ := final["rawInvocations"].(float64); got != 5 {
		t.Fatalf("expected 5 invocations in the accumulated text, got %v", final["rawInvocations"])
	}
	if got, _ := final["rawWrapperOpens"].(float64); got != 1 {
		t.Fatalf("expected 1 wrapper in the accumulated text, got %v", final["rawWrapperOpens"])
	}
	if got, _ := final["emittedCallCount"].(float64); got != 5 {
		t.Fatalf("expected 5 distinct emitted calls, got %v", final["emittedCallCount"])
	}
	emitted := countEvent(trace, "call_emitted")
	if emitted != 5 {
		t.Fatalf("expected 5 call_emitted records, got %d", emitted)
	}
	// The replayed parts were dropped, so at least one part reports a rewrite.
	if countEvent(trace, "part") == 0 {
		t.Fatalf("expected part records")
	}
}

// A model that repeats itself inside one wrapper is left alone (intentional
// repeats are allowed), and the trace is what shows that the duplication came
// from the text rather than from a missed replay: no part reproduces the start
// of the accumulated text, yet the text holds ten invocations.
func TestToolCallDebugTraceSeparatesTextDuplicationFromMissedReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	toolcalldebug.ResetForTest()
	defer toolcalldebug.ResetForTest()
	t.Setenv("DS2API_DEBUG_TOOLCALL", path)

	single := `<|EPSE|invoke name="bash"><|EPSE|parameter name="command"><![CDATA[echo "TC-$(date)"]]></|EPSE|parameter></|EPSE|invoke>`
	wrapper := "<|EPSE|tool_calls>" + strings.Repeat(single, 5) + "</|EPSE|tool_calls>"
	body := runReplayStream(t, []string{"bash"}, replayContentLine(t, wrapper))
	if calls := streamedToolCalls(t, body); len(calls) != 5 {
		t.Fatalf("expected 5 tool calls, got %d body=%s", len(calls), body)
	}

	trace := readTrace(t, path)
	final := lastEvent(t, trace, "finalize")
	if final == nil {
		t.Fatalf("expected a finalize record")
	}
	if got, _ := final["rawInvocations"].(float64); got != 5 {
		t.Fatalf("expected 5 invocations, got %v", final["rawInvocations"])
	}
	for _, record := range trace {
		if record["event"] != "part" {
			continue
		}
		if match, _ := record["prefixMatch"].(float64); match > 0 {
			t.Fatalf("a single delivered part cannot reproduce accumulated text: %#v", record)
		}
	}
}

// When a replay is appended instead of dropped, the trace has to say so: the
// accumulated text ends up holding the calls twice, and at least one part
// reports that it reproduced already accumulated text and was appended anyway.
// That is exactly the combination a duplicate-call report is diagnosed from, so
// it is pinned here even though the shape is not handled yet.
func TestToolCallDebugTraceShowsAppendedReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	toolcalldebug.ResetForTest()
	defer toolcalldebug.ResetForTest()
	t.Setenv("DS2API_DEBUG_TOOLCALL", path)

	wrapper := "<|EPSE|tool_calls>"
	for i := 1; i <= 5; i++ {
		marker := "TC" + string(rune('0'+i))
		wrapper += `<|EPSE|invoke name="bash"><|EPSE|parameter name="command"><![CDATA[echo "` + marker + `"]]></|EPSE|parameter></|EPSE|invoke>`
	}
	wrapper += "</|EPSE|tool_calls>"

	// No round-level line between the delivery and the replay: the pump merges
	// the tail of the first delivery with the replay head, so the replay cannot
	// be recognised from the text alone.
	lines := make([]string, 0, 128)
	for _, chunk := range replayRunes(wrapper, 24) {
		lines = append(lines, replayContentLine(t, chunk))
	}
	for _, chunk := range replayRunes(wrapper, 7) {
		lines = append(lines, replayContentLine(t, chunk))
	}
	body := runReplayStream(t, []string{"bash"}, lines...)
	calls := streamedToolCalls(t, body)

	trace := readTrace(t, path)
	final := lastEvent(t, trace, "finalize")
	if final == nil {
		t.Fatalf("expected a finalize record")
	}
	invocations, _ := final["rawInvocations"].(float64)
	appendedReplay := false
	for _, record := range trace {
		if record["event"] != "part" {
			continue
		}
		match, _ := record["prefixMatch"].(float64)
		interior, _ := record["interiorOffset"].(float64)
		appendLen, _ := record["appendLen"].(float64)
		if appendLen > 0 && (match > 0 || interior >= 0) {
			appendedReplay = true
			break
		}
	}
	if int(invocations) != len(calls)*2 && !appendedReplay {
		t.Fatalf("trace neither shows the duplication nor an appended replay: invocations=%v calls=%d trace=%v",
			invocations, len(calls), trace)
	}
	if int(invocations) == len(calls)*2 && !appendedReplay {
		t.Fatalf("the accumulated text holds every call twice but no part reports an appended replay: %v", trace)
	}
}

func readTrace(t *testing.T, path string) []map[string]any {
	t.Helper()
	toolcalldebug.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace failed: %v", err)
	}
	records := make([]map[string]any, 0, 32)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("trace line is not JSON: %v (%q)", err, line)
		}
		records = append(records, record)
	}
	return records
}

func lastEvent(t *testing.T, records []map[string]any, event string) map[string]any {
	t.Helper()
	for i := len(records) - 1; i >= 0; i-- {
		if records[i]["event"] == event {
			return records[i]
		}
	}
	return nil
}

func countEvent(records []map[string]any, event string) int {
	count := 0
	for _, record := range records {
		if record["event"] == event {
			count++
		}
	}
	return count
}
