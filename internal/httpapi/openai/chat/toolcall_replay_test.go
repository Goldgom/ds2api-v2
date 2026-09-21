package chat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// DeepSeek resends the whole message as a snapshot when a continue round opens,
// and the transport coalesces buffered increments with that snapshot. Before
// the replay handling was fixed, each round appended another copy of the
// message, so the tool-call block inside it was emitted once per round as a new
// call with a new id.

const replayToolBlock = `<tool_calls><invoke name="search"><parameter name="q">golang duplicate tool call replay</parameter></invoke></tool_calls>`

func replayContentLine(t *testing.T, text string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"p": "response/content", "v": text})
	if err != nil {
		t.Fatalf("marshal content line failed: %v", err)
	}
	return "data: " + string(b)
}

func replaySnapshotLine(t *testing.T, text string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"v": map[string]any{"response": map[string]any{
		"fragments": []any{map[string]any{"id": 3, "type": "RESPONSE", "content": text}},
	}}})
	if err != nil {
		t.Fatalf("marshal snapshot line failed: %v", err)
	}
	return "data: " + string(b)
}

func runReplayStream(t *testing.T, toolNames []string, lines ...string) string {
	t.Helper()
	h := &Handler{}
	resp := makeSSEHTTPResponse(append(append([]string(nil), lines...), `data: [DONE]`)...)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h.handleStream(rec, req, resp, "cid-replay", "deepseek-v4-flash", "prompt", 0, false, false, toolNames, nil, nil)
	return rec.Body.String()
}

func streamedToolCalls(t *testing.T, body string) []map[string]any {
	t.Helper()
	frames, _ := parseSSEDataFrames(t, body)
	out := make([]map[string]any, 0, 2)
	for _, frame := range frames {
		choices, _ := frame["choices"].([]any)
		for _, item := range choices {
			choice, _ := item.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			toolCalls, _ := delta["tool_calls"].([]any)
			for _, raw := range toolCalls {
				call, _ := raw.(map[string]any)
				out = append(out, call)
			}
		}
	}
	return out
}

func streamedDeltaContent(t *testing.T, body string) string {
	t.Helper()
	frames, _ := parseSSEDataFrames(t, body)
	out := strings.Builder{}
	for _, frame := range frames {
		choices, _ := frame["choices"].([]any)
		for _, item := range choices {
			choice, _ := item.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if text, ok := delta["content"].(string); ok {
				out.WriteString(text)
			}
		}
	}
	return out.String()
}

func toolCallIndex(t *testing.T, call map[string]any) float64 {
	t.Helper()
	index, ok := call["index"].(float64)
	if !ok {
		t.Fatalf("expected numeric tool call index, got %#v", call["index"])
	}
	return index
}

func TestHandleStreamReplayedToolCallBlockEmitsOneCall(t *testing.T) {
	message := "我先说明一下思路，然后给出调用：" + replayToolBlock
	body := runReplayStream(t, []string{"search"},
		replayContentLine(t, message),
		replaySnapshotLine(t, message),
		replayContentLine(t, " 接着补充一点点。"),
	)

	calls := streamedToolCalls(t, body)
	if len(calls) != 1 {
		t.Fatalf("expected one tool call for a replayed block, got %d body=%s", len(calls), body)
	}
	if got := asString(calls[0]["id"]); got == "" {
		t.Fatalf("expected a tool call id, body=%s", body)
	}
	if got := streamedDeltaContent(t, body); strings.Count(got, "我先说明一下思路") != 1 {
		t.Fatalf("expected replayed content once, got %q", got)
	}
}

func TestHandleStreamContinueRoundsDoNotDuplicateToolCalls(t *testing.T) {
	message := "我先说明一下思路，然后给出调用：" + replayToolBlock
	second := message + " 另外补充第一点。"
	third := second + " 另外补充第二点。"
	body := runReplayStream(t, []string{"search"},
		replayContentLine(t, message),
		replaySnapshotLine(t, message),
		replayContentLine(t, " 另外补充第一点。"),
		replaySnapshotLine(t, second),
		replayContentLine(t, " 另外补充第二点。"),
		replaySnapshotLine(t, third),
	)

	calls := streamedToolCalls(t, body)
	if len(calls) != 1 {
		t.Fatalf("expected one tool call across continue rounds, got %d body=%s", len(calls), body)
	}
	content := streamedDeltaContent(t, body)
	if strings.Count(content, "我先说明一下思路") != 1 {
		t.Fatalf("expected replayed content once, got %q", content)
	}
	if !strings.Contains(content, "另外补充第二点。") {
		t.Fatalf("expected the last round content, got %q", content)
	}
}

func TestHandleStreamDivergedReplayKeepsSingleToolCall(t *testing.T) {
	stale := "我先说明一下思路，然后给出调用：" + replayToolBlock + " 旧的结尾。"
	rewritten := "我先说明一下思路，然后给出调用：" + replayToolBlock + " 重写过的结尾。"
	body := runReplayStream(t, []string{"search"},
		replayContentLine(t, stale),
		replaySnapshotLine(t, rewritten),
		replayContentLine(t, " 后续内容。"),
	)

	calls := streamedToolCalls(t, body)
	if len(calls) != 1 {
		t.Fatalf("expected one tool call after a diverged replay, got %d body=%s", len(calls), body)
	}
	content := streamedDeltaContent(t, body)
	if strings.Count(content, "旧的结尾。") != 1 {
		t.Fatalf("expected the already streamed tail once, got %q", content)
	}
	if !strings.HasSuffix(content, "重写过的结尾。 后续内容。") {
		t.Fatalf("expected the regenerated tail to continue the stream, got %q", content)
	}
}

func TestHandleStreamKeepsIntentionallyRepeatedIdenticalCalls(t *testing.T) {
	body := runReplayStream(t, []string{"search"},
		replayContentLine(t, "第一批："+replayToolBlock),
		replayContentLine(t, "第二批："+replayToolBlock),
	)

	calls := streamedToolCalls(t, body)
	if len(calls) != 2 {
		t.Fatalf("expected two intentionally repeated calls, got %d body=%s", len(calls), body)
	}
	if asString(calls[0]["id"]) == asString(calls[1]["id"]) {
		t.Fatalf("expected distinct ids, got %#v", calls)
	}
	if toolCallIndex(t, calls[0]) == toolCallIndex(t, calls[1]) {
		t.Fatalf("expected distinct openai indexes, got %#v", calls)
	}
	if !streamHasToolCallsDelta(mustFrames(t, body)) {
		t.Fatalf("expected tool_calls deltas, body=%s", body)
	}
	if got := streamFinishReason(mustFrames(t, body)); got != "tool_calls" {
		t.Fatalf("expected finish_reason=tool_calls, got %q body=%s", got, body)
	}
}

func mustFrames(t *testing.T, body string) []map[string]any {
	t.Helper()
	frames, _ := parseSSEDataFrames(t, body)
	return frames
}
