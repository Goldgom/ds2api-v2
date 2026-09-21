package chat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// One table drives the tool-call contract for every shape the DeepSeek web chat
// emits: a call without parameters, several calls in one wrapper, the legacy
// canonical markup, JSON parameter values, empty CDATA, prose around a call,
// two wrappers in one message, an unknown tool name, markup inside CDATA and
// multibyte arguments. Every shape is checked twice - as a single chunk and as
// a message that arrives in small chunks - and again on the non-stream path.
//
// The invariants pinned here are the ones a client relies on: one call per
// invocation, a unique non-empty id, a unique and increasing index, arguments
// that decode to a JSON object, no markup in the visible content, and
// finish_reason=tool_calls.
type toolCallShape struct {
	name      string
	message   string
	toolNames []string
	// want maps a call name to its expected arguments JSON.
	want map[string]string
	// wantCalls is the expected number of tool calls.
	wantCalls int
	// wantContent lists substrings that must appear in the visible content.
	wantContent []string
	// wantNoMarkup asserts that no raw markup leaks into the visible content.
	wantNoMarkup bool
}

func toolCallShapes() []toolCallShape {
	epse := func(body string) string { return "<|EPSE|tool_calls>" + body + "</|EPSE|tool_calls>" }
	invoke := func(name, params string) string {
		return `<|EPSE|invoke name="` + name + `">` + params + `</|EPSE|invoke>`
	}
	param := func(name, value string) string {
		return `<|EPSE|parameter name="` + name + `"><![CDATA[` + value + `]]></|EPSE|parameter>`
	}
	return []toolCallShape{
		{
			name:      "zero-parameter-call",
			message:   epse(invoke("list_issues", "")),
			toolNames: []string{"list_issues"},
			want:      map[string]string{"list_issues": `{}`},
			wantCalls: 1,
		},
		{
			name:      "five-calls-one-wrapper",
			message:   epse(invoke("ls", param("path", ".")) + invoke("read", param("path", "a.md")) + invoke("find", param("pattern", "*.md")) + invoke("grep", param("q", "todo")) + invoke("ls", param("path", "src"))),
			toolNames: []string{"ls", "read", "find", "grep"},
			wantCalls: 5,
		},
		{
			name:      "legacy-xml",
			message:   `<tool_calls><invoke name="read"><parameter name="path">README.md</parameter></invoke></tool_calls>`,
			toolNames: []string{"read"},
			want:      map[string]string{"read": `{"path":"README.md"}`},
			wantCalls: 1,
		},
		{
			name:      "json-parameter-value",
			message:   epse(invoke("write", `<|EPSE|parameter name="payload">{"a":1,"b":[2,3]}</|EPSE|parameter>`)),
			toolNames: []string{"write"},
			want:      map[string]string{"write": `{"payload":{"a":1,"b":[2,3]}}`},
			wantCalls: 1,
		},
		{
			name:      "empty-cdata-value",
			message:   epse(invoke("write", param("content", ""))),
			toolNames: []string{"write"},
			want:      map[string]string{"write": `{"content":""}`},
			wantCalls: 1,
		},
		{
			name:         "prose-around-call",
			message:      "先看目录，然后读取说明。\n" + epse(invoke("ls", param("path", "."))) + "\n完成。",
			toolNames:    []string{"ls"},
			wantCalls:    1,
			wantContent:  []string{"先看目录", "完成。"},
			wantNoMarkup: true,
		},
		{
			name:         "two-wrappers-with-prose",
			message:      "第一步：\n" + epse(invoke("ls", param("path", "."))) + "\n第二步：\n" + epse(invoke("ls", param("path", "src"))),
			toolNames:    []string{"ls"},
			wantCalls:    2,
			wantContent:  []string{"第一步：", "第二步："},
			wantNoMarkup: true,
		},
		{
			name:      "unknown-tool-name",
			message:   epse(invoke("mystery_tool", param("x", "1"))),
			toolNames: []string{"ls"},
			want:      map[string]string{"mystery_tool": `{"x":1}`},
			wantCalls: 1,
		},
		{
			name:      "cdata-with-markup-inside",
			message:   epse(invoke("write", param("content", "<div class=\"x\">hi</div>\n<tool_calls>example</tool_calls>"))),
			toolNames: []string{"write"},
			wantCalls: 1,
		},
		{
			name:      "multibyte-and-newlines",
			message:   epse(invoke("write", param("content", "第一行\n第二行\t制表"))),
			toolNames: []string{"write"},
			want:      map[string]string{"write": `{"content":"第一行\n第二行\t制表"}`},
			wantCalls: 1,
		},
	}
}

func TestToolCallShapesSingleChunk(t *testing.T) {
	for _, tc := range toolCallShapes() {
		t.Run(tc.name, func(t *testing.T) {
			body := runReplayStream(t, tc.toolNames, replayContentLine(t, tc.message))
			assertToolCallShape(t, body, tc)
		})
	}
}

func TestToolCallShapesChunked(t *testing.T) {
	for _, tc := range toolCallShapes() {
		for _, size := range []int{16, 24, 48} {
			t.Run(tc.name, func(t *testing.T) {
				lines := make([]string, 0, 8)
				for _, chunk := range replayRunes(tc.message, size) {
					lines = append(lines, replayContentLine(t, chunk))
				}
				body := runReplayStream(t, tc.toolNames, lines...)
				assertToolCallShape(t, body, tc)
			})
		}
	}
}

func assertToolCallShape(t *testing.T, body string, tc toolCallShape) {
	t.Helper()
	calls := streamedToolCalls(t, body)
	if len(calls) != tc.wantCalls {
		t.Fatalf("expected %d tool calls, got %d body=%s", tc.wantCalls, len(calls), body)
	}
	want := make(map[string]string, len(tc.want))
	for name, args := range tc.want {
		want[name] = args
	}
	ids := map[string]bool{}
	indexes := map[float64]bool{}
	for _, call := range calls {
		id := asString(call["id"])
		if id == "" || ids[id] {
			t.Fatalf("expected a unique non-empty id, got %q in %v", id, ids)
		}
		ids[id] = true
		index := toolCallIndex(t, call)
		if indexes[index] {
			t.Fatalf("expected a unique index, got %v", indexes)
		}
		indexes[index] = true
		fn, _ := call["function"].(map[string]any)
		args, _ := fn["arguments"].(string)
		var decoded any
		if err := json.Unmarshal([]byte(args), &decoded); err != nil {
			t.Fatalf("tool call %v arguments are not valid JSON: %q", fn["name"], args)
		}
		if _, ok := decoded.(map[string]any); !ok {
			t.Fatalf("tool call %v arguments must decode to an object, got %#v (%q)", fn["name"], decoded, args)
		}
		if len(want) > 0 {
			name := asString(fn["name"])
			expected, ok := want[name]
			if !ok {
				t.Fatalf("unexpected call %q args=%s body=%s", name, args, body)
			}
			if args != expected {
				t.Fatalf("call %q args = %s, want %s", name, args, expected)
			}
			delete(want, name)
		}
	}
	if tc.wantNoMarkup {
		content := streamedDeltaContent(t, body)
		for _, marker := range []string{"|EPSE|", "<tool_calls", "<invoke", "parameter name"} {
			if strings.Contains(content, marker) {
				t.Fatalf("markup %q leaked into content: %q", marker, content)
			}
		}
	}
	for _, expected := range tc.wantContent {
		if content := streamedDeltaContent(t, body); !strings.Contains(content, expected) {
			t.Fatalf("expected content %q, got %q", expected, content)
		}
	}
	if reason := streamFinishReason(mustFrames(t, body)); reason != "tool_calls" {
		t.Fatalf("expected finish_reason=tool_calls, got %q body=%s", reason, body)
	}
}

func TestToolCallShapesNonStream(t *testing.T) {
	for _, tc := range toolCallShapes() {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			resp := makeSSEHTTPResponse(replayContentLine(t, tc.message), `data: [DONE]`)
			rec := httptest.NewRecorder()
			h.handleNonStream(rec, resp, "cid", "deepseek-v4-flash", "prompt", 0, false, false, tc.toolNames, nil, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
			}
			out := decodeJSONBody(t, rec.Body.String())
			choices, _ := out["choices"].([]any)
			if len(choices) != 1 {
				t.Fatalf("expected one choice, got %#v", out)
			}
			choice, _ := choices[0].(map[string]any)
			message, _ := choice["message"].(map[string]any)
			toolCalls, _ := message["tool_calls"].([]any)
			if len(toolCalls) != tc.wantCalls {
				t.Fatalf("expected %d tool calls, got %d body=%s", tc.wantCalls, len(toolCalls), rec.Body.String())
			}
			ids := map[string]bool{}
			for i, raw := range toolCalls {
				call, _ := raw.(map[string]any)
				id := asString(call["id"])
				if id == "" || ids[id] {
					t.Fatalf("expected a unique non-empty id, got %q", id)
				}
				ids[id] = true
				if idx, ok := call["index"].(float64); ok && int(idx) != i {
					t.Fatalf("expected index %d, got %v", i, call["index"])
				}
				fn, _ := call["function"].(map[string]any)
				args := asString(fn["arguments"])
				var decoded any
				if err := json.Unmarshal([]byte(args), &decoded); err != nil {
					t.Fatalf("arguments are not valid JSON: %q", args)
				}
				if _, ok := decoded.(map[string]any); !ok {
					t.Fatalf("arguments must decode to an object, got %#v", decoded)
				}
			}
			if tc.wantNoMarkup {
				content := asString(message["content"])
				if strings.Contains(content, "|EPSE|") || strings.Contains(content, "<tool_calls") {
					t.Fatalf("markup leaked into message content: %q", content)
				}
			}
		})
	}
}
