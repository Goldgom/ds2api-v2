package chat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
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

func replayRunes(s string, n int) []string {
	runes := []rune(s)
	out := make([]string, 0, len(runes)/n+1)
	for i := 0; i < len(runes); i += n {
		end := i + n
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

// Several similar calls in one message must keep their own arguments: repeated
// parameter text (a display name that equals the intent, a path that reappears)
// looks exactly like a replayed fragment, and treating it as one used to cut the
// accumulated text apart, handing one call another call's arguments.
func TestHandleStreamSimilarToolCallsKeepTheirOwnArguments(t *testing.T) {
	message := `<|EPSE|tool_calls>
<|EPSE|invoke name="ls">
<|EPSE|parameter name="intent"><![CDATA[列出目录]]></|EPSE|parameter>
<|EPSE|parameter name="path"><![CDATA[.]]></|EPSE|parameter>
</|EPSE|invoke>
<|EPSE|invoke name="read">
<|EPSE|parameter name="displayName"><![CDATA[读取来源指南]]></|EPSE|parameter>
<|EPSE|parameter name="intent"><![CDATA[读取来源指南]]></|EPSE|parameter>
<|EPSE|parameter name="path"><![CDATA[C:\Users\Goldgom\.tokenbird\workspaces\my-workspace\sources\meow-resume-guide.md]]></|EPSE|parameter>
</|EPSE|invoke>
<|EPSE|invoke name="find">
<|EPSE|parameter name="displayName"><![CDATA[查找 Markdown 文件]]></|EPSE|parameter>
<|EPSE|parameter name="intent"><![CDATA[查找 Markdown 文件]]></|EPSE|parameter>
<|EPSE|parameter name="pattern"><![CDATA[*.md]]></|EPSE|parameter>
</|EPSE|invoke>
</|EPSE|tool_calls>`
	want := map[string]string{
		"ls":   `{"intent":"列出目录","path":"."}`,
		"read": `{"displayName":"读取来源指南","intent":"读取来源指南","path":"C:\\Users\\Goldgom\\.tokenbird\\workspaces\\my-workspace\\sources\\meow-resume-guide.md"}`,
		"find": `{"displayName":"查找 Markdown 文件","intent":"查找 Markdown 文件","pattern":"*.md"}`,
	}

	for _, size := range []int{24, 32, 48, 96, 4096} {
		lines := make([]string, 0, 8)
		for _, chunk := range replayRunes(message, size) {
			lines = append(lines, replayContentLine(t, chunk))
		}
		body := runReplayStream(t, []string{"ls", "read", "find"}, lines...)

		calls := streamedToolCalls(t, body)
		if len(calls) != len(want) {
			t.Fatalf("chunk size %d: expected %d tool calls, got %d body=%s", size, len(want), len(calls), body)
		}
		seen := make(map[string]bool, len(calls))
		for _, call := range calls {
			fn, _ := call["function"].(map[string]any)
			name := asString(fn["name"])
			args, _ := fn["arguments"].(string)
			if expected, ok := want[name]; !ok {
				t.Fatalf("chunk size %d: unexpected call %q body=%s", size, name, body)
			} else if args != expected {
				t.Fatalf("chunk size %d: call %q args = %s, want %s", size, name, args, expected)
			}
			seen[name] = true
		}
		for name := range want {
			if !seen[name] {
				t.Fatalf("chunk size %d: call %q missing, body=%s", size, name, body)
			}
		}
		if content := streamedDeltaContent(t, body); strings.Contains(content, "|EPSE|") {
			t.Fatalf("chunk size %d: markup leaked into visible content: %q", size, content)
		}
	}
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

// A continue round may resend the message token by token, so every replayed
// fragment stays below the single-chunk snapshot floor of 32 runes.
func TestHandleStreamTokenSizedReplayEmitsSingleToolCall(t *testing.T) {
	message := "我先说明一下思路，然后给出调用：" + replayToolBlock + " 然后就结束了。"
	lines := []string{replayContentLine(t, message)}
	for _, chunk := range replayRunes(message, 24) {
		lines = append(lines, replayContentLine(t, chunk))
	}
	lines = append(lines, replayContentLine(t, " 后面是新增内容。"))

	body := runReplayStream(t, []string{"search"}, lines...)

	calls := streamedToolCalls(t, body)
	if len(calls) != 1 {
		t.Fatalf("expected one tool call for a token-sized replay, got %d body=%s", len(calls), body)
	}
	content := streamedDeltaContent(t, body)
	if !strings.Contains(content, "后面是新增内容。") {
		t.Fatalf("expected the new content after the replay, got %q", content)
	}
	// The replayed fragments are dropped from the accumulated stream, so the
	// replayed message body never reaches the client again. At most the single
	// fragment that opened the alignment may have leaked before it was confirmed.
	if extra := utf8.RuneCountInString(content) - utf8.RuneCountInString(message+" 后面是新增内容。"); extra > 31 {
		t.Fatalf("expected at most one leaked fragment, got %d extra runes: %q", extra, content)
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

// A whole message state that reuses most of the accumulated text but rewrote its
// tail cannot be merged into it: dedupe never cuts accumulated text, because
// cutting content that was only *guessed* to be a replay is what corrupted tool
// call arguments. The rewritten state is therefore appended - the replayed call
// is reported again, but every call keeps a valid payload, its own id and its own
// index, and no content is lost. See docs/toolcall-semantics.md section 7.
func TestHandleStreamDivergedReplayKeepsEveryCallValid(t *testing.T) {
	stale := "我先说明一下思路，然后给出调用：" + replayToolBlock + " 旧的结尾。"
	rewritten := "我先说明一下思路，然后给出调用：" + replayToolBlock + " 重写过的结尾。"
	body := runReplayStream(t, []string{"search"},
		replayContentLine(t, stale),
		replaySnapshotLine(t, rewritten),
		replayContentLine(t, " 后续内容。"),
	)

	calls := streamedToolCalls(t, body)
	if len(calls) == 0 {
		t.Fatalf("expected the rewritten state to report its call, body=%s", body)
	}
	seenIDs := make(map[string]bool, len(calls))
	seenIndexes := make(map[float64]bool, len(calls))
	for _, call := range calls {
		fn, _ := call["function"].(map[string]any)
		if got := asString(fn["name"]); got != "search" {
			t.Fatalf("unexpected call name %q, body=%s", got, body)
		}
		if got := asString(fn["arguments"]); !json.Valid([]byte(got)) {
			t.Fatalf("call arguments are not valid JSON: %q body=%s", got, body)
		}
		id := asString(call["id"])
		if id == "" || seenIDs[id] {
			t.Fatalf("expected distinct non-empty ids, got %q body=%s", id, body)
		}
		seenIDs[id] = true
		index := toolCallIndex(t, call)
		if seenIndexes[index] {
			t.Fatalf("expected distinct indexes, got %v body=%s", index, body)
		}
		seenIndexes[index] = true
	}
	content := streamedDeltaContent(t, body)
	if !strings.Contains(content, "旧的结尾。") {
		t.Fatalf("expected the streamed tail to survive, got %q", content)
	}
	if !strings.HasSuffix(content, "重写过的结尾。 后续内容。") {
		t.Fatalf("expected the regenerated tail to continue the stream, got %q", content)
	}
	if strings.Contains(content, "|EPSE|") {
		t.Fatalf("markup leaked into visible content: %q", content)
	}
	if got := streamFinishReason(mustFrames(t, body)); got != "tool_calls" {
		t.Fatalf("expected finish_reason=tool_calls, got %q body=%s", got, body)
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
