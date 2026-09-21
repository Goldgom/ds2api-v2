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
	// minReplayMarkupRunes is the smallest chunk carrying tool-call markup that
	// may open an alignment. Tool-call markup is split freely by the upstream,
	// so these fragments have to be recognised below the snapshot floor as well.
	minReplayMarkupRunes = 8
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
// The surrounding transport may also coalesce buffered increments with the next
// snapshot into a single chunk, so a replay does not always start at offset 0
// of the incoming chunk.
type ContinuationReplay struct {
	// Kept replaces the accumulated text. It is that text itself in the common
	// case, and a shortened version of it when the upstream re-rendered the
	// message from a point that both copies share.
	Kept string
	// Append is the text to append after Kept. It holds every byte that is new,
	// so incremental consumers such as the tool sieve must process Append.
	Append string
	// Dropped reports that text which had already been accumulated was
	// discarded. State derived from that text has to be rebuilt.
	Dropped bool
}

// ResolveContinuationReplay decides how much of an incoming chunk is new.
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
	// Snapshot replay that diverged: upstream re-rendered the message from a
	// point both copies share. Drop the stale tail past that point and take
	// only the regenerated remainder, otherwise every replayed round appends
	// another full copy of the message.
	if keep := sharedRunePrefixLen(existing, incoming); utf8.RuneCountInString(existing[:keep]) >= minContinuationSnapshotLen {
		return ContinuationReplay{Kept: existing[:keep], Append: incoming[keep:], Dropped: true}
	}
	// A replay that starts inside the chunk, because a buffered increment and
	// the next snapshot were delivered together.
	if at := embeddedSnapshotStart(existing, incoming); at > 0 {
		head := incoming[:at]
		snapshot := incoming[at:]
		inner := ResolveContinuationReplay(existing, snapshot)
		if strings.Contains(snapshot, head) {
			// A buffered increment is always generated before the snapshot that
			// follows it, so the snapshot already carries it.
			head = ""
		}
		return ContinuationReplay{
			Kept:    inner.Kept,
			Append:  head + inner.Append,
			Dropped: inner.Dropped,
		}
	}
	return ContinuationReplay{Kept: existing, Append: incoming}
}

// embeddedSnapshotStart returns the offset inside incoming where the upstream
// restarted the replayed message, or -1 when there is no embedded replay.
func embeddedSnapshotStart(existing, incoming string) int {
	probe := runePrefix(existing, minContinuationSnapshotLen)
	if probe == "" || len(probe) > len(incoming) {
		return -1
	}
	// Offset 0 is already covered by the prefix rules above. A UTF-8 encoded
	// rune never starts with a continuation byte, so a match can only begin on
	// a rune boundary.
	idx := strings.Index(incoming[1:], probe)
	if idx < 0 {
		return -1
	}
	return 1 + idx
}

// runePrefix returns the first n runes of s, or s itself when it is shorter.
func runePrefix(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
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
// (or ResolveContinuationReplay) instead: when the upstream replayed a snapshot
// that diverged from the accumulated text, the stale tail has to be dropped as
// well, which this wrapper cannot do.
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
	t.openAlignment(existing, incoming, replay)
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
		// The replay ran into regenerated content: keep the shared head and
		// continue with the new tail.
		kept := existing[:t.offset+matched]
		replay := ContinuationReplay{Kept: kept, Append: incoming[matched:], Dropped: len(kept) < len(existing)}
		t.Reset()
		return replay, true
	}
	t.Reset()
	return ContinuationReplay{}, false
}

// openAlignment starts an alignment when incoming looks like a replayed fragment
// of a message the upstream already sent.
//
// The opening evidence has to be tool-call markup: legitimately repeated output
// (a repeated table row, a run of identical characters) is byte-identical to a
// replayed text fragment, so plain text can never be treated as a replay on its
// own. Markup, on the other hand, is only ever repeated when the upstream
// replayed it, and it is exactly what makes a duplicated tool call. Chunks that
// were appended verbatim stay appended until the next chunk confirms the replay,
// so a false candidate costs nothing.
func (t *ReplayTracker) openAlignment(existing, incoming string, replay ContinuationReplay) {
	if replay.Dropped {
		return
	}
	appendVerbatim := replay.Append == incoming
	droppedWhole := replay.Append == "" && replay.Kept == existing
	if !appendVerbatim && !droppedWhole {
		return
	}
	if utf8.RuneCountInString(incoming) < minReplayMarkupRunes || !containsToolCallMarkup(incoming) {
		return
	}
	at := strings.Index(existing, incoming)
	if at < 0 {
		return
	}
	t.active, t.confirmed = true, false
	t.base, t.offset = len(existing), at+len(incoming)
}

// ApplyToBuilder resolves incoming against existing, rewinding existing when the
// replay invalidated the tail it already held, and returns the text to append.
func (t *ReplayTracker) ApplyToBuilder(existing *strings.Builder, incoming string) (appendText string, dropped bool) {
	if existing == nil {
		return incoming, false
	}
	replay := t.Resolve(existing.String(), incoming)
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
