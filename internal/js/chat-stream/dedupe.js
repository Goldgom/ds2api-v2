'use strict';

// Mirrors internal/sse/dedupe.go. DeepSeek resends the whole message as a
// snapshot when a stream opens and at the start of every `continue` round, so
// appending such a snapshot verbatim duplicates everything it replays -
// including tool-call markup, which the sieve then parses again and emits as
// another tool call with a fresh id.
//
// The rules are one-sided: they only ever drop the text an incoming chunk
// replayed, never cut the accumulated text apart. Cutting content that was only
// *guessed* to be a replay is what corrupted tool-call arguments (a message made
// of several similar blocks shares a long prologue with its own next block), and
// a re-rendered snapshot can only be appended in any case, because text that was
// already streamed cannot be retracted.
const MIN_CONTINUATION_SNAPSHOT_LEN = 32;

// resolveContinuationReplay decides how much of an incoming chunk is new.
//
// kept    replaces the accumulated text; it is that text itself.
// append  is appended after kept; it holds every byte that is new.
// dropped reports that a replay that had already been consumed was taken back,
//         so derived state (such as the tool sieve) has to be rebuilt.
function resolveContinuationReplay(existing, incoming) {
  const current = typeof existing === 'string' ? existing : '';
  if (!incoming) {
    return { kept: current, append: '', dropped: false };
  }
  if (!current) {
    return { kept: '', append: incoming, dropped: false };
  }
  if (incoming.length < MIN_CONTINUATION_SNAPSHOT_LEN) {
    return { kept: current, append: incoming, dropped: false };
  }
  // Snapshot replay that grew while we streamed: keep only the new tail.
  if (incoming.startsWith(current)) {
    return { kept: current, append: incoming.slice(current.length), dropped: false };
  }
  // Snapshot replay that is stale or identical: everything it carries is
  // already accumulated, so nothing is added.
  if (current.startsWith(incoming)) {
    return { kept: current, append: '', dropped: false };
  }
  return { kept: current, append: incoming, dropped: false };
}

// TOOL_MARKUP_HINTS are fragments that only ever occur inside tool-call markup.
const TOOL_MARKUP_HINTS = [
  '<tool', '</tool', 'tool_calls', 'EPSE',
  '<invoke', '</invoke', 'invoke name', 'invoke|name',
  '<parameter', '</parameter', 'parameter name', 'parameter|name',
];

const MIN_REPLAY_MARKUP_LEN = 8;

function containsToolCallMarkup(text) {
  if (!text) {
    return false;
  }
  for (const hint of TOOL_MARKUP_HINTS) {
    if (text.indexOf(hint) >= 0) {
      return true;
    }
  }
  return false;
}

// ReplayTracker drops continuation snapshot replays that arrive as many small
// deltas, which resolveContinuationReplay cannot recognise on its own because
// every fragment stays below the snapshot floor. The alignment is anchored on
// the text accumulated before the replay started, so the replayed fragments can
// be dropped whatever size they are delivered in.
class ReplayTracker {
  constructor() {
    this.reset();
  }

  reset() {
    this.active = false;
    this.confirmed = false;
    this.base = 0;
    this.offset = 0;
  }

  resolve(existing, incoming) {
    const current = typeof existing === 'string' ? existing : '';
    if (!incoming) {
      return { kept: current, append: '', dropped: false };
    }
    if (!current) {
      this.reset();
      return { kept: '', append: incoming, dropped: false };
    }
    if (this.active) {
      const followed = this.followReplay(current, incoming);
      if (followed) {
        return followed;
      }
    }
    const replay = resolveContinuationReplay(current, incoming);
    this.openAlignment(current, incoming, replay);
    return replay;
  }

  followReplay(existing, incoming) {
    if (this.offset > existing.length) {
      // The text was rewound below the aligned position.
      this.reset();
      return null;
    }
    const expected = existing.slice(this.offset);
    const limit = Math.min(expected.length, incoming.length);
    let matched = 0;
    while (matched < limit && expected[matched] === incoming[matched]) {
      matched += 1;
    }
    if (matched === incoming.length) {
      this.offset += matched;
      if (!this.confirmed) {
        this.confirmed = true;
        if (this.base < existing.length) {
          // The chunk that opened the alignment was replayed content after all.
          return { kept: existing.slice(0, this.base), append: '', dropped: true };
        }
      }
      return { kept: existing, append: '', dropped: false };
    }
    if (this.confirmed && matched > 0) {
      // The replayed fragment stopped matching. Only the matched prefix is known
      // to be duplicated, so keep the accumulated text untouched and continue
      // with the rest. Cutting the text here would destroy content that was only
      // guessed to be a replay.
      const replay = { kept: existing, append: incoming.slice(matched), dropped: false };
      this.reset();
      return replay;
    }
    this.reset();
    return null;
  }

  // The evidence is deliberately narrow, because both a missed replay and a
  // false one are costly and the two are hard to tell apart in text:
  //
  //   - the chunk must restart the message, i.e. be a prefix of the accumulated
  //     text. A continue round that resends the message token by token always
  //     starts that way.
  //   - it must carry tool-call markup. Legitimately repeated output is
  //     byte-identical to a replayed text fragment, so plain text can never be a
  //     replay on its own - and neither can markup that merely appears somewhere
  //     inside the message, which is why matching an interior offset is not
  //     accepted here.
  //
  // A chunk that was appended verbatim stays appended until the next chunk
  // confirms the replay, so a false candidate costs nothing.
  openAlignment(existing, incoming, replay) {
    if (replay.dropped) {
      return;
    }
    const appendVerbatim = replay.append === incoming;
    const droppedWhole = replay.append === '' && replay.kept === existing;
    if (!appendVerbatim && !droppedWhole) {
      return;
    }
    if (incoming.length < MIN_REPLAY_MARKUP_LEN || !containsToolCallMarkup(incoming)) {
      return;
    }
    if (!existing.startsWith(incoming)) {
      return;
    }
    this.active = true;
    this.confirmed = false;
    this.base = existing.length;
    this.offset = incoming.length;
  }
}

// trimContinuationOverlap returns only the part of incoming that should be
// appended to the accumulated text. Callers that own a mutable buffer must use
// resolveContinuationReplay or ReplayTracker instead, because a replay that was
// already consumed can be taken back.
function trimContinuationOverlap(existing, incoming) {
  return resolveContinuationReplay(existing, incoming).append;
}

module.exports = {
  MIN_CONTINUATION_SNAPSHOT_LEN,
  containsToolCallMarkup,
  resolveContinuationReplay,
  ReplayTracker,
  trimContinuationOverlap,
};
