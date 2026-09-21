package shared

import (
	"testing"

	"ds2api/internal/sse"
)

func TestStreamAccumulatorAppliesThinkingAndTextDedupe(t *testing.T) {
	acc := StreamAccumulator{ThinkingEnabled: true, StripReferenceMarkers: true}
	thinkingPrefix := "this is a long thinking snapshot prefix used by DeepSeek continue replay"
	textPrefix := "this is a long visible answer snapshot prefix used by DeepSeek continue replay"
	first := acc.Apply(sse.LineResult{
		Parsed: true,
		Parts: []sse.ContentPart{
			{Type: "thinking", Text: thinkingPrefix},
			{Type: "text", Text: textPrefix},
		},
	})
	second := acc.Apply(sse.LineResult{
		Parsed: true,
		Parts: []sse.ContentPart{
			{Type: "thinking", Text: thinkingPrefix + " next"},
			{Type: "text", Text: textPrefix + " world"},
		},
	})

	if !first.ContentSeen || !second.ContentSeen {
		t.Fatalf("expected both chunks to mark content seen")
	}
	if got := acc.RawThinking.String(); got != thinkingPrefix+" next" {
		t.Fatalf("raw thinking = %q", got)
	}
	if got := acc.Thinking.String(); got != thinkingPrefix+" next" {
		t.Fatalf("thinking = %q", got)
	}
	if got := acc.RawText.String(); got != textPrefix+" world" {
		t.Fatalf("raw text = %q", got)
	}
	if got := acc.Text.String(); got != textPrefix+" world" {
		t.Fatalf("text = %q", got)
	}
	if got := second.Parts[0].VisibleText; got != " next" {
		t.Fatalf("thinking delta = %q", got)
	}
	if got := second.Parts[1].VisibleText; got != " world" {
		t.Fatalf("text delta = %q", got)
	}
}

func TestStreamAccumulatorKeepsHiddenThinkingForToolDetection(t *testing.T) {
	acc := StreamAccumulator{ThinkingEnabled: false, StripReferenceMarkers: true}
	result := acc.Apply(sse.LineResult{
		Parsed: true,
		Parts: []sse.ContentPart{
			{Type: "thinking", Text: "<tool_calls></tool_calls>"},
		},
		ToolDetectionThinkingParts: []sse.ContentPart{
			{Type: "thinking", Text: "detect"},
			{Type: "thinking", Text: " tools"},
		},
	})

	if !result.ContentSeen {
		t.Fatalf("expected hidden thinking to count as upstream content")
	}
	if got := acc.RawThinking.String(); got != "<tool_calls></tool_calls>" {
		t.Fatalf("raw thinking = %q", got)
	}
	if got := acc.Thinking.String(); got != "" {
		t.Fatalf("visible thinking = %q", got)
	}
	if got := acc.ToolDetectionThinking.String(); got != "detect tools" {
		t.Fatalf("tool detection thinking = %q", got)
	}
}

func TestStreamAccumulatorSuppressesCitationTextWhenSearchEnabled(t *testing.T) {
	acc := StreamAccumulator{SearchEnabled: true, StripReferenceMarkers: true}
	result := acc.Apply(sse.LineResult{
		Parsed: true,
		Parts:  []sse.ContentPart{{Type: "text", Text: "[citation:1]"}},
	})

	if !result.ContentSeen {
		t.Fatalf("expected citation chunk to mark upstream content")
	}
	if len(result.Parts) != 1 || !result.Parts[0].CitationOnly {
		t.Fatalf("expected citation-only delta, got %#v", result.Parts)
	}
	if got := acc.RawText.String(); got != "[citation:1]" {
		t.Fatalf("raw text = %q", got)
	}
	if got := acc.Text.String(); got != "" {
		t.Fatalf("visible text = %q", got)
	}
}

func TestStreamAccumulatorStripsInlineCitationAndReferenceMarkers(t *testing.T) {
	acc := StreamAccumulator{SearchEnabled: true, StripReferenceMarkers: true}
	result := acc.Apply(sse.LineResult{
		Parsed: true,
		Parts:  []sse.ContentPart{{Type: "text", Text: "广州天气[citation:1] 多云[reference:0]"}},
	})

	if !result.ContentSeen {
		t.Fatalf("expected marker chunk to mark upstream content")
	}
	if got := acc.Text.String(); got != "广州天气 多云" {
		t.Fatalf("visible text = %q", got)
	}
	if len(result.Parts) != 1 || result.Parts[0].VisibleText != "广州天气 多云" {
		t.Fatalf("unexpected parts: %#v", result.Parts)
	}
}

// A chunk that only overlaps the accumulated text is never treated as a render
// of it: dedupe is one-sided, so a message that repeats the same markup (the
// next tool-call block, a repeated parameter block) can never make the
// accumulator drop content it already holds.
func TestStreamAccumulatorAppendsWithoutLosingRepeatedHeadText(t *testing.T) {
	acc := StreamAccumulator{}
	head := "我们被问到：这是一个很长的续答快照前缀，用来验证去重逻辑不会误伤正常 token。"

	first := acc.Apply(sse.LineResult{
		Parsed: true,
		Parts:  []sse.ContentPart{{Type: "text", Text: head + "旧的结尾"}},
	})
	if first.Replayed {
		t.Fatalf("expected the first chunk to be an ordinary increment")
	}

	second := acc.Apply(sse.LineResult{
		Parsed: true,
		Parts:  []sse.ContentPart{{Type: "text", Text: head + "重写后的结尾"}},
	})
	if second.Replayed {
		t.Fatalf("expected no rewind for a chunk that merely shares a head, got %#v", second)
	}
	want := head + "旧的结尾" + head + "重写后的结尾"
	if got := acc.RawText.String(); got != want {
		t.Fatalf("raw text = %q, want %q", got, want)
	}
	if got := acc.Text.String(); got != want {
		t.Fatalf("visible text = %q, want %q", got, want)
	}
}

// A whole message state that the upstream re-sent is dropped when everything it
// carries is already accumulated, and only its new tail is appended when it grew.
func TestStreamAccumulatorDropsReplayedSnapshotAndAppendsNewTail(t *testing.T) {
	acc := StreamAccumulator{}
	head := "我们被问到：这是一个很长的续答快照前缀，用来验证去重逻辑不会误伤正常 token。"
	acc.Apply(sse.LineResult{Parsed: true, Parts: []sse.ContentPart{{Type: "text", Text: head}}})

	replayed := acc.Apply(sse.LineResult{Parsed: true, Parts: []sse.ContentPart{{Type: "text", Text: head, Snapshot: true}}})
	if got := acc.RawText.String(); got != head {
		t.Fatalf("expected the replayed snapshot to be dropped, got %q", got)
	}
	if len(replayed.Parts) != 0 {
		t.Fatalf("expected no delta for a fully replayed snapshot, got %#v", replayed.Parts)
	}

	grown := acc.Apply(sse.LineResult{Parsed: true, Parts: []sse.ContentPart{{Type: "text", Text: head + " 新的结尾。", Snapshot: true}}})
	if got := acc.RawText.String(); got != head+" 新的结尾。" {
		t.Fatalf("expected only the new tail to be appended, got %q", got)
	}
	if len(grown.Parts) != 1 || grown.Parts[0].VisibleText != " 新的结尾。" {
		t.Fatalf("expected the new tail as the delta, got %#v", grown.Parts)
	}
}
