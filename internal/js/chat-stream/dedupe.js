'use strict';

// Mirrors internal/sse/dedupe.go. DeepSeek resends the whole message as a
// snapshot when a stream opens and at the start of every `continue` round, so
// appending such a snapshot verbatim duplicates everything it replays -
// including tool-call markup, which the sieve then parses again and emits as
// another tool call with a fresh id. The transport may also coalesce buffered
// increments with the next snapshot, so a replay does not always start at
// offset 0 of the incoming chunk.
const MIN_CONTINUATION_SNAPSHOT_LEN = 32;

// resolveContinuationReplay decides how much of an incoming chunk is new.
//
// kept    replaces the accumulated text.
// append  is appended after kept; it holds every byte that is new.
// dropped reports that already accumulated text was discarded, so derived
//         state (such as the tool sieve) has to be rebuilt.
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
  // Snapshot replay that diverged: upstream re-rendered the message from a
  // point both copies share. Drop the stale tail past that point.
  const keep = sharedRunePrefixLen(current, incoming);
  if (keep >= MIN_CONTINUATION_SNAPSHOT_LEN) {
    return { kept: current.slice(0, keep), append: incoming.slice(keep), dropped: true };
  }
  // A replay that starts inside the chunk, because a buffered increment and
  // the next snapshot were delivered together.
  const at = embeddedSnapshotStart(current, incoming);
  if (at > 0) {
    const head = incoming.slice(0, at);
    const snapshot = incoming.slice(at);
    const inner = resolveContinuationReplay(current, snapshot);
    // A buffered increment is always generated before the snapshot that
    // follows it, so the snapshot already carries it.
    return {
      kept: inner.kept,
      append: (snapshot.indexOf(head) >= 0 ? '' : head) + inner.append,
      dropped: inner.dropped,
    };
  }
  return { kept: current, append: incoming, dropped: false };
}

// embeddedSnapshotStart returns the offset inside incoming where the upstream
// restarted the replayed message, or -1 when there is no embedded replay.
function embeddedSnapshotStart(existing, incoming) {
  const probe = existing.slice(0, MIN_CONTINUATION_SNAPSHOT_LEN);
  if (!probe) {
    return -1;
  }
  // Offset 0 is already covered by the prefix rules above.
  const idx = incoming.slice(1).indexOf(probe);
  if (idx < 0) {
    return -1;
  }
  return 1 + idx;
}

// sharedRunePrefixLen mirrors the Go helper. It never splits a surrogate pair.
function sharedRunePrefixLen(a, b) {
  const limit = Math.min(a.length, b.length);
  let i = 0;
  while (i < limit && a[i] === b[i]) {
    i += 1;
  }
  if (i > 0) {
    const unit = b.charCodeAt(i - 1);
    if (unit >= 0xd800 && unit <= 0xdbff) {
      i -= 1;
    }
  }
  return i;
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
      const kept = existing.slice(0, this.offset + matched);
      const replay = {
        kept,
        append: incoming.slice(matched),
        dropped: kept.length < existing.length,
      };
      this.reset();
      return replay;
    }
    this.reset();
    return null;
  }

  // The opening evidence has to be tool-call markup: legitimately repeated
  // output is byte-identical to a replayed text fragment, so plain text can
  // never be treated as a replay on its own. A chunk that was appended verbatim
  // stays appended until the next chunk confirms the replay, so a false
  // candidate costs nothing.
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
    const at = existing.indexOf(incoming);
    if (at < 0) {
      return;
    }
    this.active = true;
    this.confirmed = false;
    this.base = existing.length;
    this.offset = at + incoming.length;
  }
}

// trimContinuationOverlap returns only the part of incoming that should be
// appended to the accumulated text. Callers that can drop a stale tail must use
// resolveContinuationReplay or ReplayTracker instead.
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
