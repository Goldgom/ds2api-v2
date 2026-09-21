package shared

import (
	"strings"
	"testing"
)

// The visible layer may only strip a tool-call wrapper that was turned into a
// structured tool call. A wrapper that does not parse has to stay visible: the
// streaming sieve releases such a block as plain text on purpose, and dropping
// it here left the client with an empty message and no call at all, which the
// handler then reported as an upstream failure.
func TestStripLeakedToolCallWrapperBlocksKeepsUnparseableWrapper(t *testing.T) {
	cases := []struct {
		name         string
		text         string
		wantStripped bool
	}{
		{
			name:         "parseable-epse-wrapper",
			text:         `<|EPSE|tool_calls><|EPSE|invoke name="read"><|EPSE|parameter name="path"><![CDATA[README.md]]></|EPSE|parameter></|EPSE|invoke></|EPSE|tool_calls>`,
			wantStripped: true,
		},
		{
			name:         "parseable-legacy-wrapper",
			text:         `<tool_calls><invoke name="read"><parameter name="path">README.md</parameter></invoke></tool_calls>`,
			wantStripped: true,
		},
		{
			name:         "unclosed-invoke",
			text:         "<|EPSE|tool_calls>\n<|EPSE|invoke name=\"read\">\n<|EPSE|parameter name=\"path\"><![CDATA[README.md]]></|EPSE|parameter>\n</|EPSE|tool_calls>",
			wantStripped: false,
		},
		{
			name:         "no-invoke-at-all",
			text:         `<|EPSE|tool_calls>调用工具</|EPSE|tool_calls>`,
			wantStripped: false,
		},
		{
			name:         "empty-wrapper",
			text:         `<|EPSE|tool_calls></|EPSE|tool_calls>`,
			wantStripped: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripLeakedToolCallWrapperBlocks(tc.text)
			if tc.wantStripped {
				if strings.TrimSpace(got) != "" {
					t.Fatalf("expected the wrapper to be stripped, got %q", got)
				}
				return
			}
			if !strings.Contains(got, "tool_calls") {
				t.Fatalf("expected the unparseable wrapper to stay visible, got %q", got)
			}
		})
	}
}

// Text around a structured wrapper survives, and prose without any wrapper is
// returned unchanged.
func TestStripLeakedToolCallWrapperBlocksKeepsSurroundingText(t *testing.T) {
	text := "先看目录。\n" + `<|EPSE|tool_calls><|EPSE|invoke name="ls"><|EPSE|parameter name="path"><![CDATA[.]]></|EPSE|parameter></|EPSE|invoke></|EPSE|tool_calls>` + "\n完成。"
	got := stripLeakedToolCallWrapperBlocks(text)
	if got != "先看目录。\n\n完成。" {
		t.Fatalf("unexpected cleaned text: %q", got)
	}
	if plain := "没有任何标记的正文。"; stripLeakedToolCallWrapperBlocks(plain) != plain {
		t.Fatalf("expected plain text unchanged")
	}
}
