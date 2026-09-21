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

// sharedRunePrefixLen mirrors the Go helper.
function sharedRunePrefixLen(a, b) {
  const limit = Math.min(a.length, b.length);
  let i = 0;
  while (i < limit && a[i] === b[i]) {
    i += 1;
  }
  return i;
}

// trimContinuationOverlap returns only the part of incoming that should be
// appended to the accumulated text. Callers that can drop a stale tail must use
// resolveContinuationReplay instead.
function trimContinuationOverlap(existing, incoming) {
  return resolveContinuationReplay(existing, incoming).append;
}

module.exports = {
  MIN_CONTINUATION_SNAPSHOT_LEN,
  resolveContinuationReplay,
  trimContinuationOverlap,
};
