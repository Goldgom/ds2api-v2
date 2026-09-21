package sse

import (
	"strings"
	"testing"
)

// A continue round that resends the message is allowed to be split differently
// than the original delivery: the transport re-chunks freely, and the round
// boundary may even merge the tail of the previous round with the head of the
// replay. The alignment therefore matches text, not part boundaries.
func TestReplayTrackerAlignsReplaySplitDifferently(t *testing.T) {
	message := "<|EPSE|tool_calls>"
	for i := 1; i <= 5; i++ {
		message += `<|EPSE|invoke name="bash"><|EPSE|parameter name="command"><![CDATA[echo "TC` +
			string(rune('0'+i)) + `-$(date +%s%N)"]]></|EPSE|parameter></|EPSE|invoke>`
	}
	message += "</|EPSE|tool_calls>"

	var tracker ReplayTracker
	accumulated := ""
	for _, chunk := range sliceRunes(message, 24) {
		replay := tracker.Resolve(accumulated, chunk)
		accumulated = replay.Kept + replay.Append
	}
	if accumulated != message {
		t.Fatalf("initial delivery mangled the message:\n%q", accumulated)
	}

	// The continue round resends the message token by token, and the round opener
	// is reported on the first chunk (a round boundary merged it with the tail of
	// the previous round).
	rewound := false
	for i, chunk := range sliceRunes(message, 7) {
		replay := tracker.ResolveChunk(accumulated, chunk, i == 0)
		if replay.Dropped {
			rewound = true
		}
		accumulated = replay.Kept + replay.Append
	}
	if !rewound {
		t.Fatalf("expected the token-sized replay to be recognised")
	}
	if accumulated != message {
		t.Fatalf("replay was not deduped:\n%q\nwant:\n%q", accumulated, message)
	}
	if strings.Count(accumulated, "<|EPSE|tool_calls>") != 1 {
		t.Fatalf("expected a single call block, got %d", strings.Count(accumulated, "<|EPSE|tool_calls>"))
	}
}

// A message whose head is tool markup, and whose later calls repeat similar or
// identical parameter text, must survive any chunking. Matching an interior
// offset used to treat those repeated fragments as a replayed snapshot and cut
// the accumulated text apart.
func TestReplayTrackerKeepsSimilarToolCallsIntact(t *testing.T) {
	message := `<|EPSE|tool_calls>
<|EPSE|invoke name="ls">
<|EPSE|parameter name="intent"><![CDATA[列出目录]]></|EPSE|parameter>
<|EPSE|parameter name="path"><![CDATA[.]]></|EPSE|parameter>
</|EPSE|invoke>
<|EPSE|invoke name="read">
<|EPSE|parameter name="displayName"><![CDATA[读取来源指南]]></|EPSE|parameter>
<|EPSE|parameter name="intent"><![CDATA[读取来源指南]]></|EPSE|parameter>
<|EPSE|parameter name="path"><![CDATA[C:\Users\Goldgom\.tokenbird\workspaces\my-workspace\sources\meow-resume-guide.md]]></|EPSE|parameter>
</|EPSE|invoke>
<|EPSE|invoke name="find">
<|EPSE|parameter name="displayName"><![CDATA[查找 Markdown 文件]]></|EPSE|parameter>
<|EPSE|parameter name="intent"><![CDATA[查找 Markdown 文件]]></|EPSE|parameter>
<|EPSE|parameter name="pattern"><![CDATA[*.md]]></|EPSE|parameter>
</|EPSE|invoke>
</|EPSE|tool_calls>`

	for _, size := range []int{16, 24, 32, 48, 96, 4096} {
		var tracker ReplayTracker
		accumulated := ""
		for _, chunk := range sliceRunes(message, size) {
			replay := tracker.Resolve(accumulated, chunk)
			if replay.Dropped {
				t.Fatalf("chunk size %d: unexpected rewind for %q", size, chunk)
			}
			accumulated = replay.Kept + replay.Append
		}
		if accumulated != message {
			t.Fatalf("chunk size %d: message was mangled:\n%q\nwant:\n%q", size, accumulated, message)
		}
	}
}
