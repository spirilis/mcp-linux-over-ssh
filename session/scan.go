package session

import (
	"bytes"
	"fmt"
	"regexp"
)

// scanOverlap is how far back a marker search re-reads on each pass so a marker split
// across two PTY reads is still found. It must exceed the longest marker line.
const scanOverlap = 256

// promptScanWindow bounds how much of the tail a prompt regex is matched against. Regex
// matching has no fixed pattern length to overlap by, so rescanning is bounded by a
// window instead. A prompt is always at the end of the output, so this loses nothing in
// practice while keeping the fallback framing path off an O(n^2) rescan.
const promptScanWindow = 64 << 10

// ANSI/terminal control sequences a PTY interleaves with real output. Bracketed paste
// (CSI ?2004h / ?2004l) is covered by the CSI form.
//
// The final alternative deliberately excludes every introducer handled above it —
// `[` (CSI), `]` (OSC), and P/X/^/_ (DCS/SOS/PM/APC). Those begin sequences that only end
// at a terminator, and matching one as a self-contained two-byte escape would both leave
// its payload in the output and convince the incremental filter that a half-arrived
// sequence was already complete.
var (
	csiSeq   = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	oscSeq   = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	strSeq   = regexp.MustCompile(`\x1b[P^_X][^\x1b]*\x1b\\`)
	otherEsc = regexp.MustCompile(`\x1b[@-OQ-WY-Z\\]`)
)

// completeEsc matches, anchored at the start, one whole escape sequence. It is how the
// incremental filter decides whether a trailing escape has finished arriving.
var completeEsc = regexp.MustCompile(`^(?:\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[P^_X][^\x1b]*\x1b\\|\x1b[@-OQ-WY-Z\\])`)

// escLookback bounds how far back the filter searches for an unfinished escape sequence.
const escLookback = 64

// ansiFilter cleans the PTY byte stream incrementally: escape sequences are removed and
// CRLF is folded to LF before anything reaches the accumulator.
//
// Cleaning here rather than at match time is what lets a user-supplied prompt regex be
// written against the text a human sees. A colourised PS1 would otherwise arrive
// interleaved with SGR sequences and defeat any reasonable pattern. The raw bytes are
// still preserved in the transcript, which is the thing meant for diagnosis.
//
// A sequence — or a CRLF pair — can straddle two reads, so an unfinished tail is carried
// forward instead of being emitted half-cleaned.
type ansiFilter struct {
	carry []byte
}

func (f *ansiFilter) filter(p []byte) []byte {
	b := p
	if len(f.carry) > 0 {
		b = append(append([]byte(nil), f.carry...), p...)
		f.carry = f.carry[:0]
	}

	if i := incompleteTail(b); i >= 0 {
		f.carry = append(f.carry[:0], b[i:]...)
		b = b[:i]
	}
	return normalizeNewlines(stripANSI(b))
}

// incompleteTail reports where an unfinished escape sequence or a dangling CR begins, or
// -1 if b can be cleaned in full.
func incompleteTail(b []byte) int {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return len(b) - 1
	}
	start := len(b) - escLookback
	if start < 0 {
		start = 0
	}
	i := bytes.LastIndexByte(b[start:], 0x1b)
	if i < 0 {
		return -1
	}
	i += start
	if completeEsc.Match(b[i:]) {
		return -1
	}
	return i
}

// stripANSI removes terminal escape sequences from b, returning a new slice.
func stripANSI(b []byte) []byte {
	b = csiSeq.ReplaceAll(b, nil)
	b = oscSeq.ReplaceAll(b, nil)
	b = strSeq.ReplaceAll(b, nil)
	b = otherEsc.ReplaceAll(b, nil)
	return b
}

// normalizeNewlines converts the CRLF a PTY emits into LF. Bare CRs are left alone: they
// are meaningful in progress output, and rewriting them would corrupt it.
func normalizeNewlines(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// marker describes one command's end sentinel.
//
// The line sent to the shell builds the marker from two separate string literals, so the
// text the PTY echoes back contains `'END_<nonce>__'` while the text the shell actually
// prints is the joined `__MCPEND_<nonce>__:<exit code>`. That asymmetry is what lets the
// echo be located and discarded without depending on `stty -echo` having worked.
type marker struct {
	nonce string
	// emitted is what the shell prints: the thing we search for.
	emitted []byte
	// echoSig appears only in the echoed command line, never in real output.
	echoSig []byte
}

func newMarker(nonce string) marker {
	return marker{
		nonce:   nonce,
		emitted: []byte(fmt.Sprintf("__MCPEND_%s__:", nonce)),
		echoSig: []byte(fmt.Sprintf("'END_%s__'", nonce)),
	}
}

// command renders the line to send for cmd. $? is expanded as a printf argument, so it
// carries the exit status of cmd itself.
func (m marker) command(cmd string) string {
	return fmt.Sprintf("%s; printf '%%s%%s:%%d\\n' '__MCP' 'END_%s__' \"$?\"", cmd, m.nonce)
}

// probe renders a bare marker emission, used to test whether the remote shell supports
// this framing at all before relying on it.
func (m marker) probe() string {
	return fmt.Sprintf("printf '%%s%%s:%%d\\n' '__MCP' 'END_%s__' \"$?\"", m.nonce)
}

// find locates the marker in buf at or after from, returning the index where the marker
// line starts, the exit code it carries, and whether a complete marker line was present.
// A marker whose trailing newline has not arrived yet reports false so the caller keeps
// waiting rather than parsing a half-written exit code.
func (m marker) find(buf []byte, from int) (start, exitCode int, ok bool) {
	if from < 0 {
		from = 0
	}
	if from > len(buf) {
		return 0, 0, false
	}
	rel := bytes.Index(buf[from:], m.emitted)
	if rel < 0 {
		return 0, 0, false
	}
	start = from + rel
	tail := buf[start+len(m.emitted):]
	end := bytes.IndexByte(tail, '\n')
	if end < 0 {
		return 0, 0, false
	}
	digits := bytes.TrimSpace(tail[:end])
	if len(digits) == 0 {
		return 0, 0, false
	}
	code := 0
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, 0, false
		}
		code = code*10 + int(c-'0')
	}
	return start, code, true
}

// stripEcho removes the echoed command line from the front of region.
//
// The echoed text is located by its marker signature rather than by comparing against the
// sent string, which makes it immune to the line wrapping a narrow terminal would
// introduce. Everything up to and including the newline that ends the echo is discarded.
func (m marker) stripEcho(region []byte) []byte {
	idx := bytes.LastIndex(region, m.echoSig)
	if idx < 0 {
		return region
	}
	if nl := bytes.IndexByte(region[idx:], '\n'); nl >= 0 {
		return region[idx+nl+1:]
	}
	return region[idx+len(m.echoSig):]
}

// stripLeadingEcho removes a leading echo of cmd when no marker is in play (prompt-only
// framing). Unlike the marker path this cannot survive wrapping, which is one more reason
// the sentinel is preferred.
func stripLeadingEcho(region []byte, cmd string) []byte {
	trimmed := bytes.TrimLeft(region, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte(cmd)) {
		return region
	}
	rest := trimmed[len(cmd):]
	if nl := bytes.IndexByte(rest, '\n'); nl >= 0 {
		return rest[nl+1:]
	}
	return rest
}

// findPrompt matches re against the tail of buf, bounded by promptScanWindow, and returns
// the index where the match starts. It returns -1 when the prompt is not present.
func findPrompt(re *regexp.Regexp, buf []byte, from int) int {
	if re == nil {
		return -1
	}
	start := from
	if start < 0 {
		start = 0
	}
	if len(buf)-start > promptScanWindow {
		start = len(buf) - promptScanWindow
	}
	if start > len(buf) {
		return -1
	}
	loc := re.FindIndex(buf[start:])
	if loc == nil {
		return -1
	}
	return start + loc[0]
}

// truncateMiddle bounds s to max bytes, keeping the head and tail and noting what was
// dropped. Callers still consume the full output from the shell; only what is handed back
// to the model is shortened.
func truncateMiddle(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	half := max / 2
	dropped := len(s) - 2*half
	return fmt.Sprintf("%s\n...[%d bytes elided]...\n%s", s[:half], dropped, s[len(s)-half:]), true
}
