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

func TestResolveContinuationReplayDropsDivergedTail(t *testing.T) {
	head := "我们被问到：这是一个很长的续答快照前缀，用来验证去重逻辑不会误伤正常 token。"
	existing := head + "旧的结尾"
	incoming := head + "重写后的结尾"
	replay := ResolveContinuationReplay(existing, incoming)
	if !replay.Dropped {
		t.Fatalf("expected diverged replay to drop the stale tail, got %+v", replay)
	}
	if replay.Kept != head {
		t.Fatalf("expected kept=%q, got %q", head, replay.Kept)
	}
	if got := replay.Kept + replay.Append; got != incoming {
		t.Fatalf("expected accumulated text %q, got %q", incoming, got)
	}
}

func TestResolveContinuationReplayHandlesEmbeddedSnapshot(t *testing.T) {
	head := "我们被问到：这是一个很长的续答快照前缀，用来验证去重逻辑不会误伤正常 token。"
	t.Run("buffered increment already carried by the snapshot", func(t *testing.T) {
		increment := " 另外补充第一点。"
		incoming := increment + head + increment
		replay := ResolveContinuationReplay(head, incoming)
		if replay.Dropped {
			t.Fatalf("expected no dropped tail, got %+v", replay)
		}
		if got := replay.Kept + replay.Append; got != head+increment {
			t.Fatalf("expected %q, got %q", head+increment, got)
		}
	})
	t.Run("buffered increment missing from the snapshot is kept", func(t *testing.T) {
		increment := " 另外补充第一点。"
		incoming := increment + head + " 全新的收尾。"
		replay := ResolveContinuationReplay(head, incoming)
		if got := replay.Kept + replay.Append; got != head+increment+" 全新的收尾。" {
			t.Fatalf("expected buffered increment and new tail, got %q", got)
		}
	})
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

func TestApplyContinuationReplayKeepsSingleCopyForCoalescedRounds(t *testing.T) {
	content := "第一段很长很长很长很长的回答内容，用来验证传输层合并后的快照重放。"
	body := content
	accumulated := body
	// The transport coalesces the buffered increment of the previous round with
	// the whole snapshot of the next one.
	for _, suffix := range []string{" 第二段补充。", " 第三段补充。"} {
		accumulated = ApplyContinuationReplay(accumulated, suffix+body+suffix)
		body += suffix
	}
	if accumulated != body {
		t.Fatalf("expected %q, got %q", body, accumulated)
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

	// The upstream then re-renders the tail from inside the replayed region.
	diverged := tracker.Resolve(accumulated, chunk(72, 76)+"被改写的结尾。")
	if got, want := diverged.Kept, chunk(0, 76); got != want {
		t.Fatalf("expected the aligned head %q, got %q", want, got)
	}
	if diverged.Append != "被改写的结尾。" {
		t.Fatalf("expected the new tail, got %q", diverged.Append)
	}
	if !diverged.Dropped {
		t.Fatalf("expected the stale tail to be dropped")
	}
}
