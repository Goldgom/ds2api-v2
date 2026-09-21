package responses

import (
	"ds2api/internal/assistantturn"
	"ds2api/internal/toolcall"
	"net/http"
	"strings"

	"ds2api/internal/config"
	openaifmt "ds2api/internal/format/openai"
	"ds2api/internal/httpapi/openai/shared"
	"ds2api/internal/promptcompat"
	"ds2api/internal/responsehistory"
	"ds2api/internal/sse"
	streamengine "ds2api/internal/stream"
	"ds2api/internal/toolcalldebug"
	"ds2api/internal/toolstream"
)

type responsesStreamRuntime struct {
	w        http.ResponseWriter
	rc       *http.ResponseController
	canFlush bool

	responseID    string
	model         string
	finalPrompt   string
	refFileTokens int
	toolNames     []string
	toolsRaw      any
	traceID       string
	toolChoice    promptcompat.ToolChoicePolicy

	thinkingEnabled       bool
	searchEnabled         bool
	stripReferenceMarkers bool

	bufferToolContent    bool
	emitEarlyToolDeltas  bool
	toolCallsEmitted     bool
	toolCallsDoneEmitted bool

	sieve             toolstream.State
	accumulator       shared.StreamAccumulator
	visibleText       strings.Builder
	responseMessageID int
	upstreamErr       string

	// functionCalls holds one entry per tool call the client was told about, in
	// the order it was told, and functionRound maps the index inside the current
	// upstream round to that entry. Keeping the ids on the call instead of on the
	// round index is what keeps the streamed output_item events and the
	// response.completed object describing the same call with the same ids: a
	// client that reads both must not see one call twice.
	functionCalls []*responsesFunctionCall
	functionRound map[int]*responsesFunctionCall
	// emittedCallEpoch records the replay epoch a call was emitted in, so a call
	// that reappears after a dropped snapshot replay is skipped as an echo.
	emittedCallEpoch  map[string]uint64
	replayEpoch       uint64
	messageItemID     string
	messageOutputID   int
	nextOutputID      int
	messageAdded      bool
	messagePartAdded  bool
	sequence          int
	failed            bool
	finalErrorStatus  int
	finalErrorMessage string
	finalErrorCode    string

	persistResponse func(obj map[string]any)
	history         *responsehistory.Session
}

func newResponsesStreamRuntime(
	w http.ResponseWriter,
	rc *http.ResponseController,
	canFlush bool,
	responseID string,
	model string,
	finalPrompt string,
	thinkingEnabled bool,
	searchEnabled bool,
	stripReferenceMarkers bool,
	toolNames []string,
	toolsRaw any,
	bufferToolContent bool,
	emitEarlyToolDeltas bool,
	toolChoice promptcompat.ToolChoicePolicy,
	traceID string,
	persistResponse func(obj map[string]any),
	history *responsehistory.Session,
) *responsesStreamRuntime {
	runtime := &responsesStreamRuntime{
		w:                     w,
		rc:                    rc,
		canFlush:              canFlush,
		responseID:            responseID,
		model:                 model,
		finalPrompt:           finalPrompt,
		thinkingEnabled:       thinkingEnabled,
		searchEnabled:         searchEnabled,
		stripReferenceMarkers: stripReferenceMarkers,
		toolNames:             toolNames,
		toolsRaw:              toolsRaw,
		bufferToolContent:     bufferToolContent,
		emitEarlyToolDeltas:   emitEarlyToolDeltas,
		functionRound:         map[int]*responsesFunctionCall{},
		emittedCallEpoch:      map[string]uint64{},
		messageOutputID:       -1,
		toolChoice:            toolChoice,
		traceID:               traceID,
		persistResponse:       persistResponse,
		history:               history,
		accumulator: shared.StreamAccumulator{
			TraceID:               responseID,
			ThinkingEnabled:       thinkingEnabled,
			SearchEnabled:         searchEnabled,
			StripReferenceMarkers: stripReferenceMarkers,
		},
	}
	if toolcalldebug.Enabled() {
		// The shared accumulator traces responses-surface streams too; without
		// this marker a part record could not be attributed to a surface.
		toolcalldebug.Log("request_start", map[string]any{
			"req":                 responseID,
			"surface":             "responses",
			"model":               model,
			"promptHash":          toolcalldebug.Hash(finalPrompt),
			"promptLen":           len(finalPrompt),
			"traceID":             traceID,
			"thinkingEnabled":     thinkingEnabled,
			"bufferToolContent":   bufferToolContent,
			"emitEarlyToolDeltas": emitEarlyToolDeltas,
			"toolNames":           len(toolNames),
			"toolsRawPresent":     toolsRaw != nil,
			"toolChoiceRequired":  toolChoice.IsRequired(),
		})
	}
	return runtime
}

func (s *responsesStreamRuntime) failResponse(status int, message, code string) {
	s.failed = true
	s.finalErrorStatus = status
	s.finalErrorMessage = message
	s.finalErrorCode = code
	failedResp := map[string]any{
		"id":          s.responseID,
		"type":        "response",
		"object":      "response",
		"model":       s.model,
		"status":      "failed",
		"status_code": status,
		"output":      []any{},
		"output_text": "",
		"error": map[string]any{
			"message": message,
			"type":    openAIErrorType(status),
			"code":    code,
			"param":   nil,
		},
	}
	if s.persistResponse != nil {
		s.persistResponse(failedResp)
	}
	if s.history != nil {
		s.history.Error(status, message, code, responsehistory.ThinkingForArchive(s.accumulator.RawThinking.String(), s.accumulator.ToolDetectionThinking.String(), s.accumulator.Thinking.String()), responsehistory.TextForArchive(s.accumulator.RawText.String(), s.accumulator.Text.String()))
	}
	s.sendEvent("response.failed", openaifmt.BuildResponsesFailedPayload(s.responseID, s.model, status, message, code))
	s.sendDone()
}

func (s *responsesStreamRuntime) markContextCancelled() {
	s.failed = true
	s.finalErrorStatus = 499
	s.finalErrorMessage = "request context cancelled"
	s.finalErrorCode = string(streamengine.StopReasonContextCancelled)
}

func (s *responsesStreamRuntime) finalize(finishReason string, deferEmptyOutput bool) bool {
	s.failed = false
	s.finalErrorStatus = 0
	s.finalErrorMessage = ""
	s.finalErrorCode = ""
	if s.bufferToolContent {
		s.processToolStreamEvents(toolstream.Flush(&s.sieve, s.toolNames), true, true)
	}

	finalThinking := s.accumulator.Thinking.String()
	finalToolDetectionThinking := s.accumulator.ToolDetectionThinking.String()
	finalText := s.accumulator.Text.String()
	turn := assistantturn.BuildTurnFromStreamSnapshot(assistantturn.StreamSnapshot{
		RawText:               s.accumulator.RawText.String(),
		VisibleText:           finalText,
		RawThinking:           s.accumulator.RawThinking.String(),
		VisibleThinking:       finalThinking,
		DetectionThinking:     finalToolDetectionThinking,
		ContentFilter:         finishReason == "content_filter",
		UpstreamError:         s.upstreamErr,
		ResponseMessageID:     s.responseMessageID,
		AlreadyEmittedCalls:   s.toolCallsEmitted,
		AlreadyEmittedToolRaw: s.toolCallsDoneEmitted,
	}, assistantturn.BuildOptions{
		Model:                 s.model,
		Prompt:                s.finalPrompt,
		RefFileTokens:         s.refFileTokens,
		SearchEnabled:         s.searchEnabled,
		StripReferenceMarkers: s.stripReferenceMarkers,
		ToolNames:             s.toolNames,
		ToolsRaw:              s.toolsRaw,
		ToolChoice:            s.toolChoice,
	})
	textParsed := turn.ParsedToolCalls
	detected := turn.ToolCalls
	s.logToolPolicyRejections(textParsed)

	if len(detected) > 0 {
		s.toolCallsEmitted = true
		if !s.toolCallsDoneEmitted {
			s.emitFunctionCallDoneEvents(detected)
		}
	}

	s.closeMessageItem()

	outcome := assistantturn.FinalizeTurn(turn, assistantturn.FinalizeOptions{
		AlreadyEmittedToolCalls: s.toolCallsEmitted || s.toolCallsDoneEmitted,
	})
	if outcome.ShouldFail {
		status, message, code := outcome.Error.Status, outcome.Error.Message, outcome.Error.Code
		if deferEmptyOutput {
			s.finalErrorStatus = status
			s.finalErrorMessage = message
			s.finalErrorCode = code
			return false
		}
		s.failResponse(status, message, code)
		return true
	}
	s.closeIncompleteFunctionItems()

	obj := s.buildCompletedResponseObject(turn.Thinking, turn.Text, detected)
	if s.persistResponse != nil {
		s.persistResponse(obj)
	}
	if s.history != nil {
		s.history.Success(
			http.StatusOK,
			responsehistory.ThinkingForArchive(turn.RawThinking, turn.DetectionThinking, turn.Thinking),
			responsehistory.TextForArchive(turn.RawText, turn.Text),
			outcome.FinishReason,
			assistantturn.OpenAIResponsesUsage(turn),
		)
	}
	s.sendEvent("response.completed", openaifmt.BuildResponsesCompletedPayload(obj))
	s.sendDone()
	return true
}

func (s *responsesStreamRuntime) logToolPolicyRejections(textParsed toolcall.ToolCallParseResult) {
	logRejected := func(parsed toolcall.ToolCallParseResult, channel string) {
		rejected := filteredRejectedToolNamesForLog(parsed.RejectedToolNames)
		if !parsed.RejectedByPolicy || len(rejected) == 0 {
			return
		}
		config.Logger.Warn(
			"[responses] rejected tool calls by policy",
			"trace_id", strings.TrimSpace(s.traceID),
			"channel", channel,
			"tool_choice_mode", s.toolChoice.Mode,
			"rejected_tool_names", strings.Join(rejected, ","),
		)
	}
	logRejected(textParsed, "text")
}

func (s *responsesStreamRuntime) onParsed(parsed sse.LineResult) streamengine.ParsedDecision {
	if !parsed.Parsed {
		return streamengine.ParsedDecision{}
	}
	if parsed.ResponseMessageID > 0 {
		s.responseMessageID = parsed.ResponseMessageID
	}
	if parsed.ErrorMessage != "" {
		s.upstreamErr = parsed.ErrorMessage
		return streamengine.ParsedDecision{Stop: true, StopReason: streamengine.StopReason("upstream_error")}
	}
	if parsed.ContentFilter {
		return streamengine.ParsedDecision{Stop: true, StopReason: streamengine.StopReason("content_filter")}
	}
	if parsed.Stop {
		return streamengine.ParsedDecision{Stop: true}
	}

	batch := responsesDeltaBatch{runtime: s}
	accumulated := s.accumulator.Apply(parsed)
	if accumulated.Replayed {
		// The upstream replayed a snapshot that diverged from the accumulated
		// text, so the accumulator dropped the stale tail. Sieve state derived
		// from that tail must be dropped with it, and the calls that were already
		// emitted before this point are echoes from now on.
		s.sieve = toolstream.State{}
		s.replayEpoch++
	}
	for _, p := range accumulated.Parts {
		if p.Type == "thinking" {
			batch.append("reasoning", p.VisibleText)
			continue
		}
		if p.RawText == "" {
			continue
		}
		if p.CitationOnly {
			continue
		}
		if !s.bufferToolContent {
			batch.append("text", p.VisibleText)
			continue
		}
		batch.flush()
		s.processToolStreamEvents(toolstream.ProcessChunk(&s.sieve, p.RawText, s.toolNames), true, true)
	}

	batch.flush()
	if s.history != nil {
		s.history.Progress(
			responsehistory.ThinkingForArchive(s.accumulator.RawThinking.String(), s.accumulator.ToolDetectionThinking.String(), s.accumulator.Thinking.String()),
			responsehistory.TextForArchive(s.accumulator.RawText.String(), s.accumulator.Text.String()),
		)
	}
	return streamengine.ParsedDecision{ContentSeen: accumulated.ContentSeen}
}
