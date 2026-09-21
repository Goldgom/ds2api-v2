package responses

import (
	"ds2api/internal/toolcall"
	"ds2api/internal/toolcalldebug"
	"ds2api/internal/toolstream"
	"encoding/json"
	"strings"

	openaifmt "ds2api/internal/format/openai"

	"github.com/google/uuid"
)

func (s *responsesStreamRuntime) allocateOutputIndex() int {
	idx := s.nextOutputID
	s.nextOutputID++
	return idx
}

func (s *responsesStreamRuntime) ensureMessageItemID() string {
	if strings.TrimSpace(s.messageItemID) != "" {
		return s.messageItemID
	}
	s.messageItemID = "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	return s.messageItemID
}

func (s *responsesStreamRuntime) ensureMessageOutputIndex() int {
	if s.messageOutputID >= 0 {
		return s.messageOutputID
	}
	s.messageOutputID = s.allocateOutputIndex()
	return s.messageOutputID
}

func (s *responsesStreamRuntime) ensureMessageItemAdded() {
	if s.messageAdded {
		return
	}
	itemID := s.ensureMessageItemID()
	item := map[string]any{
		"id":     itemID,
		"type":   "message",
		"role":   "assistant",
		"status": "in_progress",
	}
	s.sendEvent(
		"response.output_item.added",
		openaifmt.BuildResponsesOutputItemAddedPayload(s.responseID, itemID, s.ensureMessageOutputIndex(), item),
	)
	s.messageAdded = true
}

func (s *responsesStreamRuntime) ensureMessageContentPartAdded() {
	if s.messagePartAdded {
		return
	}
	s.ensureMessageItemAdded()
	s.sendEvent(
		"response.content_part.added",
		openaifmt.BuildResponsesContentPartAddedPayload(
			s.responseID,
			s.ensureMessageItemID(),
			s.ensureMessageOutputIndex(),
			0,
			map[string]any{"type": "output_text", "text": ""},
		),
	)
	s.messagePartAdded = true
}

func (s *responsesStreamRuntime) emitTextDelta(content string) {
	if content == "" {
		return
	}
	s.ensureMessageContentPartAdded()
	s.visibleText.WriteString(content)
	s.sendEvent(
		"response.output_text.delta",
		openaifmt.BuildResponsesTextDeltaPayload(
			s.responseID,
			s.ensureMessageItemID(),
			s.ensureMessageOutputIndex(),
			0,
			content,
		),
	)
}

func (s *responsesStreamRuntime) closeMessageItem() {
	if !s.messageAdded {
		return
	}
	itemID := s.ensureMessageItemID()
	outputIndex := s.ensureMessageOutputIndex()
	text := s.visibleText.String()
	if s.messagePartAdded {
		s.sendEvent(
			"response.output_text.done",
			openaifmt.BuildResponsesTextDonePayload(
				s.responseID,
				itemID,
				outputIndex,
				0,
				text,
			),
		)
		s.sendEvent(
			"response.content_part.done",
			openaifmt.BuildResponsesContentPartDonePayload(
				s.responseID,
				itemID,
				outputIndex,
				0,
				map[string]any{"type": "output_text", "text": text},
			),
		)
		s.messagePartAdded = false
	}
	item := map[string]any{
		"id":     itemID,
		"type":   "message",
		"role":   "assistant",
		"status": "completed",
		"content": []map[string]any{
			{
				"type": "output_text",
				"text": text,
			},
		},
	}
	s.sendEvent(
		"response.output_item.done",
		openaifmt.BuildResponsesOutputItemDonePayload(s.responseID, itemID, outputIndex, item),
	)
}

// responsesFunctionCall is one tool call the client was told about. Its ids are
// created once, when the call first becomes visible, and every later event for
// that call reuses them: the streamed output_item events and the
// response.completed object have to describe the same call with the same ids, or
// a client that reads both ends up with the call twice.
type responsesFunctionCall struct {
	itemID   string
	callID   string
	outputID int
	name     string
	args     string
	added    bool
	done     bool
}

// functionCallSignature identifies a call by its observable payload. It is used
// to skip echoes of a replayed snapshot, not to deduplicate calls the model
// really repeated: a repeated call appears twice in the same parsed batch and
// both are emitted.
func functionCallSignature(call toolcall.ParsedToolCall) string {
	args, _ := json.Marshal(call.Input)
	return call.Name + "\x00" + string(args)
}

func (s *responsesStreamRuntime) callForIndex(callIndex int) *responsesFunctionCall {
	if s.functionRound == nil {
		s.functionRound = map[int]*responsesFunctionCall{}
	}
	if call, ok := s.functionRound[callIndex]; ok {
		return call
	}
	call := s.newFunctionCall()
	s.functionRound[callIndex] = call
	return call
}

func (s *responsesStreamRuntime) newFunctionCall() *responsesFunctionCall {
	call := &responsesFunctionCall{
		itemID:   "fc_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		callID:   "call_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		outputID: s.allocateOutputIndex(),
	}
	s.functionCalls = append(s.functionCalls, call)
	return call
}

func (s *responsesStreamRuntime) noteFunctionCallName(call *responsesFunctionCall, name string) {
	name = strings.TrimSpace(name)
	if name != "" && call.name == "" {
		call.name = name
	}
}

// functionNamesByIndex reports the names seen so far for the current round, which
// is what the incremental delta filter needs to drop stray fragments.
func (s *responsesStreamRuntime) functionNamesByIndex() map[int]string {
	names := make(map[int]string, len(s.functionRound))
	for idx, call := range s.functionRound {
		if strings.TrimSpace(call.name) != "" {
			names[idx] = call.name
		}
	}
	return names
}

// resetStreamToolCallState starts a new tool-call round: the next round numbers
// its calls from zero again, so the index -> call mapping is dropped. The calls
// themselves keep their ids, because the client was already told about them.
func (s *responsesStreamRuntime) resetStreamToolCallState() {
	s.functionRound = map[int]*responsesFunctionCall{}
}

func (s *responsesStreamRuntime) ensureFunctionItemAdded(callIndex int, name string) {
	call := s.callForIndex(callIndex)
	s.noteFunctionCallName(call, name)
	if call.added || strings.TrimSpace(call.name) == "" {
		return
	}
	item := map[string]any{
		"id":        call.itemID,
		"type":      "function_call",
		"call_id":   call.callID,
		"name":      call.name,
		"arguments": "",
		"status":    "in_progress",
	}
	s.sendEvent(
		"response.output_item.added",
		openaifmt.BuildResponsesOutputItemAddedPayload(s.responseID, call.itemID, call.outputID, item),
	)
	if toolcalldebug.Enabled() {
		toolcalldebug.Log("call_added", map[string]any{
			"req":       s.responseID,
			"surface":   "responses",
			"name":      call.name,
			"itemHash":  toolcalldebug.Hash(call.itemID),
			"outputID":  call.outputID,
			"roundIdx":  callIndex,
			"callCount": len(s.functionCalls),
		})
	}
	call.added = true
	s.toolCallsEmitted = true
}

func (s *responsesStreamRuntime) emitFunctionCallDeltaEvents(deltas []toolstream.ToolCallDelta) {
	for _, d := range deltas {
		call := s.callForIndex(d.Index)
		s.noteFunctionCallName(call, d.Name)
		s.ensureFunctionItemAdded(d.Index, d.Name)
		if strings.TrimSpace(d.Arguments) == "" {
			continue
		}
		call.args += d.Arguments
		s.sendEvent(
			"response.function_call_arguments.delta",
			openaifmt.BuildResponsesFunctionCallArgumentsDeltaPayload(s.responseID, call.itemID, call.outputID, call.callID, d.Arguments),
		)
	}
}

func (s *responsesStreamRuntime) emitFunctionCallDoneEvents(calls []toolcall.ParsedToolCall) {
	normalizedCalls := toolcall.NormalizeParsedToolCallsForSchemas(calls, s.toolsRaw)
	for idx, tc := range normalizedCalls {
		if strings.TrimSpace(tc.Name) == "" {
			continue
		}
		sig := functionCallSignature(tc)
		if epoch, ok := s.emittedCallEpoch[sig]; ok && epoch < s.replayEpoch {
			// The same call was already announced before the upstream replayed a
			// snapshot that was dropped, so this is the echo of that snapshot.
			if toolcalldebug.Enabled() {
				toolcalldebug.Log("call_echo_skipped", map[string]any{
					"req":     s.responseID,
					"surface": "responses",
					"name":    tc.Name,
					"sigHash": toolcalldebug.Hash(sig),
					"epoch":   epoch,
					"replay":  s.replayEpoch,
				})
			}
			continue
		}
		s.emittedCallEpoch[sig] = s.replayEpoch
		call := s.callForIndex(idx)
		s.noteFunctionCallName(call, tc.Name)
		s.ensureFunctionItemAdded(idx, tc.Name)
		if call.done {
			continue
		}
		argsBytes, _ := json.Marshal(tc.Input)
		args := string(argsBytes)
		call.args = args
		s.sendEvent(
			"response.function_call_arguments.done",
			openaifmt.BuildResponsesFunctionCallArgumentsDonePayload(s.responseID, call.itemID, call.outputID, call.callID, call.name, args),
		)
		item := map[string]any{
			"id":        call.itemID,
			"type":      "function_call",
			"call_id":   call.callID,
			"name":      call.name,
			"arguments": args,
			"status":    "completed",
		}
		s.sendEvent(
			"response.output_item.done",
			openaifmt.BuildResponsesOutputItemDonePayload(s.responseID, call.itemID, call.outputID, item),
		)
		call.done = true
		s.toolCallsDoneEmitted = true
		if toolcalldebug.Enabled() {
			toolcalldebug.Log("call_done", map[string]any{
				"req":       s.responseID,
				"surface":   "responses",
				"name":      call.name,
				"sigHash":   toolcalldebug.Hash(sig),
				"itemHash":  toolcalldebug.Hash(call.itemID),
				"roundIdx":  idx,
				"callCount": len(s.functionCalls),
			})
		}
	}
}

// finalFunctionCalls lines the calls parsed from the finished text up with the
// calls the client already received, so the completed object repeats the ids
// they were announced with. Calls that were never streamed get fresh ids.
func (s *responsesStreamRuntime) finalFunctionCalls(calls []toolcall.ParsedToolCall) []*responsesFunctionCall {
	normalizedCalls := toolcall.NormalizeParsedToolCallsForSchemas(calls, s.toolsRaw)
	out := make([]*responsesFunctionCall, 0, len(normalizedCalls))
	used := make([]bool, len(s.functionCalls))
	for _, tc := range normalizedCalls {
		name := strings.TrimSpace(tc.Name)
		if name == "" {
			continue
		}
		argsBytes, _ := json.Marshal(tc.Input)
		args := string(argsBytes)
		matchIdx := -1
		for i, call := range s.functionCalls {
			if used[i] || call.name != name {
				continue
			}
			if matchIdx < 0 {
				matchIdx = i
			}
			if call.args == args {
				matchIdx = i
				break
			}
		}
		if matchIdx >= 0 {
			used[matchIdx] = true
			call := s.functionCalls[matchIdx]
			call.args = args
			out = append(out, call)
			continue
		}
		call := s.newFunctionCall()
		call.name = name
		call.args = args
		call.added = true
		call.done = true
		out = append(out, call)
	}
	return out
}
