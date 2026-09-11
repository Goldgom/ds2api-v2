package completionruntime

import (
	"testing"

	"ds2api/internal/promptcompat"
)

type mockExpertSegmentConfig struct {
	enabled     bool
	maxChars    int
	stopDelayMs int
}

func (m mockExpertSegmentConfig) ExpertPromptSegmentEnabled() bool    { return m.enabled }
func (m mockExpertSegmentConfig) ExpertPromptSegmentMaxChars() int    { return m.maxChars }
func (m mockExpertSegmentConfig) ExpertPromptSegmentStopDelayMs() int { return m.stopDelayMs }

func TestShouldSegmentExpertPrompt_DisabledReturnsNil(t *testing.T) {
	stdReq := promptcompat.StandardRequest{
		ResolvedModel: "deepseek-v4-pro",
		FinalPrompt:   stringRepeat("X", 200000),
	}
	opts := Options{ExpertPromptSegment: mockExpertSegmentConfig{enabled: false, maxChars: 120000}}
	if segs := shouldSegmentExpertPrompt(stdReq, opts); segs != nil {
		t.Fatalf("expected nil when disabled, got %d segments", len(segs))
	}
}

func TestShouldSegmentExpertPrompt_NonExpertReturnsNil(t *testing.T) {
	stdReq := promptcompat.StandardRequest{
		ResolvedModel: "deepseek-v4-flash",
		FinalPrompt:   stringRepeat("X", 200000),
	}
	opts := Options{ExpertPromptSegment: mockExpertSegmentConfig{enabled: true, maxChars: 120000}}
	if segs := shouldSegmentExpertPrompt(stdReq, opts); segs != nil {
		t.Fatalf("expected nil for non-expert model, got %d segments", len(segs))
	}
}

func TestShouldSegmentExpertPrompt_UnderThresholdReturnsNil(t *testing.T) {
	stdReq := promptcompat.StandardRequest{
		ResolvedModel: "deepseek-v4-pro",
		FinalPrompt:   "short prompt",
	}
	opts := Options{ExpertPromptSegment: mockExpertSegmentConfig{enabled: true, maxChars: 120000}}
	if segs := shouldSegmentExpertPrompt(stdReq, opts); segs != nil {
		t.Fatalf("expected nil for short prompt, got %d segments", len(segs))
	}
}

func TestShouldSegmentExpertPrompt_RemovedExpertModelReturnsNil(t *testing.T) {
	stdReq := promptcompat.StandardRequest{
		ResolvedModel: "deepseek-v4-pro",
		FinalPrompt:   "<User>:" + stringRepeat("X", 200000) + "<Assistant>:",
	}
	opts := Options{ExpertPromptSegment: mockExpertSegmentConfig{enabled: true, maxChars: 1000}}
	if segs := shouldSegmentExpertPrompt(stdReq, opts); segs != nil {
		t.Fatalf("expected nil for removed expert model, got %d segments", len(segs))
	}
}

func TestShouldSegmentExpertPrompt_NoReaderReturnsNil(t *testing.T) {
	stdReq := promptcompat.StandardRequest{
		ResolvedModel: "deepseek-v4-pro",
		FinalPrompt:   stringRepeat("X", 200000),
	}
	opts := Options{}
	if segs := shouldSegmentExpertPrompt(stdReq, opts); segs != nil {
		t.Fatalf("expected nil when no reader provided, got %d segments", len(segs))
	}
}

func TestSegmentStopDelay(t *testing.T) {
	opts := Options{ExpertPromptSegment: mockExpertSegmentConfig{stopDelayMs: 3000}}
	d := segmentStopDelay(opts)
	if d.Milliseconds() != 3000 {
		t.Fatalf("expected 3000ms, got %dms", d.Milliseconds())
	}
}

func stringRepeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
