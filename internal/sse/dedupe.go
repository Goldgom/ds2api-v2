package sse

import (
	"strings"
	"unicode/utf8"
)

// minContinuationSnapshotLen is the smallest chunk size, in runes, that can be
// treated as a continuation snapshot replay on its own. Shorter chunks are
// usually ordinary increments: a short chunk that repeats earlier text is far
// more likely to be legitimate content than a replayed snapshot.
const minContinuationSnapshotLen = 32

const (
	// minReplayCandidateRunes is the smallest chunk that may open an alignment.
	// The upstream splits a replayed message freely - a snapshot into fragments,
	// a fragment into tokens - so a replay is often delivered as many parts that
	// all stay below the snapshot floor.
	minReplayCandidateRunes = 8
)

// toolMarkupHints are fragments that only ever occur inside tool-call markup.
var toolMarkupHints = []string{
	"<tool", "</tool", "tool_calls", "EPSE",
	"<invoke", "</invoke", "invoke name", "invoke|name",
	"<parameter", "</parameter", "parameter name", "parameter|name",
}

func containsToolCallMarkup(s string) bool {
	for _, hint := range toolMarkupHints {
		if strings.Contains(s, hint) {
			return true
		}
	}
	return false
}

// ContinuationReplay describes how an incoming chunk relates to the text that
// was already accumulated for the same upstream message.
//
// DeepSeek resends the whole message as a snapshot when a stream opens and at
// the start of every `continue` round. Such a snapshot overlaps text that was
// already accumulated, so appending it verbatim duplicates everything it
// replays - including tool-call markup, which the tool sieve then parses a
// second time and emits as another tool call with a fresh id.
//
// Snapshots are recognised structurally, not by comparing text: the parser
// marks the chunk as a snapshot (ContentPart.Snapshot) and the transport keeps
// it in a chunk of its own, so a snapshot is compared with the accumulated text
// as a whole instead of being mixed with buffered increments.
type ContinuationReplay struct {
	// Kept replaces the accumulated text. It is always either that text itself
	// or that text without the tail that was appended from an earlier chunk of
	// the same replay (see ReplayTracker); accumulated text is never cut.
	Kept string
	// Append is the text to append after Kept. It holds every byte that is new,
	// so incremental consumers such as the tool sieve must process Append.
	Append string
	// Dropped reports that a replay that had already been consumed was taken
	// back, so state derived from it (such as a tool-call sieve) has to be
	// rebuilt.
	Dropped bool
}

// ResolveContinuationReplay decides how much of an incoming chunk is new.
//
// The rules are deliberately one-sided: they only ever drop text the incoming
// chunk replayed, never cut the accumulated text apart. Cutting content that was
// only *guessed* to be a replay is what made the tool sieve hand one call the
// arguments of another, and neither a missed replay nor a false one may destroy
// content. A message made of several similar tool-call blocks shares a long
// textual prologue with its own next block, so overlap alone is never evidence
// that the message is being re-rendered.
//
// Whole message states that the upstream re-sent are recognised structurally
// instead: the parser marks them (ContentPart.Snapshot) and the transport keeps
// them in a chunk of their own, so their text can be compared with the
// accumulated text as a whole. A re-rendered snapshot that keeps everything
// already accumulated is therefore handled by the prefix rules below, and one
// that drops something can only append - state that was already streamed can
// not be retracted anyway.
func ResolveContinuationReplay(existing, incoming string) ContinuationReplay {
	keepAll := ContinuationReplay{Kept: existing}
	if incoming == "" {
		return keepAll
	}
	if existing == "" {
		return ContinuationReplay{Kept: "", Append: incoming}
	}
	if utf8.RuneCountInString(incoming) < minContinuationSnapshotLen {
		return ContinuationReplay{Kept: existing, Append: incoming}
	}
	// Snapshot replay that grew while we streamed: keep only the new tail.
	if strings.HasPrefix(incoming, existing) {
		return ContinuationReplay{Kept: existing, Append: incoming[len(existing):]}
	}
	// Snapshot replay that is stale or identical: everything it carries is
	// already accumulated, so nothing is added.
	if strings.HasPrefix(existing, incoming) {
		return keepAll
	}
	return ContinuationReplay{Kept: existing, Append: incoming}
}

// sharedRunePrefixLen returns the length in bytes of the longest common prefix
// of a and b, rounded down to a rune boundary of b.
func sharedRunePrefixLen(a, b string) int {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	i := 0
	for i < limit && a[i] == b[i] {
		i++
	}
	if i == limit {
		return i
	}
	for i > 0 && !utf8.RuneStart(b[i]) {
		i--
	}
	return i
}

// TrimContinuationOverlap returns only the part of incoming that should be
// appended to the accumulated text.
//
// Callers that own a mutable buffer must use TrimContinuationReplayFromBuilder
// (or ResolveContinuationReplay) instead: when a replay that was already
// consumed is taken back, the tail it contributed has to be dropped as well,
// which this wrapper cannot do.
func TrimContinuationOverlap(existing, incoming string) string {
	return ResolveContinuationReplay(existing, incoming).Append
}

// ReplayTracker drops continuation snapshot replays from a text that grows over
// several upstream chunks.
//
// ResolveContinuationReplay can only recognise a replay that is contained in a
// single chunk. A `continue` round that resends the message token by token never
// produces such a chunk: every fragment stays below the snapshot floor, so it
// was appended again and again - including the tool-call markup inside it, which
// the tool sieve then parsed a second time and emitted as another tool call.
//
// The tracker keeps an alignment against the text accumulated before the replay
// started, so the replayed fragments can be recognised whatever size they are
// delivered in.
type ReplayTracker struct {
	// active is set while a replay is being aligned.
	active bool
	// confirmed is set once the chunk after the opening candidate matched the
	// aligned position.
	confirmed bool
	// base is the length of the accumulated text at the start of the replayed
	// region, i.e. the length to rewind to when the replay is confirmed.
	base int
	// offset is the next expected byte offset inside the accumulated text.
	offset int
}

// Reset forgets the current alignment. Callers must call it whenever they
// replace the accumulated text themselves.
func (t *ReplayTracker) Reset() {
	if t == nil {
		return
	}
	*t = ReplayTracker{}
}

// Resolve decides how much of an incoming chunk is new.
func (t *ReplayTracker) Resolve(existing, incoming string) ContinuationReplay {
	return t.ResolveChunk(existing, incoming, false)
}

// ResolveSnapshot decides how much of an incoming whole-message state is new.
//
// Such a part may be a replayed fragment batch whose head carries no tool-call
// markup: the upstream resends the message one fragment at a time, and the
// calls only appear in the later fragments.
func (t *ReplayTracker) ResolveSnapshot(existing, incoming string) ContinuationReplay {
	return t.ResolveChunk(existing, incoming, true)
}

// ResolveChunk decides how much of an incoming chunk is new. snapshot reports
// that the chunk carries a whole message state (see ContentPart.Snapshot).
func (t *ReplayTracker) ResolveChunk(existing, incoming string, snapshot bool) ContinuationReplay {
	if t == nil {
		return ResolveContinuationReplay(existing, incoming)
	}
	if incoming == "" {
		return ContinuationReplay{Kept: existing}
	}
	if existing == "" {
		t.Reset()
		return ContinuationReplay{Kept: "", Append: incoming}
	}
	if t.active {
		if replay, ok := t.followReplay(existing, incoming); ok {
			return replay
		}
	}
	replay := ResolveContinuationReplay(existing, incoming)
	t.openAlignment(existing, incoming, replay, snapshot)
	return replay
}

// followReplay consumes a chunk that continues an open alignment. It reports
// false when the alignment does not hold, leaving the chunk to the chunk-local
// rules.
func (t *ReplayTracker) followReplay(existing, incoming string) (ContinuationReplay, bool) {
	if t.offset > len(existing) {
		// The text was rewound below the aligned position, so the alignment no
		// longer describes it.
		t.Reset()
		return ContinuationReplay{}, false
	}
	matched := sharedRunePrefixLen(existing[t.offset:], incoming)
	if matched == len(incoming) {
		t.offset += matched
		if !t.confirmed {
			t.confirmed = true
			if t.base < len(existing) {
				// The chunk that opened the alignment was replayed content
				// after all, so drop it together with this fragment.
				return ContinuationReplay{Kept: existing[:t.base], Dropped: true}, true
			}
		}
		return ContinuationReplay{Kept: existing}, true
	}
	if t.confirmed && matched > 0 {
		// The replayed fragment stopped matching the accumulated text. Only the
		// matched prefix is known to be duplicated, so keep the accumulated text
		// untouched and continue with the rest of the chunk. Cutting the text
		// here would destroy content that was only guessed to be a replay.
		replay := ContinuationReplay{Kept: existing, Append: incoming[matched:]}
		t.Reset()
		return replay, true
	}
	t.Reset()
	return ContinuationReplay{}, false
}

// openAlignment starts an alignment when incoming looks like the first part of a
// message the upstream is resending.
//
// The evidence is deliberately narrow, because both a missed replay and a false
// one are costly and the two are hard to tell apart in text:
//
//   - the chunk must restart the message, i.e. it must be a prefix of the
//     accumulated text. A `continue` round that resends the message always
//     starts that way, whether it resends it as one snapshot or as one part per
//     fragment.
//   - it must be long enough to align on (`minReplayCandidateRunes`).
//   - an ordinary increment must also carry tool-call markup. Legitimately
//     repeated output (a run of identical characters, a repeated table row, a
//     repeated parameter block) is byte-identical to a replayed prose fragment,
//     so plain deltas can never be treated as a replay on their own. A part that
//     carries a whole message state may: the upstream resends the message one
//     fragment at a time and the calls only appear in the later fragments, so
//     refusing to align on a markup-free head would let every later fragment be
//     appended again - which is how the same call gets emitted twice.
//
// Matching an interior offset is not accepted either (a chunk that only appears
// somewhere inside the message): repeated parameter text inside one message
// triggers that, and a confirmed alignment rewinds the accumulated text.
//
// A chunk that was appended verbatim stays appended until the next chunk
// confirms the replay, so a false candidate costs nothing.
func (t *ReplayTracker) openAlignment(existing, incoming string, replay ContinuationReplay, snapshot bool) {
	if replay.Dropped {
		return
	}
	appendVerbatim := replay.Append == incoming
	droppedWhole := replay.Append == "" && replay.Kept == existing
	if !appendVerbatim && !droppedWhole {
		return
	}
	if utf8.RuneCountInString(incoming) < minReplayCandidateRunes {
		return
	}
	if !snapshot && !containsToolCallMarkup(incoming) {
		return
	}
	if !strings.HasPrefix(existing, incoming) {
		return
	}
	t.active, t.confirmed = true, false
	t.base, t.offset = len(existing), len(incoming)
}

// ApplyToBuilder resolves incoming against existing, rewinding existing when the
// replay invalidated the tail it already held, and returns the text to append.
// snapshot reports that incoming carries a whole message state.
func (t *ReplayTracker) ApplyToBuilder(existing *strings.Builder, incoming string, snapshot bool) (appendText string, dropped bool) {
	if existing == nil {
		return incoming, false
	}
	replay := t.ResolveChunk(existing.String(), incoming, snapshot)
	if replay.Dropped {
		existing.Reset()
		existing.WriteString(replay.Kept)
	}
	return replay.Append, replay.Dropped
}

// ApplyContinuationReplay returns the accumulated text after applying incoming,
// dropping the stale tail of existing when incoming replayed a diverged
// snapshot.
func ApplyContinuationReplay(existing, incoming string) string {
	replay := ResolveContinuationReplay(existing, incoming)
	return replay.Kept + replay.Append
}

// TrimContinuationOverlapFromBuilder appends the new part of incoming to
// existing, rewinding existing first when the incoming chunk replayed a
// snapshot that diverged from it. It returns the text that was appended.
func TrimContinuationOverlapFromBuilder(existing *strings.Builder, incoming string) string {
	appendText, _ := TrimContinuationReplayFromBuilder(existing, incoming)
	return appendText
}

// TrimContinuationReplayFromBuilder behaves like TrimContinuationOverlapFromBuilder
// and also reports whether existing was rewound, which means any state derived
// from the dropped tail has to be discarded together with it.
func TrimContinuationReplayFromBuilder(existing *strings.Builder, incoming string) (appendText string, rewound bool) {
	if existing == nil {
		return incoming, false
	}
	replay := ResolveContinuationReplay(existing.String(), incoming)
	if replay.Dropped {
		existing.Reset()
		existing.WriteString(replay.Kept)
	}
	return replay.Append, replay.Dropped
}
