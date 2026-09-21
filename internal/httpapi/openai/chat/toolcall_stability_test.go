package chat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The tool-call path has to stay usable on the transport shapes that actually
// occur: very large arguments, markup inside argument text, a call split across
// rounds, a call that the model put into hidden thinking, status and keep-alive
// noise, and markup that is broken just before a well-formed block.
func TestToolCallStabilityRisks(t *testing.T) {
	t.Run("long-arguments", func(t *testing.T) {
		value := strings.Repeat("Line of file content.\n", 8000)
		message := `<|EPSE|tool_calls><|EPSE|invoke name="write"><|EPSE|parameter name="content"><![CDATA[` + value + `]]></|EPSE|parameter></|EPSE|invoke></|EPSE|tool_calls>`
		body := runReplayStream(t, []string{"write"}, replayContentLine(t, message))

		calls := streamedToolCalls(t, body)
		if len(calls) != 1 {
			t.Fatalf("expected 1 call, got %d body=%s", len(calls), body)
		}
		fn, _ := calls[0]["function"].(map[string]any)
		var args map[string]any
		if err := json.Unmarshal([]byte(asString(fn["arguments"])), &args); err != nil {
			t.Fatalf("arguments are not valid JSON: %v", err)
		}
		if got, _ := args["content"].(string); got != value {
			t.Fatalf("arguments lost content: got %d bytes, want %d", len(got), len(value))
		}
	})

	t.Run("xml-content-in-cdata", func(t *testing.T) {
		value := `<root><item id="1">text</item><![CDATA[nested]]></root>`
		message := `<|EPSE|tool_calls><|EPSE|invoke name="write"><|EPSE|parameter name="content"><![CDATA[` + value + `]]></|EPSE|parameter></|EPSE|invoke></|EPSE|tool_calls>`
		body := runReplayStream(t, []string{"write"}, replayContentLine(t, message))

		calls := streamedToolCalls(t, body)
		if len(calls) != 1 {
			t.Fatalf("expected 1 call, got %d body=%s", len(calls), body)
		}
		fn, _ := calls[0]["function"].(map[string]any)
		var args map[string]any
		if err := json.Unmarshal([]byte(asString(fn["arguments"])), &args); err != nil {
			t.Fatalf("arguments are not valid JSON: %v", err)
		}
		if got, _ := args["content"].(string); got != value {
			t.Fatalf("xml content mangled: got %q, want %q", got, value)
		}
	})

	t.Run("call-split-across-rounds", func(t *testing.T) {
		body := runReplayStream(t, []string{"write"},
			replayContentLine(t, `<|EPSE|tool_calls><|EPSE|invoke name="write"><|EPSE|parameter name="content"><![CDATA[part-one`),
			replayContentLine(t, `-part-two]]></|EPSE|parameter></|EPSE|invoke></|EPSE|tool_calls>`),
		)

		calls := streamedToolCalls(t, body)
		if len(calls) != 1 {
			t.Fatalf("expected 1 call, got %d content=%q", len(calls), streamedDeltaContent(t, body))
		}
		fn, _ := calls[0]["function"].(map[string]any)
		if got := asString(fn["arguments"]); got != `{"content":"part-one-part-two"}` {
			t.Fatalf("expected the completed argument, got %s", got)
		}
	})

	// The model may emit the call inside hidden thinking, which is not rendered
	// when thinking is disabled - the call still has to reach the client.
	t.Run("hidden-thinking-call", func(t *testing.T) {
		message := `<|EPSE|tool_calls><|EPSE|invoke name="read"><|EPSE|parameter name="path"><![CDATA[README.md]]></|EPSE|parameter></|EPSE|invoke></|EPSE|tool_calls>`
		line, err := json.Marshal(map[string]any{"p": "response/thinking_content", "v": message})
		if err != nil {
			t.Fatalf("marshal thinking line failed: %v", err)
		}
		body := runReplayStream(t, []string{"read"}, "data: "+string(line))

		calls := streamedToolCalls(t, body)
		if len(calls) != 1 {
			t.Fatalf("expected 1 call from hidden thinking, got %d body=%s", len(calls), body)
		}
		fn, _ := calls[0]["function"].(map[string]any)
		if got := asString(fn["arguments"]); got != `{"path":"README.md"}` {
			t.Fatalf("expected the read arguments, got %s", got)
		}
	})

	t.Run("status-and-keepalive-noise", func(t *testing.T) {
		body := runReplayStream(t, []string{"read"},
			`: keep-alive`,
			`event: ready`,
			`data: {"request_message_id":1,"response_message_id":2,"model_type":"default"}`,
			replayContentLine(t, `<|EPSE|tool_calls><|EPSE|invoke name="read"><|EPSE|parameter name="path"><![CDATA[README.md]]></|EPSE|parameter></|EPSE|invoke></|EPSE|tool_calls>`),
			`data: {"p":"response/status","v":"INCOMPLETE"}`,
			`data: {"p":"response/status","v":"FINISHED"}`,
		)

		calls := streamedToolCalls(t, body)
		if len(calls) != 1 {
			t.Fatalf("expected 1 call, got %d content=%q", len(calls), streamedDeltaContent(t, body))
		}
		if got := asString(mustFrames(t, body)[0]["object"]); got != "chat.completion.chunk" {
			t.Fatalf("unexpected frame: %q", got)
		}
	})

	// A block whose closing tag is missing cannot be parsed, so it is released
	// as text. It must not be treated as a re-render of the text accumulated so
	// far either: doing that used to cut the following well-formed block apart,
	// which leaked its tail into the visible content.
	t.Run("good-call-after-malformed-block", func(t *testing.T) {
		body := runReplayStream(t, []string{"read", "ls"},
			replayContentLine(t, `<|EPSE|tool_calls><|EPSE|invoke name="read"><|EPSE|parameter name="path"><![CDATA[x]]></|EPSE|parameter></|EPSE|tool_calls>`),
			replayContentLine(t, `<|EPSE|tool_calls><|EPSE|invoke name="ls"><|EPSE|parameter name="path"><![CDATA[.]]></|EPSE|parameter></|EPSE|invoke></|EPSE|tool_calls>`),
		)

		calls := streamedToolCalls(t, body)
		if len(calls) != 1 {
			t.Fatalf("expected the well-formed call only, got %d body=%s", len(calls), body)
		}
		fn, _ := calls[0]["function"].(map[string]any)
		if got := asString(fn["name"]); got != "ls" {
			t.Fatalf("expected the ls call, got %q", got)
		}
		if got := asString(fn["arguments"]); got != `{"path":"."}` {
			t.Fatalf("expected the ls arguments, got %s", got)
		}
		content := streamedDeltaContent(t, body)
		// The malformed block has no closing invoke tag, so any of the good
		// block's tags in the content means its tail was released as text.
		for _, leaked := range []string{`CDATA[.]]`, `</|EPSE|invoke>`} {
			if strings.Contains(content, leaked) {
				t.Fatalf("the malformed block leaked the good block's markup %q: %q", leaked, content)
			}
		}
		if !strings.Contains(content, `name="read"`) {
			t.Fatalf("expected the unparseable block to stay visible, got %q", content)
		}
	})
}

// Markup that cannot be parsed into a call must stay visible. Dropping it made
// the response look empty, which the handler reported as an upstream failure
// (503 with upstream_unavailable) even though the upstream had responded.
func TestToolCallUnparseableMarkupStaysVisible(t *testing.T) {
	block := "<|EPSE|tool_calls>\n<|EPSE|invoke name=\"read\">\n<|EPSE|parameter name=\"path\"><![CDATA[README.md]]></|EPSE|parameter>\n</|EPSE|tool_calls>"

	body := runReplayStream(t, []string{"read"}, replayContentLine(t, block))
	if calls := streamedToolCalls(t, body); len(calls) != 0 {
		t.Fatalf("expected no call for an unclosed invoke, got %d body=%s", len(calls), body)
	}
	if got := streamedDeltaContent(t, body); !strings.Contains(got, "README.md") {
		t.Fatalf("expected the unparseable block to stay visible, got %q", got)
	}
	if reason := streamFinishReason(mustFrames(t, body)); reason != "stop" {
		t.Fatalf("expected finish_reason=stop, got %q body=%s", reason, body)
	}

	h := &Handler{}
	resp := makeSSEHTTPResponse(replayContentLine(t, block), `data: [DONE]`)
	rec := httptest.NewRecorder()
	h.handleNonStream(rec, resp, "cid", "deepseek-v4-flash", "prompt", 0, false, false, []string{"read"}, nil, nil)
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
	if got := asString(message["content"]); !strings.Contains(got, "README.md") {
		t.Fatalf("expected the unparseable block in the message content, got %q", got)
	}
	if _, ok := message["tool_calls"]; ok {
		t.Fatalf("expected no tool calls for an unclosed invoke, got %#v", message["tool_calls"])
	}
}

// Truncated markup at the end of a stream must be handed to the client as text
// as well, and the stream must still end with a finish_reason.
func TestToolCallTruncatedMarkupStaysVisible(t *testing.T) {
	block := "<|EPSE|tool_calls>\n<|EPSE|invoke name=\"read\">\n<|EPSE|parameter name=\"path\"><![CDATA[README.md"
	body := runReplayStream(t, []string{"read"}, replayContentLine(t, block))

	if calls := streamedToolCalls(t, body); len(calls) != 0 {
		t.Fatalf("expected no call for truncated markup, got %d body=%s", len(calls), body)
	}
	if got := streamedDeltaContent(t, body); !strings.Contains(got, "README.md") {
		t.Fatalf("expected the truncated markup to stay visible, got %q", got)
	}
	if reason := streamFinishReason(mustFrames(t, body)); reason != "stop" {
		t.Fatalf("expected finish_reason=stop, got %q body=%s", reason, body)
	}
}

// Degraded but recoverable markup is repaired on a best-effort basis: the call
// is reported even though the model omitted quotes or used the canonical
// parameter form inside an extended wrapper.
func TestToolCallDegradedMarkupIsRepaired(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    map[string]string
	}{
		{
			name:    "legacy-parameter-inside-wrapper",
			message: `<|EPSE|tool_calls><|EPSE|invoke name="read"><parameter name="path">README.md</parameter></|EPSE|invoke></|EPSE|tool_calls>`,
			want:    map[string]string{"read": `{"path":"README.md"}`},
		},
		{
			name:    "cdata-value-contains-closing-tag-text",
			message: `<|EPSE|tool_calls><|EPSE|invoke name="write"><|EPSE|parameter name="content"><![CDATA[a]]></|EPSE|parameter>"b"]]></|EPSE|parameter></|EPSE|invoke></|EPSE|tool_calls>`,
			want:    map[string]string{"write": `{"content":"a"}`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := runReplayStream(t, []string{"read", "write"}, replayContentLine(t, tc.message))
			calls := streamedToolCalls(t, body)
			if len(calls) != 1 {
				t.Fatalf("expected 1 repaired call, got %d body=%s", len(calls), body)
			}
			fn, _ := calls[0]["function"].(map[string]any)
			name := asString(fn["name"])
			expected, ok := tc.want[name]
			if !ok {
				t.Fatalf("unexpected call %q", name)
			}
			if got := asString(fn["arguments"]); got != expected {
				t.Fatalf("call %q args = %s, want %s", name, got, expected)
			}
			if content := streamedDeltaContent(t, body); strings.Contains(content, "|EPSE|") {
				t.Fatalf("markup leaked into content: %q", content)
			}
		})
	}
}
