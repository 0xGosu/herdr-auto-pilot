package llm

import "sync"

// capWriter is an io.Writer that stops accumulating past a byte cap and
// remembers that it did.
//
// It exists because the LLM adapters read a child's stdout and stderr into
// memory and only check the size AFTER the process exits. For a well-behaved
// CLI that is fine, and it is how the consult, task-generation and
// learn-from-correction runs have always worked. For a faulty one it is not:
// a judge writing continuously until its timeout accumulates everything it
// produced in that window before anything rejects it, and stderr — which no
// cap covers at all, since only a tail of it is ever reported — accumulates
// beside it.
//
// Writes past the cap are DISCARDED rather than treated as an error: an error
// from cmd.Stdout makes exec kill the pipe copy and surface a write failure,
// which would report a misbehaving CLI as a broken one. The caller asks
// Overflowed() instead and produces the same "oversized output" refusal it
// produced before, just without having held the excess.
//
// Safe for concurrent use: exec copies stdout and stderr on separate
// goroutines, and a caller may share one limiter across both.
type capWriter struct {
	mu       sync.Mutex
	buf      []byte
	cap      int
	overflow bool
}

func newCapWriter(max int) *capWriter { return &capWriter{cap: max} }

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Always report the full length written: the writer's contract with exec is
	// "I consumed this", and a short count is an error to the copier.
	if room := w.cap - len(w.buf); room > 0 {
		if len(p) <= room {
			w.buf = append(w.buf, p...)
			return len(p), nil
		}
		w.buf = append(w.buf, p[:room]...)
	}
	w.overflow = true
	return len(p), nil
}

// String returns what was kept, which is at most the cap.
func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

// Overflowed reports whether anything was discarded.
func (w *capWriter) Overflowed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.overflow
}
