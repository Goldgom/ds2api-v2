package toolcalldebug

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisabledByDefault(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	t.Setenv(envVar, "")

	if Enabled() {
		t.Fatalf("expected the trace to be off without %s", envVar)
	}
	Log("should_not_appear", map[string]any{"a": 1})
	if Target() != "" {
		t.Fatalf("expected no target, got %q", Target())
	}
}

func TestWritesJSONLinesToConfiguredPath(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	t.Setenv(envVar, path)

	if !Enabled() {
		t.Fatalf("expected the trace to be on")
	}
	Log("part", map[string]any{"channel": "raw_text", "partLen": 42})
	Log("call_emitted", map[string]any{"name": "bash"})
	Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 trace lines, got %d: %q", len(lines), string(raw))
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("trace line is not JSON: %v", err)
	}
	if first["event"] != "part" || first["channel"] != "raw_text" {
		t.Fatalf("unexpected first record: %#v", first)
	}
	if _, ok := first["ts"]; !ok {
		t.Fatalf("expected a timestamp, got %#v", first)
	}
}

func TestTargetDirectoryGetsDefaultFileName(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	dir := t.TempDir()
	t.Setenv(envVar, dir)

	if !Enabled() {
		t.Fatalf("expected the trace to be on")
	}
	Log("request_start", map[string]any{"req": "cid"})
	Close()

	if _, err := os.Stat(filepath.Join(dir, filepath.Base(defaultRel))); err != nil {
		t.Fatalf("expected the default file name inside the directory: %v", err)
	}
}

func TestHelpersMeasureRepeats(t *testing.T) {
	existing := "我先说明一下思路，然后给出调用：<|EPSE|tool_calls>"
	if got := PrefixMatchLen(existing, "我先说明"); got != len("我先说明") {
		t.Fatalf("PrefixMatchLen = %d", got)
	}
	tail := "上一轮尾部"
	if got := InteriorMatchOffset(existing, tail+existing, 32, 4096); got != len(tail) {
		t.Fatalf("InteriorMatchOffset = %d, want %d", got, len(tail))
	}
	if got := InteriorMatchOffset(existing, "完全不同的另一段内容，与已累积文本没有任何重合。", 32, 4096); got != -1 {
		t.Fatalf("expected no interior match, got %d", got)
	}
	// A match beyond the scan limit is ignored, so the scan stays bounded.
	filler := strings.Repeat("填", 2048)
	if got := InteriorMatchOffset(existing, filler+existing, 32, 64); got != -1 {
		t.Fatalf("expected the far match to be out of range, got %d", got)
	}
	if got := CountInvocations(`<|EPSE|invoke name="a"></|EPSE|invoke><invoke name="b"></invoke>`); got != 2 {
		t.Fatalf("CountInvocations = %d, want 2", got)
	}
	if got := CountWrapperOpens(`<|EPSE|tool_calls><|EPSE|invoke name="a"></|EPSE|invoke></|EPSE|tool_calls>`); got != 1 {
		t.Fatalf("CountWrapperOpens = %d, want 1", got)
	}
	if got := CountWrapperOpens(`<tool_calls><invoke name="a"></invoke></tool_calls>`); got != 1 {
		t.Fatalf("CountWrapperOpens = %d, want 1", got)
	}
	if got := Hash("abc"); len(got) != 16 {
		t.Fatalf("Hash length = %d", len(got))
	}
	if got := Preview("第一行\n第二行\t制表\x01", 6); got != "第一行 第二" {
		t.Fatalf("Preview = %q", got)
	}
	if got := Preview("abcdef", 0); got != "" {
		t.Fatalf("Preview with limit 0 = %q", got)
	}
}
