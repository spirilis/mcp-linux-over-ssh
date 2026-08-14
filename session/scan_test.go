package session

import (
	"regexp"
	"strings"
	"testing"
)

func TestMarkerFindParsesExitCode(t *testing.T) {
	m := newMarker("deadbeef")
	buf := []byte("hello\n__MCPEND_deadbeef__:7\n$ ")

	start, code, ok := m.find(buf, 0)
	if !ok {
		t.Fatal("expected the marker to be found")
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	if got := string(buf[:start]); got != "hello\n" {
		t.Errorf("region before marker = %q, want %q", got, "hello\n")
	}
}

// A marker whose line has not fully arrived must not be reported: parsing a half-written
// exit code would hand back a wrong status and desynchronize the next command.
func TestMarkerFindWaitsForCompleteLine(t *testing.T) {
	m := newMarker("deadbeef")
	if _, _, ok := m.find([]byte("out\n__MCPEND_deadbeef__:1"), 0); ok {
		t.Error("marker without a trailing newline should not be reported as found")
	}
	if _, _, ok := m.find([]byte("out\n__MCPEND_deadbeef__:"), 0); ok {
		t.Error("marker without an exit code should not be reported as found")
	}
}

// The real read path grows the buffer a chunk at a time; the marker must be found exactly
// once the final byte lands, no matter where the chunk boundaries fall.
func TestMarkerFoundAcrossSplitReads(t *testing.T) {
	m := newMarker("cafebabe")
	full := "some output\n__MCPEND_cafebabe__:0\n"

	for split := 1; split < len(full); split++ {
		var acc []byte
		scanned := 0
		foundAt := -1

		for _, chunk := range []string{full[:split], full[split:]} {
			acc = append(acc, chunk...)
			from := scanned - scanOverlap
			if from < 0 {
				from = 0
			}
			if _, code, ok := m.find(acc, from); ok {
				foundAt = code
				break
			}
			scanned = len(acc)
		}

		if foundAt != 0 {
			t.Fatalf("split at %d: marker not found with exit code 0 (got %d)", split, foundAt)
		}
	}
}

// A fresh nonce per command is what stops a marker left over from an abandoned command
// from satisfying the next one.
func TestMarkerIgnoresForeignNonce(t *testing.T) {
	current := newMarker("1111")
	stale := "__MCPEND_2222__:0\n"
	if _, _, ok := current.find([]byte(stale), 0); ok {
		t.Error("a marker from a different command must not match")
	}
}

// With PTY echo on, the command line comes back before the output. The marker is built
// from two string literals precisely so the echo can be told apart from the real thing.
func TestStripEchoRemovesEchoedCommandLine(t *testing.T) {
	m := newMarker("abc123")
	region := []byte(m.command("echo hi") + "\r\nhi\n")
	region = normalizeNewlines(region)

	if got := string(m.stripEcho(region)); got != "hi\n" {
		t.Errorf("stripEcho = %q, want %q", got, "hi\n")
	}
}

// A narrow terminal wraps the echoed command across lines. Locating the echo by its marker
// signature rather than by string-comparing the command survives that.
func TestStripEchoSurvivesWrappedEcho(t *testing.T) {
	m := newMarker("abc123")
	line := m.command("echo hi")
	wrapped := line[:20] + "\n" + line[20:] + "\n"

	if got := string(m.stripEcho([]byte(wrapped + "hi\n"))); got != "hi\n" {
		t.Errorf("stripEcho = %q, want %q", got, "hi\n")
	}
}

func TestStripEchoNoopWhenEchoDisabled(t *testing.T) {
	m := newMarker("abc123")
	if got := string(m.stripEcho([]byte("hi\n"))); got != "hi\n" {
		t.Errorf("stripEcho = %q, want %q", got, "hi\n")
	}
}

func TestStripLeadingEcho(t *testing.T) {
	if got := string(stripLeadingEcho([]byte("ls -l\noutput\n"), "ls -l")); got != "output\n" {
		t.Errorf("stripLeadingEcho = %q, want %q", got, "output\n")
	}
	if got := string(stripLeadingEcho([]byte("output\n"), "ls -l")); got != "output\n" {
		t.Errorf("stripLeadingEcho should be a no-op when nothing was echoed, got %q", got)
	}
}

func TestStripANSI(t *testing.T) {
	in := []byte("\x1b[1;32muser@host\x1b[0m:\x1b[34m~\x1b[0m$ \x1b]0;title\x07\x1b[?2004h")
	if got := string(stripANSI(in)); got != "user@host:~$ " {
		t.Errorf("stripANSI = %q, want %q", got, "user@host:~$ ")
	}
}

// An escape sequence or a CRLF pair can straddle two PTY reads. The filter must hold the
// unfinished tail back rather than emit it half-cleaned.
func TestANSIFilterHandlesSplitSequences(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
	}{
		{"split CSI", []string{"a\x1b[1;3", "2mb\r\nc\r\n"}},
		{"split CRLF", []string{"a\x1b[1;32mb\r", "\nc\r\n"}},
		{"escape at boundary", []string{"a\x1b", "[1;32mb\r\nc\r\n"}},
		{"split OSC", []string{"a\x1b]0;ti", "tle\x07b\r\nc\r\n"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f ansiFilter
			var out strings.Builder
			for _, c := range tc.chunks {
				out.Write(f.filter([]byte(c)))
			}
			if got := out.String(); got != "ab\nc\n" {
				t.Errorf("filtered = %q, want %q", got, "ab\nc\n")
			}
		})
	}
}

// Cleaning the stream as it arrives is what lets a prompt regex be written against the
// text a human sees rather than against SGR-interleaved bytes.
func TestANSIFilterLetsPlainPromptRegexMatchColourisedPrompt(t *testing.T) {
	var f ansiFilter
	cleaned := f.filter([]byte("\x1b[1;32muser@host\x1b[0m:\x1b[34m~\x1b[0m$ "))

	re := regexp.MustCompile(`(?m)^[^\n]*[$#]\s*$`)
	if findPrompt(re, cleaned, 0) < 0 {
		t.Errorf("prompt regex did not match cleaned output %q", cleaned)
	}
}

func TestFindPromptWindowIsBounded(t *testing.T) {
	re := regexp.MustCompile(`(?m)^[^\n]*[$#]\s*$`)

	// A prompt buried further back than the window is deliberately not matched.
	buf := append([]byte("user@host:~$ \n"), make([]byte, promptScanWindow+1024)...)
	for i := 14; i < len(buf); i++ {
		buf[i] = 'x'
	}
	if idx := findPrompt(re, buf, 0); idx >= 0 {
		t.Errorf("prompt outside the scan window should not match, got index %d", idx)
	}

	// One at the end is.
	buf = append(buf, []byte("\nuser@host:~$ ")...)
	if idx := findPrompt(re, buf, 0); idx < 0 {
		t.Error("prompt at the tail should match")
	}
}

func TestTruncateMiddleKeepsHeadAndTail(t *testing.T) {
	s := strings.Repeat("a", 100) + strings.Repeat("b", 100)

	out, truncated := truncateMiddle(s, 100)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if !strings.HasPrefix(out, strings.Repeat("a", 50)) {
		t.Error("head was not preserved")
	}
	if !strings.HasSuffix(out, strings.Repeat("b", 50)) {
		t.Error("tail was not preserved")
	}
	if !strings.Contains(out, "100 bytes elided") {
		t.Errorf("expected an elision note, got %q", out)
	}

	if out, truncated := truncateMiddle("short", 100); truncated || out != "short" {
		t.Errorf("short input should pass through unchanged, got %q truncated=%v", out, truncated)
	}
}

func TestTranscriptRetainsTail(t *testing.T) {
	tr := NewTranscript(64)
	for i := 0; i < 100; i++ {
		tr.Write([]byte("0123456789"))
	}

	tail := tr.Tail(0)
	if len(tail) != 64 {
		t.Fatalf("tail length = %d, want 64", len(tail))
	}
	if !strings.HasSuffix(string(tail), "0123456789") {
		t.Errorf("tail should end with the most recent write, got %q", tail)
	}
	if got := len(tr.Tail(10)); got != 10 {
		t.Errorf("Tail(10) length = %d, want 10", got)
	}
}

func TestTranscriptDisabled(t *testing.T) {
	tr := NewTranscript(0)
	tr.Write([]byte("anything"))
	if got := tr.Tail(0); len(got) != 0 {
		t.Errorf("a zero-size transcript should retain nothing, got %q", got)
	}
}
