package session

import "sync"

// Transcript is a bounded record of the raw bytes the remote shell has emitted, kept so
// that a prompt regex that doesn't match can be diagnosed from what the shell actually
// printed. Escape sequences are deliberately preserved — stripping them would hide the
// very thing being diagnosed.
//
// It is a growable buffer compacted on a threshold rather than a true ring: at these
// sizes the amortized copy is cheaper than the bookkeeping a wrapped buffer needs.
type Transcript struct {
	mu   sync.Mutex
	buf  []byte
	size int
}

// NewTranscript returns a transcript retaining at least the most recent size bytes. A
// size of zero disables recording entirely.
func NewTranscript(size int) *Transcript {
	if size < 0 {
		size = 0
	}
	return &Transcript{size: size}
}

// Write records p. It never fails and never blocks on anything but its own lock, because
// it sits on the PTY read path — stalling here would stall the remote shell.
func (t *Transcript) Write(p []byte) {
	if t.size == 0 || len(p) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.size {
		copy(t.buf, t.buf[len(t.buf)-t.size:])
		t.buf = t.buf[:t.size]
	}
}

// Tail returns a copy of the most recent n bytes, capped at the retention size. A
// non-positive n means "as much as is retained".
func (t *Transcript) Tail(n int) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()

	if n <= 0 || n > t.size {
		n = t.size
	}
	if n > len(t.buf) {
		n = len(t.buf)
	}
	out := make([]byte, n)
	copy(out, t.buf[len(t.buf)-n:])
	return out
}

// Size reports the retention target in bytes.
func (t *Transcript) Size() int { return t.size }
