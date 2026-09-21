package responses

import (
	"ds2api/internal/toolcall"
	"sort"
	"strings"

	openaifmt "ds2api/internal/format/openai"
)

// closeIncompleteFunctionItems finishes the calls the client was already told
// about but that never received their arguments. Every event it sends reuses the
// ids of the announced call.
func (s *responsesStreamRuntime) closeIncompleteFunctionItems() {
	for _, call := range s.functionCalls {
		if !call.added || call.done {
			continue
		}
		name := strings.TrimSpace(call.name)
		if name == "" {
			continue
		}
		args := strings.TrimSpace(call.args)
		if args == "" {
			args = "{}"
		}
		call.args = args
		s.sendEvent(
			"response.function_call_arguments.done",
			openaifmt.BuildResponsesFunctionCallArgumentsDonePayload(s.responseID, call.itemID, call.outputID, call.callID, name, args),
		)
		item := map[string]any{
			"id":        call.itemID,
			"type":      "function_call",
			"call_id":   call.callID,
			"name":      name,
			"arguments": args,
			"status":    "completed",
		}
		s.sendEvent(
			"response.output_item.done",
			openaifmt.BuildResponsesOutputItemDonePayload(s.responseID, call.itemID, call.outputID, item),
		)
		call.done = true
		s.toolCallsDoneEmitted = true
	}
}

func (s *responsesStreamRuntime) buildCompletedResponseObject(finalThinking, finalText string, calls []toolcall.ParsedToolCall) map[string]any {
	type indexedItem struct {
		index int
		item  map[string]any
	}
	indexed := make([]indexedItem, 0, len(calls)+1)

	if len(calls) > 0 {
		// Reserve the message slot before the calls so a call that was streamed
		// earlier does not push the message item behind it in the output order.
		s.ensureMessageOutputIndex()
	}

	if s.messageAdded {
		text := s.visibleText.String()
		indexed = append(indexed, indexedItem{
			index: s.ensureMessageOutputIndex(),
			item: map[string]any{
				"id":     s.ensureMessageItemID(),
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []map[string]any{
					{
						"type": "output_text",
						"text": text,
					},
				},
			},
		})
	} else if len(calls) > 0 && strings.TrimSpace(finalThinking) != "" {
		indexed = append(indexed, indexedItem{
			index: s.ensureMessageOutputIndex(),
			item: map[string]any{
				"id":     s.ensureMessageItemID(),
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []map[string]any{
					{
						"type": "reasoning",
						"text": finalThinking,
					},
				},
			},
		})
	} else if len(calls) == 0 {
		content := make([]map[string]any, 0, 2)
		if finalThinking != "" {
			content = append(content, map[string]any{
				"type": "reasoning",
				"text": finalThinking,
			})
		}
		if finalText != "" {
			content = append(content, map[string]any{
				"type": "output_text",
				"text": finalText,
			})
		}
		if len(content) > 0 {
			indexed = append(indexed, indexedItem{
				index: s.ensureMessageOutputIndex(),
				item: map[string]any{
					"id":      s.ensureMessageItemID(),
					"type":    "message",
					"role":    "assistant",
					"status":  "completed",
					"content": content,
				},
			})
		}
	}

	normalizedCalls := toolcall.NormalizeParsedToolCallsForSchemas(calls, s.toolsRaw)
	for _, call := range s.finalFunctionCalls(normalizedCalls) {
		indexed = append(indexed, indexedItem{
			index: call.outputID,
			item: map[string]any{
				"id":        call.itemID,
				"type":      "function_call",
				"call_id":   call.callID,
				"name":      call.name,
				"arguments": call.args,
				"status":    "completed",
			},
		})
	}

	sort.SliceStable(indexed, func(i, j int) bool {
		return indexed[i].index < indexed[j].index
	})
	output := make([]any, 0, len(indexed))
	for _, it := range indexed {
		output = append(output, it.item)
	}

	outputText := s.visibleText.String()
	if outputText == "" && len(calls) == 0 {
		if finalText != "" {
			outputText = finalText
		} else if finalThinking != "" {
			outputText = finalThinking
		}
	}

	obj := openaifmt.BuildResponseObjectFromItems(
		s.responseID,
		s.model,
		s.finalPrompt,
		finalThinking,
		finalText,
		output,
		outputText,
	)
	if s.refFileTokens > 0 {
		addRefFileTokensToUsage(obj, s.refFileTokens)
	}
	return obj
}
