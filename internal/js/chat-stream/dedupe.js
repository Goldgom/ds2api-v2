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

const MIN_REPRODUCED_TRUST_RUNES = 16;

// sharedPrefixLen returns the length of the longest common prefix, rounded down
// to a code point boundary of b.
function sharedPrefixLen(a, b) {
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

function runeCount(value) {
  return Array.from(value).length;
}

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
// parts, which resolveContinuationReplay cannot recognise on its own because
// every fragment stays below the snapshot floor.
//
// The alignment is driven by the text itself: it tracks how much of the text
// accumulated before the replay started the replay has reproduced, instead of
// the byte position the previous part ended at. The transport re-chunks the
// stream freely (a round boundary can merge the tail of the previous round with
// the head of the replay), so aligning on part boundaries broke as soon as the
// boundaries differed and every following part was appended again - the same
// call was then emitted twice.
class ReplayTracker {
  constructor() {
    this.reset();
  }

  reset() {
    this.active = false;
    this.trusted = false;
    this.base = 0;
    this.seen = 0;
  }

  resolve(existing, incoming) {
    return this.resolveChunk(existing, incoming, false);
  }

  // resolveSnapshot handles a part that carries a whole message state: it may be
  // a replayed fragment batch whose head has no tool markup, so a markup-free
  // head may open an alignment here.
  resolveSnapshot(existing, incoming) {
    return this.resolveChunk(existing, incoming, true);
  }

  // resolveChunk decides how much of incoming is new. permissive reports that
  // incoming carries whole fragment content or opens a new upstream round, so a
  // markup-free head may start a replay alignment.
  resolveChunk(existing, incoming, permissive) {
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
    this.openAlignment(current, incoming, replay, permissive);
    return replay;
  }

  followReplay(existing, incoming) {
    if (this.base > existing.length) {
      this.reset();
      return null;
    }
    const known = existing.slice(0, this.base);
    if (this.seen > known.length) {
      this.reset();
      return null;
    }
    const available = known.slice(this.seen);

    if (this.trusted) {
      const matched = sharedPrefixLen(available, incoming);
      if (matched === incoming.length && available.length > 0) {
        this.seen += matched;
        return { kept: existing, append: '', dropped: false };
      }
      // The part reproduced the rest of the accumulated text (the remainder is
      // new) or stopped reproducing it: only the reproduced prefix is known to
      // be duplicated.
      const dropped = existing.length > known.length;
      this.reset();
      return { kept: known, append: incoming.slice(matched), dropped };
    }

    // Not trusted yet: the part is handled by the chunk-local rules exactly as
    // if no alignment were open, so a false candidate cannot lose content.
    const local = resolveContinuationReplay(existing, incoming);
    if (available.length === 0) {
      this.reset();
      return local;
    }
    const matched = sharedPrefixLen(available, incoming);
    if (matched === incoming.length) {
      this.seen += matched;
    } else if (matched === available.length && matched < incoming.length) {
      this.seen = known.length;
    } else {
      this.reset();
      return local;
    }
    if (runeCount(known.slice(0, this.seen)) < MIN_REPRODUCED_TRUST_RUNES) {
      if (this.seen >= known.length) {
        this.reset();
      }
      return local;
    }
    this.trusted = true;
    const dropped = existing.length > known.length;
    if (this.seen >= known.length) {
      this.reset();
      return { kept: known, append: incoming.slice(matched), dropped };
    }
    return { kept: known, append: '', dropped };
  }

  // The evidence is deliberately narrow, because both a missed replay and a
  // false one are costly and the two are hard to tell apart in text:
  //
  //   - the chunk must restart the message, i.e. be a prefix of the accumulated
  //     text. A continue round that resends the message always starts that way,
  //     whether it resends it as one snapshot or as one part per fragment.
  //   - it must be long enough to align on.
  //   - an ordinary increment must also carry tool-call markup: legitimately
  //     repeated output is byte-identical to a replayed prose fragment, so plain
  //     deltas can never be a replay on their own. A part that carries a whole
  //     message state may: the upstream resends the message one fragment at a
  //     time and the calls only appear in the later fragments, so refusing to
  //     align on a markup-free head would let every later fragment be appended
  //     again - the same call would be emitted twice.
  //
  // Matching an interior offset is not accepted (a chunk that only appears
  // somewhere inside the message), because a confirmed alignment rewinds the
  // accumulated text.
  //
  // A chunk that was appended verbatim stays appended until the next chunk
  // confirms the replay, so a false candidate costs nothing.
  openAlignment(existing, incoming, replay, permissive) {
    if (replay.dropped) {
      return;
    }
    const appendVerbatim = replay.append === incoming;
    const droppedWhole = replay.append === '' && replay.kept === existing;
    if (!appendVerbatim && !droppedWhole) {
      return;
    }
    if (!permissive && !containsToolCallMarkup(incoming)) {
      return;
    }
    if (!existing.startsWith(incoming)) {
      return;
    }
    this.active = true;
    this.trusted = false;
    this.base = existing.length;
    this.seen = incoming.length;
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
