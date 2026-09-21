package responses

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ds2api/internal/promptcompat"
)

// responsesFunctionCallIDs returns the function_call item id per call id for the
// streamed output_item.done events and for the response.completed object.
func responsesFunctionCallIDs(t *testing.T, body string) (streamed, completed map[string]string, order []string) {
	t.Helper()
	streamed = map[string]string{}
	for _, payload := range extractSSEEventPayloads(body, "response.output_item.done") {
		item, _ := payload["item"].(map[string]any)
		if asString(item["type"]) != "function_call" {
			continue
		}
		callID := asString(item["call_id"])
		streamed[callID] = asString(item["id"])
	}

	var completedPayload map[string]any
	for _, payload := range extractSSEEventPayloads(body, "response.completed") {
		completedPayload = payload
	}
	if completedPayload == nil {
		t.Fatalf("no response.completed event in body=%s", body)
	}
	responseObj, _ := completedPayload["response"].(map[string]any)
	output, _ := responseObj["output"].([]any)
	completed = map[string]string{}
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if asString(item["type"]) != "function_call" {
			continue
		}
		callID := asString(item["call_id"])
		completed[callID] = asString(item["id"])
		order = append(order, asString(item["name"])+":"+asString(item["arguments"]))
	}
	return streamed, completed, order
}

func responsesToolCallLines(wrapper string, chunk int) []string {
	lines := []string{`data: {"request_message_id":1,"response_message_id":2,"model_type":"default"}`}
	for _, part := range splitRunes(wrapper, chunk) {
		b, _ := json.Marshal(map[string]any{"p": "response/content", "v": part})
		lines = append(lines, "data: "+string(b))
	}
	return append(lines, "data: [DONE]")
}

func responsesToolCallStream(t *testing.T, lines []string) *http.Response {
	t.Helper()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(strings.Join(lines, "\n") + "\n")),
	}
}

func bashCallsWrapper(calls int) string {
	wrapper := "<tool_calls>"
	for i := 1; i <= calls; i++ {
		wrapper += "\n  <invoke name=\"bash\">\n    <parameter name=\"command\">echo " + string(rune('0'+i)) + "</parameter>\n  </invoke>"
	}
	return wrapper + "\n</tool_calls>"
}

func runResponsesToolCallStream(t *testing.T, responseID string, lines []string) string {
	t.Helper()
	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	rec := httptest.NewRecorder()
	h.handleResponsesStream(rec, req, responsesToolCallStream(t, lines), "owner-a", responseID, "deepseek-v4-flash", "prompt", 0, false, false, []string{"bash"}, nil, promptcompat.DefaultToolChoicePolicy(), "")
	return rec.Body.String()
}

// A wrapper holding five calls, delivered in small chunks, must produce five
// function_call items and five completed calls.
func TestHandleResponsesStreamEmitsEachToolCallOnce(t *testing.T) {
	body := runResponsesToolCallStream(t, "resp_tool_once", responsesToolCallLines(bashCallsWrapper(5), 17))

	streamed, completed, order := responsesFunctionCallIDs(t, body)
	if len(streamed) != 5 {
		t.Fatalf("expected 5 streamed function_call items, got %d body=%s", len(streamed), body)
	}
	if len(completed) != 5 {
		t.Fatalf("expected 5 function calls in the completed object, got %d body=%s", len(completed), body)
	}
	if len(order) != 5 {
		t.Fatalf("expected 5 calls in the completed output, got %d (order=%v)", len(order), order)
	}
}

// The streamed output_item events and response.completed must describe each call
// with the same ids. Otherwise a Responses client that reads the streamed items
// and then reconciles them with the completed object ends up with every call
// twice, which is the duplicate-call report seen from Codex-style clients.
func TestHandleResponsesStreamToolCallIDsMatchCompletedObject(t *testing.T) {
	body := runResponsesToolCallStream(t, "resp_id_stability", responsesToolCallLines(bashCallsWrapper(5), 17))

	streamed, completed, _ := responsesFunctionCallIDs(t, body)
	if len(streamed) != len(completed) {
		t.Fatalf("count mismatch: streamed=%d completed=%d body=%s", len(streamed), len(completed), body)
	}
	for callID, itemID := range streamed {
		other, ok := completed[callID]
		if !ok {
			t.Fatalf("streamed call_id %s is missing from the completed object: %v", callID, completed)
		}
		if other != itemID {
			t.Fatalf("item id changed for %s: streamed=%s completed=%s", callID, itemID, other)
		}
	}
}

// The upstream may replay the whole message in a continue round. The replayed
// copy must not reach the client as new calls.
func TestHandleResponsesStreamDropsReplayedToolCallRound(t *testing.T) {
	wrapper := bashCallsWrapper(5)
	lines := responsesToolCallLines(wrapper, 24)[:0]
	lines = append(lines, `data: {"request_message_id":1,"response_message_id":2,"model_type":"default"}`)
	for _, part := range splitRunes(wrapper, 24) {
		b, _ := json.Marshal(map[string]any{"p": "response/content", "v": part})
		lines = append(lines, "data: "+string(b))
	}
	lines = append(lines,
		`data: {"request_message_id":1,"response_message_id":2,"model_type":"default"}`,
		`data: {"p":"response/status","v":"WIP"}`,
	)
	for _, part := range splitRunes(wrapper, 6) {
		b, _ := json.Marshal(map[string]any{"p": "response/content", "v": part})
		lines = append(lines, "data: "+string(b))
	}
	lines = append(lines, "data: [DONE]")

	body := runResponsesToolCallStream(t, "resp_replay_round", lines)

	streamed, completed, _ := responsesFunctionCallIDs(t, body)
	if len(streamed) != 5 {
		t.Fatalf("expected 5 streamed function_call items after a replayed round, got %d body=%s", len(streamed), body)
	}
	if len(completed) != 5 {
		t.Fatalf("expected 5 calls in the completed object after a replayed round, got %d body=%s", len(completed), body)
	}
}

func splitRunes(s string, n int) []string {
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
