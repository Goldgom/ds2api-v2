package sse

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTrimContinuationOverlapReturnsSuffixForSnapshotReplay(t *testing.T) {
	existing := "我们被问到：这是一个很长的续答快照前缀，用来验证去重逻辑不会误伤正常 token。"
	incoming := existing + "继续分析"
	got := TrimContinuationOverlap(existing, incoming)
	if got != "继续分析" {
		t.Fatalf("expected suffix only, got %q", got)
	}
}

func TestTrimContinuationOverlapDropsStaleShorterSnapshot(t *testing.T) {
	incoming := "我们被问到：这是一个很长的续答快照前缀，用来验证去重逻辑不会误伤正常 token。"
	existing := incoming + "继续分析"
	got := TrimContinuationOverlap(existing, incoming)
	if got != "" {
		t.Fatalf("expected stale snapshot to be dropped, got %q", got)
	}
}

func TestTrimContinuationOverlapPreservesNormalIncrement(t *testing.T) {
	existing := "我们"
	incoming := "被"
	got := TrimContinuationOverlap(existing, incoming)
	if got != "被" {
		t.Fatalf("expected normal increment unchanged, got %q", got)
	}
}

func TestTrimContinuationOverlapKeepsShortPrefixLikeNormalToken(t *testing.T) {
	existing := "我们被问到"
	incoming := "我们"
	got := TrimContinuationOverlap(existing, incoming)
	if got != "我们" {
		t.Fatalf("expected short token preserved, got %q", got)
	}
}

func TestTrimContinuationOverlapKeepsShortMultibyteChunk(t *testing.T) {
	existing := strings.Repeat("字", 36)
	incoming := strings.Repeat("字", 16)
	got := TrimContinuationOverlap(existing, incoming)
	if got != incoming {
		t.Fatalf("expected short multibyte chunk preserved, got %q", got)
	}
}

func TestResolveContinuationReplayDropsIdenticalSnapshot(t *testing.T) {
	existing := "我们被问到：这是一个很长的续答快照前缀，用来验证去重逻辑不会误伤正常 token。"
	replay := ResolveContinuationReplay(existing, existing)
	if replay.Dropped {
		t.Fatalf("expected no dropped tail for an identical snapshot, got %+v", replay)
	}
	if replay.Kept != existing {
		t.Fatalf("expected accumulated text to survive, got %q", replay.Kept)
	}
	if replay.Append != "" {
		t.Fatalf("expected identical snapshot to be dropped, got %q", replay.Append)
	}
}

// A chunk that only overlaps the accumulated text is never treated as a re-render
// of it. Every rule in ResolveContinuationReplay is one-sided: it may drop what
// the incoming chunk replayed, but it never cuts the accumulated text apart, so
// no rule can destroy content that was already accumulated.
func TestResolveContinuationReplayNeverCutsAccumulatedText(t *testing.T) {
	head := "我们被问到：这是一个很长的续答快照前缀，用来验证去重逻辑不会误伤正常 token。"
	existing := head + "旧的结尾"
	incoming := head + "重写后的结尾"
	replay := ResolveContinuationReplay(existing, incoming)
	if replay.Dropped {
		t.Fatalf("expected the accumulated text to survive, got %+v", replay)
	}
	if replay.Kept != existing {
		t.Fatalf("expected kept=%q, got %q", existing, replay.Kept)
	}
	if replay.Append != incoming {
		t.Fatalf("expected the whole chunk to be appended, got %q", replay.Append)
	}
}

// The next block of a message that repeats the same markup shares a long
// prologue with the previous one. That overlap used to be taken as evidence
// that the message was being re-rendered, and the accumulated text was cut at
// the prologue - which handed one call the arguments of another.
func TestResolveContinuationReplayKeepsSimilarToolBlocksApart(t *testing.T) {
	first := `<|EPSE|tool_calls><|EPSE|invoke name="read"><|EPSE|parameter name="path"><![CDATA[x]]></|EPSE|parameter></|EPSE|tool_calls>`
	second := `<|EPSE|tool_calls><|EPSE|invoke name="ls"><|EPSE|parameter name="path"><![CDATA[.]]></|EPSE|parameter></|EPSE|invoke></|EPSE|tool_calls>`
	replay := ResolveContinuationReplay(first, second)
	if replay.Dropped || replay.Kept != first || replay.Append != second {
		t.Fatalf("expected the second block to be appended verbatim, got %+v", replay)
	}
}

// A continue round delivers the increment of the previous round and the whole
// message state of the next one as separate chunks (the transport keeps a
// snapshot chunk apart from buffered increments). Resolving them in that order
// keeps one copy of the message: the increment is new, and the snapshot that
// carries it is recognised as a replay of the accumulated text.
func TestResolveContinuationReplayKeepsOneCopyAcrossIsolatedChunks(t *testing.T) {
	content := "第一段很长很长很长很长的回答内容，用来验证传输层分开投递的续答轮次。"
	first := " 另外补充第一点。"
	second := " 继续补充第二点。"
	accumulated := ResolveContinuationReplay("", content).Append
	accumulated += ResolveContinuationReplay(accumulated, first).Append
	accumulated += ResolveContinuationReplay(accumulated, second).Append
	// The next round resends the whole message state, which already carries
	// everything accumulated so far.
	replay := ResolveContinuationReplay(accumulated, content+first+second+" 第二段补充。")
	accumulated = replay.Kept + replay.Append
	want := content + first + second + " 第二段补充。"
	if accumulated != want {
		t.Fatalf("expected %q, got %q", want, accumulated)
	}
}

func TestApplyContinuationReplayKeepsSingleCopyAcrossContinueRounds(t *testing.T) {
	content := "第一段很长很长很长很长的回答内容，用来验证多轮 continue 不会重复累积。"
	body := content
	rounds := []string{body}
	for _, suffix := range []string{" 第二段补充。", " 第三段补充。", " 第四段补充。"} {
		body += suffix
		rounds = append(rounds, body)
	}
	accumulated := rounds[0]
	for _, round := range rounds[1:] {
		accumulated = ApplyContinuationReplay(accumulated, round)
	}
	if accumulated != body {
		t.Fatalf("expected %q, got %q", body, accumulated)
	}
}

func TestApplyContinuationReplayKeepsSingleCopyForStaleRounds(t *testing.T) {
	content := "第一段很长很长很长很长的回答内容，用来验证传输层重发的旧快照被丢弃。"
	accumulated := content + " 第二段补充。"
	// A round that resends the stale state must not append anything...
	accumulated = ApplyContinuationReplay(accumulated, content)
	if accumulated != content+" 第二段补充。" {
		t.Fatalf("expected the stale state to be dropped, got %q", accumulated)
	}
	// ...and a round that carries a regenerated state must not lose text either.
	accumulated = ApplyContinuationReplay(accumulated, content+" 重写过的第二段。")
	if accumulated != content+" 第二段补充。"+content+" 重写过的第二段。" {
		t.Fatalf("expected both copies without any loss, got %q", accumulated)
	}
}

func sliceRunes(s string, n int) []string {
	runes := []rune(s)
	out := make([]string, 0, len(runes)/n+1)
	for i := 0; i < len(runes); i += n {
		end := i + n
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

func TestReplayTrackerDropsTokenSizedReplay(t *testing.T) {
	block := `<tool_calls><invoke name="search"><parameter name="q">golang duplicate tool call replay</parameter></invoke></tool_calls>`
	message := "我先说明一下思路，然后给出调用：" + block + " 然后就结束了。"

	var tracker ReplayTracker
	accumulated := message
	dropped := false
	chunks := append(sliceRunes(message, 24), " 后面是新增内容。")
	for _, chunk := range chunks {
		replay := tracker.Resolve(accumulated, chunk)
		dropped = dropped || replay.Dropped
		accumulated = replay.Kept + replay.Append
	}

	if !dropped {
		t.Fatalf("expected the replayed fragments to be recognised")
	}
	if want := message + " 后面是新增内容。"; accumulated != want {
		t.Fatalf("expected %q, got %q", want, accumulated)
	}
	if strings.Count(accumulated, block) != 1 {
		t.Fatalf("expected a single tool-call block, got %d", strings.Count(accumulated, block))
	}
}

func TestReplayTrackerKeepsRepeatedOutput(t *testing.T) {
	var tracker ReplayTracker
	accumulated := ""
	for i := 0; i < 40; i++ {
		replay := tracker.Resolve(accumulated, strings.Repeat("字", 16))
		if replay.Dropped {
			t.Fatalf("repeated legit output must not be treated as a replay")
		}
		accumulated = replay.Kept + replay.Append
	}
	if want := strings.Repeat("字", 640); accumulated != want {
		t.Fatalf("expected %d runes, got %d", utf8.RuneCountInString(want), utf8.RuneCountInString(accumulated))
	}
}

func TestReplayTrackerKeepsUnconfirmedMarkupCandidate(t *testing.T) {
	head := "我们先看第一段说明文字，然后再给出工具调用："
	block := `<tool_calls><invoke name="search"><parameter name="q">golang</parameter></invoke></tool_calls>`
	candidate := head + "<tool_ca"

	var tracker ReplayTracker
	accumulated := head + block
	first := tracker.Resolve(accumulated, candidate)
	accumulated = first.Kept + first.Append
	if first.Dropped || !strings.HasSuffix(accumulated, candidate) {
		t.Fatalf("expected the candidate to be appended, got %+v text=%q", first, accumulated)
	}

	second := tracker.Resolve(accumulated, "这是一个完全不同的新内容片段。")
	accumulated = second.Kept + second.Append
	if second.Dropped {
		t.Fatalf("expected the alignment to be dropped without rewinding, got %+v", second)
	}
	if want := head + block + candidate + "这是一个完全不同的新内容片段。"; accumulated != want {
		t.Fatalf("expected %q, got %q", want, accumulated)
	}
}

func TestReplayTrackerKeepsHeadWhenReplayDiverges(t *testing.T) {
	head := "我们被问到：这是一个很长的续答快照前缀，用来验证去重逻辑不会误伤正常 token。"
	block := `<tool_calls><invoke name="search"><parameter name="q">golang</parameter></invoke></tool_calls>`
	message := head + block + "旧的尾巴，后面还有一段更长的内容用来让对齐跨越多个块。"
	runes := []rune(message)
	chunk := func(from, to int) string {
		if to > len(runes) {
			to = len(runes)
		}
		return string(runes[from:to])
	}

	var tracker ReplayTracker
	accumulated := message

	// The continue round replays the message fragment by fragment. The first
	// fragment has to carry tool-call markup for the replay to be recognised.
	for _, span := range [][2]int{{0, 48}, {48, 72}} {
		replay := tracker.Resolve(accumulated, chunk(span[0], span[1]))
		accumulated = replay.Kept + replay.Append
	}
	if accumulated != message {
		t.Fatalf("expected the replay to leave the message intact, got %q", accumulated)
	}

	// The upstream then stops replaying and continues with new content that
	// happens to share a few runes with the aligned position.
	diverged := tracker.Resolve(accumulated, chunk(72, 76)+"被改写的结尾。")
	if diverged.Kept != accumulated {
		t.Fatalf("expected the accumulated text to stay untouched, got %q", diverged.Kept)
	}
	if diverged.Dropped {
		t.Fatalf("a guessed replay must never cut accumulated text")
	}
	if diverged.Append != "被改写的结尾。" {
		t.Fatalf("expected only the unmatched tail, got %q", diverged.Append)
	}
	if got := diverged.Kept + diverged.Append; got != message+"被改写的结尾。" {
		t.Fatalf("unexpected accumulated text %q", got)
	}
}
