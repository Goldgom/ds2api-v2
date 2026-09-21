package chat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ds2api/internal/promptcompat"
	"ds2api/internal/toolcalldebug"
)

// A duplicate-call report can only be settled when every trace record says which
// client request produced it. Without the request id on the part/line records,
// two interleaved streams look like one memory that grows two different ways.
func TestToolCallDebugTraceAttributesRecordsToRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	toolcalldebug.ResetForTest()
	defer toolcalldebug.ResetForTest()
	t.Setenv("DS2API_DEBUG_TOOLCALL", path)

	wrapper := "<|EPSE|tool_calls>"
	for i := 1; i <= 5; i++ {
		wrapper += `<|EPSE|invoke name="bash"><|EPSE|parameter name="command"><![CDATA[echo "TC` + string(rune('0'+i)) + `"]]></|EPSE|parameter></|EPSE|invoke>`
	}
	wrapper += "</|EPSE|tool_calls>"
	lines := make([]string, 0, 64)
	for _, chunk := range replayRunes(wrapper, 24) {
		lines = append(lines, replayContentLine(t, chunk))
	}
	body := runReplayStream(t, []string{"bash"}, lines...)
	if calls := streamedToolCalls(t, body); len(calls) != 5 {
		t.Fatalf("expected 5 tool calls, got %d", len(calls))
	}

	trace := readTrace(t, path)
	start := lastEvent(t, trace, "request_start")
	if start == nil {
		t.Fatalf("expected a request_start record")
	}
	if hash, _ := start["promptHash"].(string); hash == "" {
		t.Fatalf("request_start has no prompt fingerprint: %#v", start)
	}
	if got, _ := start["promptLen"].(float64); got != float64(len("prompt")) {
		t.Fatalf("expected the prompt length in request_start, got %v", start["promptLen"])
	}
	if got, _ := start["samePromptInFlight"].(float64); got != 1 {
		t.Fatalf("a single stream must report 1 in-flight stream for its prompt, got %v", got)
	}

	attributed := 0
	for _, record := range trace {
		event, _ := record["event"].(string)
		if event != "line" && event != "part" {
			continue
		}
		req, _ := record["req"].(string)
		if req != "cid-replay" {
			t.Fatalf("%s record is not attributed to its request: %#v", event, record)
		}
		attributed++
	}
	if attributed == 0 {
		t.Fatalf("expected attributed line/part records")
	}

	end := lastEvent(t, trace, "stream_end")
	if end == nil {
		t.Fatalf("expected a stream_end record")
	}
	if req, _ := end["req"].(string); req != "cid-replay" {
		t.Fatalf("stream_end is not attributed to its request: %#v", end)
	}
}

// The in-flight counter is the instrument that separates "this server consumed
// one upstream stream twice" from "the client sent the same turn twice": two
// runtime instances started with the same prompt must make the second one report
// two streams in flight for that prompt.
func TestToolCallDebugTraceReportsConcurrentSamePromptStreams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	toolcalldebug.ResetForTest()
	defer toolcalldebug.ResetForTest()
	t.Setenv("DS2API_DEBUG_TOOLCALL", path)

	newRuntime := func(completionID string) *chatStreamRuntime {
		rec := httptest.NewRecorder()
		return newChatStreamRuntime(
			rec,
			http.NewResponseController(rec),
			true,
			completionID,
			1,
			"deepseek-v4-flash",
			"same prompt for both streams",
			true,
			false,
			false,
			[]string{"bash"},
			nil,
			promptcompat.DefaultToolChoicePolicy(),
			true,
			false,
		)
	}

	first := newRuntime("cid-first")
	second := newRuntime("cid-second")
	trace := readTraceRaw(t, path)

	starts := make([]map[string]any, 0, 2)
	for _, record := range trace {
		if record["event"] == "request_start" {
			starts = append(starts, record)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("expected 2 request_start records, got %d", len(starts))
	}
	if got, _ := starts[0]["samePromptInFlight"].(float64); got != 1 {
		t.Fatalf("first stream must report 1 in-flight stream, got %v", got)
	}
	if got, _ := starts[1]["samePromptInFlight"].(float64); got != 2 {
		t.Fatalf("overlapping stream with the same prompt must report 2 in-flight streams, got %v", got)
	}
	if got, _ := starts[0]["concurrentStreams"].(float64); got != 1 {
		t.Fatalf("first stream must report 1 concurrent stream, got %v", got)
	}
	if got, _ := starts[1]["concurrentStreams"].(float64); got != 2 {
		t.Fatalf("second overlapping stream must report 2 concurrent streams, got %v", got)
	}
	if h1, _ := starts[0]["promptHash"].(string); h1 == "" {
		t.Fatalf("first start has no prompt fingerprint")
	} else if h2, _ := starts[1]["promptHash"].(string); h2 != h1 {
		t.Fatalf("both streams must share a prompt fingerprint: %q vs %q", h1, h2)
	}

	// The counter has to drain, otherwise a later unrelated request inherits a
	// bogus in-flight count. The end marker is written while the trace is still
	// on, so both streams are released before the file is read.
	first.releaseTrace()
	second.releaseTrace()
	trace = readTrace(t, path)
	ends := 0
	for _, record := range trace {
		if record["event"] == "stream_end" {
			ends++
		}
	}
	if ends != 2 {
		t.Fatalf("expected 2 stream_end records, got %d", ends)
	}
	if got := toolcalldebug.Inflight("prompt:" + starts[0]["promptHash"].(string)); got != 0 {
		t.Fatalf("in-flight registry did not drain, still %d", got)
	}
}

// readTraceRaw parses the trace without closing it, so a test can keep recording
// while it inspects what was written so far.
func readTraceRaw(t *testing.T, path string) []map[string]any {
	t.Helper()
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
