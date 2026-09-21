package shared

import (
	"ds2api/internal/toolcall"
	"encoding/json"
	"strings"

	"github.com/google/uuid"

	"ds2api/internal/toolstream"
)

func FormatIncrementalStreamToolCallDeltas(deltas []toolstream.ToolCallDelta, ids map[int]string) []map[string]any {
	if len(deltas) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(deltas))
	for _, d := range deltas {
		if d.Name == "" && d.Arguments == "" {
			continue
		}
		callID, ok := ids[d.Index]
		if !ok || callID == "" {
			callID = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			ids[d.Index] = callID
		}
		item := map[string]any{
			"index": d.Index,
			"id":    callID,
			"type":  "function",
		}
		fn := map[string]any{}
		if d.Name != "" {
			fn["name"] = d.Name
		}
		if d.Arguments != "" {
			fn["arguments"] = d.Arguments
		}
		if len(fn) > 0 {
			item["function"] = fn
		}
		out = append(out, item)
	}
	return out
}

func FilterIncrementalToolCallDeltasByAllowed(deltas []toolstream.ToolCallDelta, seenNames map[int]string) []toolstream.ToolCallDelta {
	if len(deltas) == 0 {
		return nil
	}
	out := make([]toolstream.ToolCallDelta, 0, len(deltas))
	for _, d := range deltas {
		if d.Name != "" {
			if seenNames != nil {
				seenNames[d.Index] = d.Name
			}
			out = append(out, d)
			continue
		}
		if seenNames == nil {
			out = append(out, d)
			continue
		}
		name := strings.TrimSpace(seenNames[d.Index])
		if name == "" {
			continue
		}
		out = append(out, d)
	}
	return out
}

// FormatFinalStreamToolCallsFromIndex formats complete tool calls starting at
// indexBase. OpenAI identifies a tool call inside one assistant message by its
// index, so calls emitted in separate deltas (for example one block per
// continue round) must keep advancing the index and must keep the id that was
// assigned to that index.
func FormatFinalStreamToolCallsFromIndex(calls []toolcall.ParsedToolCall, ids map[int]string, toolsRaw any, indexBase int) []map[string]any {
	if len(calls) == 0 {
		return nil
	}
	normalizedCalls := toolcall.NormalizeParsedToolCallsForSchemas(calls, toolsRaw)
	out := make([]map[string]any, 0, len(calls))
	for i, c := range normalizedCalls {
		callIndex := indexBase + i
		callID := ""
		if ids != nil {
			callID = strings.TrimSpace(ids[callIndex])
		}
		if callID == "" {
			callID = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			if ids != nil {
				ids[callIndex] = callID
			}
		}
		args, _ := json.Marshal(c.Input)
		out = append(out, map[string]any{
			"index": callIndex,
			"id":    callID,
			"type":  "function",
			"function": map[string]any{
				"name":      c.Name,
				"arguments": string(args),
			},
		})
	}
	return out
}
