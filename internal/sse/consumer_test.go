package sse

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCollectStreamDedupesContinueSnapshotReplay(t *testing.T) {
	prefix := "我们被问到：这是一个很长的续答快照前缀，用来验证去重逻辑不会误伤正常 token。"
	body := strings.Join([]string{
		`data: {"v":{"response":{"fragments":[{"id":2,"type":"THINK","content":"` + prefix + `","references":[],"stage_id":1}]}}}`,
		``,
		`data: {"p":"response/status","v":"INCOMPLETE"}`,
		``,
		`data: {"v":{"response":{"fragments":[{"id":2,"type":"THINK","content":"` + prefix + `继续","references":[],"stage_id":1}]}}}`,
		``,
		`data: {"v":"分析"}`,
		``,
		`data: {"p":"response/status","v":"FINISHED"}`,
		``,
	}, "\n")

	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
	got := CollectStream(resp, true, true)
	if got.Thinking != prefix+"继续分析" {
		t.Fatalf("unexpected thinking after dedupe: %q", got.Thinking)
	}
}

func TestCollectStreamDropsTokenSizedReplay(t *testing.T) {
	block := `<tool_calls><invoke name="search"><parameter name="q">golang duplicate tool call replay</parameter></invoke></tool_calls>`
	message := "我先说明一下思路，然后给出调用：" + block + " 然后就结束了。"
	line := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal failed: %v", err)
		}
		return "data: " + string(b)
	}

	lines := []string{line(map[string]any{"p": "response/content", "v": message})}
	runes := []rune(message)
	// A continue round that resends the message one small fragment at a time.
	for i := 0; i < len(runes); i += 24 {
		end := i + 24
		if end > len(runes) {
			end = len(runes)
		}
		lines = append(lines, line(map[string]any{"p": "response/content", "v": string(runes[i:end])}))
	}
	lines = append(lines,
		line(map[string]any{"p": "response/content", "v": " 后面是新增内容。"}),
		"data: [DONE]",
		``,
	)

	resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Join(lines, "\n")))}
	got := CollectStream(resp, true, true)
	if want := message + " 后面是新增内容。"; got.Text != want {
		t.Fatalf("expected %q, got %q", want, got.Text)
	}
	if count := strings.Count(got.Text, block); count != 1 {
		t.Fatalf("expected a single tool-call block, got %d in %q", count, got.Text)
	}
}

func TestCollectStreamKeepsSingleCopyAcrossContinueRounds(t *testing.T) {
	first := "第一段很长很长很长很长的回答内容，用来验证多轮 continue 不会重复累积。"
	second := first + " 第二段补充。"
	third := second + " 第三段补充。"
	body := strings.Join([]string{
		`data: {"p":"response/content","v":"` + first + `"}`,
		`data: {"p":"response/status","v":"INCOMPLETE"}`,
		``,
		`data: {"v":{"response":{"fragments":[{"id":2,"type":"RESPONSE","content":"` + second + `"}]}}}`,
		`data: {"p":"response/content","v":" 第三段补充。"}`,
		``,
		`data: {"p":"response/status","v":"FINISHED"}`,
		``,
	}, "\n")

	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
	got := CollectStream(resp, true, true)
	if got.Text != third {
		t.Fatalf("expected %q, got %q", third, got.Text)
	}
}
