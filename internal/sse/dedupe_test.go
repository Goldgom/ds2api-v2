package sse

import (
	"strings"
	"testing"
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
