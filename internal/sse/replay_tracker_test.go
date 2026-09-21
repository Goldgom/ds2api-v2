package sse

import (
	"testing"
)

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
