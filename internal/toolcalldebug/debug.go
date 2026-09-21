// Package toolcalldebug writes an opt-in JSONL trace of the streaming tool-call
// pipeline.
//
// It exists to diagnose duplicate or corrupted tool calls on a live deployment,
// where the only thing that can settle the question is what the pipeline did
// with the upstream text:
//
//   - how the upstream text was split into parts, and which of those parts were
//     recognised as a replay of text that was already accumulated,
//   - how much of each part reproduced already accumulated text (a replay that
//     was NOT recognised shows up here),
//   - which tool calls the sieve produced and which ones were emitted, with
//     their ids and indexes,
//   - how many `invoke` blocks the accumulated text holds in total.
//
// The trace never stores the message text itself: only lengths, short hashes,
// counted invocations and bounded previews. It is meant to be sent as-is when a
// duplicate-call report has to be pinned down without sharing a full capture.
//
// Enable it with:
//
//	DS2API_DEBUG_TOOLCALL=1        -> logs/toolcall-debug.jsonl under the working dir
//	DS2API_DEBUG_TOOLCALL=<path>   -> that file (".jsonl" appended when it is a dir)
//	DS2API_DEBUG_TOOLCALL=stdout   -> the process log
//
// It is off by default; when off, every call is a single atomic load.
package toolcalldebug

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	envVar     = "DS2API_DEBUG_TOOLCALL"
	defaultRel = "logs/toolcall-debug.jsonl"
	// maxEntriesPerFile stops a forgotten switch from filling a disk.
	maxEntriesPerFile = 200000
)

var (
	enabled   atomic.Bool
	initOnce  sync.Once
	writeMu   sync.Mutex
	out       io.Writer
	entries   int
	dropped   int
	closed    bool
	targetTxt string
)

// Enabled reports whether the trace is on. Cheap enough to call per event.
func Enabled() bool {
	initOnce.Do(initTarget)
	return enabled.Load()
}

// Target returns the resolved destination, for logging at startup.
func Target() string {
	initOnce.Do(initTarget)
	return targetTxt
}

func initTarget() {
	raw := strings.TrimSpace(os.Getenv(envVar))
	if raw == "" {
		return
	}
	switch strings.ToLower(raw) {
	case "0", "false", "off", "no":
		return
	case "1", "true", "on", "yes":
		raw = defaultRel
	case "stdout":
		out = os.Stdout
		targetTxt = "stdout"
		enabled.Store(true)
		announce()
		return
	}

	path := raw
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, filepath.Base(defaultRel))
	} else if !strings.Contains(filepath.Base(path), ".") {
		path = path + ".jsonl"
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "%s: cannot create %s: %v\n", envVar, dir, err)
			return
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: cannot open %s: %v\n", envVar, path, err)
		return
	}
	out = file
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	targetTxt = abs
	enabled.Store(true)
	announce()
}

func announce() {
	fmt.Fprintf(os.Stderr, "%s: tool-call trace enabled -> %s\n", envVar, targetTxt)
}

// Log writes one JSONL record. It is a no-op when the trace is off.
func Log(event string, fields map[string]any) {
	if !Enabled() {
		return
	}
	record := make(map[string]any, len(fields)+2)
	record["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	record["event"] = event
	for key, value := range fields {
		record[key] = value
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}

	writeMu.Lock()
	defer writeMu.Unlock()
	if closed || out == nil {
		return
	}
	if entries >= maxEntriesPerFile {
		if dropped == 0 {
			dropped++
			_, _ = fmt.Fprintf(out, "{\"event\":\"trace_full\",\"detail\":\"%s reached %d entries\"}\n", envVar, maxEntriesPerFile)
		}
		return
	}
	if _, err := out.Write(append(line, '\n')); err != nil {
		if closer, ok := out.(io.Closer); ok {
			_ = closer.Close()
		}
		closed = true
		dropped++
		return
	}
	entries++
}

// Close flushes and closes the trace target. Tests use it to reopen.
func Close() {
	initOnce.Do(initTarget)
	writeMu.Lock()
	defer writeMu.Unlock()
	if closer, ok := out.(io.Closer); ok && out != os.Stdout {
		_ = closer.Close()
	}
	closed = true
	enabled.Store(false)
}

// ResetForTest restores the package to its pristine state so a test can point
// the trace at a temporary file.
func ResetForTest() {
	Close()
	writeMu.Lock()
	out = nil
	entries = 0
	dropped = 0
	closed = false
	targetTxt = ""
	writeMu.Unlock()
	initOnce = sync.Once{}
	enabled.Store(false)
}

// Hash returns a short, stable fingerprint of text.
func Hash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}

// Preview returns the first limit runes of text with control characters
// flattened, so a trace line stays readable and single-line.
func Preview(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	out := make([]rune, 0, limit)
	for _, r := range text {
		if len(out) >= limit {
			break
		}
		if r == '\n' || r == '\r' || r == '\t' {
			out = append(out, ' ')
			continue
		}
		if r < 0x20 {
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

// CountInvocations counts the tool calls in text. Both the extended and the
// legacy markup spell the invoke tag with a `name` attribute.
func CountInvocations(text string) int {
	return strings.Count(text, "invoke name")
}

// CountWrapperOpens counts how many tool-call wrappers text opens. Together with
// CountInvocations it separates "one wrapper holding every call twice" from "the
// wrapper itself was appended twice".
func CountWrapperOpens(text string) int {
	return strings.Count(text, "<|EPSE|tool_calls>") + strings.Count(text, "<tool_calls>")
}

// PrefixMatchLen returns how many bytes at the start of incoming reproduce the
// start of existing, rounded down to a UTF-8 boundary of incoming.
func PrefixMatchLen(existing, incoming string) int {
	limit := len(existing)
	if len(incoming) < limit {
		limit = len(incoming)
	}
	i := 0
	for i < limit && existing[i] == incoming[i] {
		i++
	}
	if i == limit || i == len(incoming) {
		return i
	}
	for i > 0 && !utf8.RuneStart(incoming[i]) {
		i--
	}
	return i
}

// InteriorMatchOffset looks for the first probePrefix of existing inside
// incoming after offset 0, which is what a replay that was merged with the tail
// of the previous round looks like. It returns -1 when there is none and
// ignores matches found beyond limit bytes, so the scan stays bounded.
func InteriorMatchOffset(existing, incoming string, probeBytes, limit int) int {
	if probeBytes <= 0 || len(existing) < probeBytes || limit <= 0 || len(incoming) <= probeBytes {
		return -1
	}
	probe := existing[:probeBytes]
	end := len(incoming) - probeBytes
	if end > limit {
		end = limit
	}
	for i := 1; i <= end; i++ {
		if incoming[i:i+probeBytes] == probe {
			return i
		}
	}
	return -1
}
