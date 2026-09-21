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
	// minReproducedTrustRunes is how much of the accumulated text a replay has to
	// reproduce before it is trusted and the reproduced copy is dropped. Below it
	// the chunks are appended as usual, so a false candidate costs nothing, and a
	// short accidental repeat (a repeated word, a repeated row) never gets
	// dropped.
	minReproducedTrustRunes = 16
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

// ReplayMayStartWithoutMarkup reports that a part may open a replay alignment
// even when it carries no tool-call markup: it either carries whole fragment
// content (the upstream resends a message fragment by fragment, and the calls
// only appear in the later fragments) or it is the first content of a new
// upstream round (the upstream resends the message right after the round-level
// lines, often starting with plain prose).
func ReplayMayStartWithoutMarkup(p ContentPart) bool {
	return p.Snapshot || p.RoundStart
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
	// trusted is set once the replay reproduced enough of the accumulated text
	// to be dropped instead of appended.
	trusted bool
	// base is the length of the accumulated text at the start of the replayed
	// region, i.e. the length to rewind to once the replay is trusted.
	base int
	// seen is how many bytes of the accumulated text the replay has reproduced.
	seen int
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

// ResolveChunk decides how much of an incoming chunk is new. permissive reports
// that the chunk carries a whole message state (see ContentPart.Snapshot) or
// opens a new upstream round (see ContentPart.RoundStart), so it may open a
// replay alignment without tool-call markup.
func (t *ReplayTracker) ResolveChunk(existing, incoming string, permissive bool) ContinuationReplay {
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
	t.openAlignment(existing, incoming, replay, permissive)
	return replay
}

// followReplay consumes a chunk that continues an open alignment. It reports
// false when the alignment does not hold, leaving the chunk to the chunk-local
// rules.
//
// The match is computed from the text itself - how much of the accumulated text
// the replay reproduced - and never from the position the previous chunk ended
// at, because the transport re-chunks the stream freely: a round boundary can
// merge the tail of the previous round with the head of the replay, and the
// pump may flush at any point. Aligning on part boundaries made the alignment
// break as soon as the boundaries differed, and every following part of the
// replay was appended again - the same call was then emitted twice.
//
// Until the replay reproduced enough of the accumulated text to be trusted, the
// chunks are handled by the chunk-local rules exactly as if no alignment were
// open. A false candidate therefore cannot lose or duplicate anything beyond the
// bounded region it reproduced, and the accumulated text is only rewound once
// the replay provably repeats it.
func (t *ReplayTracker) followReplay(existing, incoming string) (ContinuationReplay, bool) {
	if t.base > len(existing) {
		// The text was replaced below the aligned position, so the alignment no
		// longer describes it.
		t.Reset()
		return ContinuationReplay{}, false
	}
	known := existing[:t.base]
	if t.seen > len(known) {
		t.Reset()
		return ContinuationReplay{}, false
	}
	available := known[t.seen:]

	if t.trusted {
		matched := sharedRunePrefixLen(available, incoming)
		if matched == len(incoming) && len(available) > 0 {
			t.seen += matched
			// The chunk reproduces accumulated text that is already there.
			return ContinuationReplay{Kept: existing}, true
		}
		// The chunk reproduced the rest of the accumulated text (the remainder is
		// new content), or it stopped reproducing it. Only the reproduced prefix
		// is known to be duplicated: the accumulated text is rewound to the start
		// of the replay and the rest of the chunk is new content.
		dropped := len(existing) > len(known)
		t.Reset()
		return ContinuationReplay{Kept: known, Append: incoming[matched:], Dropped: dropped}, true
	}

	// Not trusted yet: the chunk is handled by the chunk-local rules exactly as
	// if no alignment were open, so a false candidate cannot lose content. The
	// alignment only tracks how much of the accumulated text the replay
	// reproduced.
	local := ResolveContinuationReplay(existing, incoming)
	if len(available) == 0 {
		// Nothing left to reproduce: the chunk is a stale replay (the chunk-local
		// rules drop it) or carries new content (they append its tail).
		t.Reset()
		return local, true
	}
	matched := sharedRunePrefixLen(available, incoming)
	switch {
	case matched == len(incoming):
		t.seen += matched
	case matched == len(available) && matched < len(incoming):
		t.seen = len(known)
	default:
		// The chunk stopped reproducing the accumulated text, so this is not a
		// replay after all.
		t.Reset()
		return local, true
	}
	if utf8.RuneCountInString(known[:t.seen]) < minReproducedTrustRunes {
		if t.seen >= len(known) {
			t.Reset()
		}
		return local, true
	}
	t.trusted = true
	dropped := len(existing) > len(known)
	if t.seen >= len(known) {
		// The replay caught up with the accumulated text: the rest of the chunk is
		// new content.
		t.Reset()
		return ContinuationReplay{Kept: known, Append: incoming[matched:], Dropped: dropped}, true
	}
	// Everything the replay reproduced is already part of the accumulated text:
	// drop the copy that was appended for it, with this chunk.
	return ContinuationReplay{Kept: known, Dropped: dropped}, true
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
//   - a plain increment must also carry tool-call markup, OR start a new round.
//     Legitimately repeated output (a run of identical characters, a repeated
//     table row, a repeated parameter block) is byte-identical to a replayed
//     prose fragment, so a delta in the middle of a round can never be treated
//     as a replay on its own. A chunk that carries a whole fragment state, or
//     that opens a round (the upstream always opens a round with status and
//     message-id lines and the replay starts right after them), may be plain
//     prose: the upstream resends the message one fragment at a time and the
//     calls only appear in the later fragments, so refusing to align on a
//     markup-free head would let every later fragment be appended again - which
//     is how the same call gets emitted twice.
//
// Matching an interior offset is not accepted either (a chunk that only appears
// somewhere inside the message): repeated parameter text inside one message
// triggers that, and a trusted alignment rewinds the accumulated text.
//
// Until enough reproduced text accumulated the chunks are merely appended, so a
// false candidate costs nothing: it is dropped only once it provably reproduces
// the accumulated text.
func (t *ReplayTracker) openAlignment(existing, incoming string, replay ContinuationReplay, permissive bool) {
	if replay.Dropped {
		return
	}
	appendVerbatim := replay.Append == incoming
	droppedWhole := replay.Append == "" && replay.Kept == existing
	if !appendVerbatim && !droppedWhole {
		return
	}
	if !permissive && !containsToolCallMarkup(incoming) {
		return
	}
	if !strings.HasPrefix(existing, incoming) {
		return
	}
	t.active, t.trusted = true, false
	t.base, t.seen = len(existing), len(incoming)
}

// ApplyToBuilder resolves incoming against existing, rewinding existing when the
// replay invalidated the tail it already held, and returns the text to append.
// permissive reports that incoming carries a whole message state or opens a new
// upstream round.
func (t *ReplayTracker) ApplyToBuilder(existing *strings.Builder, incoming string, permissive bool) (appendText string, dropped bool) {
	if existing == nil {
		return incoming, false
	}
	replay := t.ResolveChunk(existing.String(), incoming, permissive)
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
