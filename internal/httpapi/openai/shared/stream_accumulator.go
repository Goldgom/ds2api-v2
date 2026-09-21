package shared

import (
	"strings"

	"ds2api/internal/sse"
	"ds2api/internal/toolcalldebug"
)

type StreamAccumulator struct {
	ThinkingEnabled       bool
	SearchEnabled         bool
	StripReferenceMarkers bool

	RawThinking           strings.Builder
	Thinking              strings.Builder
	ToolDetectionThinking strings.Builder
	RawText               strings.Builder
	Text                  strings.Builder

	// One replay tracker per accumulated stream. A continue round resends the
	// message, and the upstream may deliver that replay as many small deltas
	// that stay below the single-chunk snapshot floor.
	rawThinkingReplay sse.ReplayTracker
	thinkingReplay    sse.ReplayTracker
	detectReplay      sse.ReplayTracker
	rawTextReplay     sse.ReplayTracker
	textReplay        sse.ReplayTracker
}

type StreamPartDelta struct {
	Type         string
	RawText      string
	VisibleText  string
	CitationOnly bool
	// Replayed is set when this part replayed an already accumulated snapshot
	// and the diverged tail of the accumulated text was dropped.
	Replayed bool
}

type StreamAccumulatorResult struct {
	ContentSeen bool
	// Replayed is set when a chunk replayed a snapshot that diverged from the
	// accumulated text, so the tail of that text was dropped. State derived
	// from the dropped tail (such as a tool-call sieve) has to be rebuilt.
	Replayed bool
	Parts    []StreamPartDelta
}

func (a *StreamAccumulator) Apply(parsed sse.LineResult) StreamAccumulatorResult {
	out := StreamAccumulatorResult{}
	trace := toolcalldebug.Enabled()
	if trace {
		toolcalldebug.Log("line", map[string]any{
			"parts":       len(parsed.Parts),
			"detectParts": len(parsed.ToolDetectionThinkingParts),
			"rawTextLen":  a.RawText.Len(),
			"rawThinkLen": a.RawThinking.Len(),
		})
	}
	for _, p := range parsed.ToolDetectionThinkingParts {
		existing := a.ToolDetectionThinking.String()
		permissive := sse.ReplayMayStartWithoutMarkup(p)
		trimmed, replayed := a.detectReplay.ApplyToBuilder(&a.ToolDetectionThinking, p.Text, permissive)
		if replayed {
			out.Replayed = true
		}
		if trace {
			tracePart("detection_thinking", existing, p, permissive, trimmed, replayed)
		}
		if trimmed != "" {
			a.ToolDetectionThinking.WriteString(trimmed)
		}
	}
	for _, p := range parsed.Parts {
		if p.Type == "thinking" {
			existing := a.RawThinking.String()
			permissive := sse.ReplayMayStartWithoutMarkup(p)
			delta := a.applyThinkingPart(p.Text, permissive)
			if delta.Replayed {
				out.Replayed = true
			}
			if trace {
				tracePart("raw_thinking", existing, p, permissive, delta.RawText, delta.Replayed)
			}
			if delta.RawText != "" {
				out.ContentSeen = true
			}
			if delta.RawText != "" || delta.VisibleText != "" {
				out.Parts = append(out.Parts, delta)
			}
			continue
		}
		existing := a.RawText.String()
		permissive := sse.ReplayMayStartWithoutMarkup(p)
		delta := a.applyTextPart(p.Text, permissive)
		if delta.Replayed {
			out.Replayed = true
		}
		if trace {
			tracePart("raw_text", existing, p, permissive, delta.RawText, delta.Replayed)
		}
		if delta.RawText != "" {
			out.ContentSeen = true
		}
		if delta.RawText != "" || delta.VisibleText != "" || delta.CitationOnly {
			out.Parts = append(out.Parts, delta)
		}
	}
	return out
}

// tracePart records what one upstream part did to the accumulated text. The
// match numbers are the ones that matter when a duplicate call has to be
// explained: `prefixMatch` says how much of the part repeats the start of the
// accumulated text, and `interiorOffset` finds the same repetition when the
// part also carries the tail of the previous round in front of it. A part that
// duplicates text but appends it anyway is exactly how a replayed call block
// reaches the sieve twice.
func tracePart(channel, existing string, p sse.ContentPart, permissive bool, appendText string, dropped bool) {
	toolcalldebug.Log("part", map[string]any{
		"channel":         channel,
		"type":            p.Type,
		"snapshot":        p.Snapshot,
		"roundStart":      p.RoundStart,
		"permissive":      permissive,
		"existingLen":     len(existing),
		"partLen":         len(p.Text),
		"partHash":        toolcalldebug.Hash(p.Text),
		"partPreview":     toolcalldebug.Preview(p.Text, 60),
		"partInvocations": toolcalldebug.CountInvocations(p.Text),
		"prefixMatch":     toolcalldebug.PrefixMatchLen(existing, p.Text),
		"interiorOffset":  toolcalldebug.InteriorMatchOffset(existing, p.Text, 32, 4096),
		"appendLen":       len(appendText),
		"dropped":         dropped,
	})
}

func (a *StreamAccumulator) applyThinkingPart(text string, permissive bool) StreamPartDelta {
	replay := a.rawThinkingReplay.ResolveChunk(a.RawThinking.String(), text, permissive)
	if replay.Dropped {
		a.RawThinking.Reset()
		a.RawThinking.WriteString(replay.Kept)
		a.Thinking.Reset()
		a.Thinking.WriteString(CleanVisibleOutput(replay.Kept, a.StripReferenceMarkers))
		a.thinkingReplay.Reset()
	}
	if replay.Append == "" {
		return StreamPartDelta{Type: "thinking", Replayed: replay.Dropped}
	}
	a.RawThinking.WriteString(replay.Append)
	delta := StreamPartDelta{Type: "thinking", RawText: replay.Append, Replayed: replay.Dropped}
	if !a.ThinkingEnabled {
		return delta
	}
	cleanedText := CleanVisibleOutput(replay.Append, a.StripReferenceMarkers)
	if cleanedText == "" {
		return delta
	}
	visible := a.thinkingReplay.ResolveChunk(a.Thinking.String(), cleanedText, permissive)
	if visible.Append == "" {
		return delta
	}
	a.Thinking.WriteString(visible.Append)
	delta.VisibleText = visible.Append
	return delta
}

func (a *StreamAccumulator) applyTextPart(text string, permissive bool) StreamPartDelta {
	replay := a.rawTextReplay.ResolveChunk(a.RawText.String(), text, permissive)
	if replay.Dropped {
		a.RawText.Reset()
		a.RawText.WriteString(replay.Kept)
		a.Text.Reset()
		a.Text.WriteString(CleanVisibleOutput(replay.Kept, a.StripReferenceMarkers))
		a.textReplay.Reset()
	}
	if replay.Append == "" {
		return StreamPartDelta{Type: "text", Replayed: replay.Dropped}
	}
	a.RawText.WriteString(replay.Append)
	delta := StreamPartDelta{Type: "text", RawText: replay.Append, Replayed: replay.Dropped}
	if a.SearchEnabled && sse.IsCitation(replay.Append) {
		delta.CitationOnly = true
		return delta
	}
	cleanedText := CleanVisibleOutput(replay.Append, a.StripReferenceMarkers)
	visible := a.textReplay.ResolveChunk(a.Text.String(), cleanedText, permissive)
	if visible.Append == "" {
		return delta
	}
	a.Text.WriteString(visible.Append)
	delta.VisibleText = visible.Append
	return delta
}
