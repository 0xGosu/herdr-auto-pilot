// Package control implements the daemon control channel: a lightweight
// local socket carrying reload/wake nudges from front-ends and the mcp
// process (NFR-009, no idle polling per NFR-003). Nudges carry no domain
// payload — data is already committed to the DB before the nudge.
package control

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Kind is the nudge type.
type Kind string

const (
	KindReload Kind = "reload"
	KindWake   Kind = "wake"
	// KindReembed asks the daemon to rebuild a FRESH embedder (clearing any
	// degraded-failure latch) and re-embed stored signatures from their
	// salient text. Daemons predating this kind log and ignore it — the
	// stale-daemon remedy (`hap daemon --ensure`) applies.
	KindReembed Kind = "reembed"
)

type message struct {
	Kind Kind `json:"kind"`
}

// Server accepts nudges and invokes the handler (debounced).
type Server struct {
	ln        net.Listener
	handler   func(Kind)
	debounce  time.Duration
	pending   chan Kind
	done      chan struct{}
	closeOnce sync.Once
}

// NewServer starts the control listener at path with owner-only
// permissions. handler runs on a dedicated dispatch goroutine.
func NewServer(path string, handler func(Kind)) (*Server, error) {
	ln, err := listen(path)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln: ln, handler: handler, debounce: 100 * time.Millisecond,
		pending: make(chan Kind, 64), done: make(chan struct{}),
	}
	go s.dispatch()
	go s.accept()
	return s, nil
}

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			// Listener closed (or fatal accept error): stop dispatching.
			// pending is never closed, so lingering connection goroutines
			// cannot panic; they bail out via the done channel instead.
			s.shutdown()
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			scanner := bufio.NewScanner(c)
			for scanner.Scan() {
				var m message
				if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
					slog.Warn("malformed control nudge ignored", "error", err)
					continue
				}
				if m.Kind != KindReload && m.Kind != KindWake && m.Kind != KindReembed {
					slog.Warn("unknown control nudge kind ignored", "kind", m.Kind)
					continue
				}
				select {
				case s.pending <- m.Kind:
				case <-s.done:
					return
				default: // burst overflow: nudges are idempotent, drop is safe
				}
			}
		}(conn)
	}
}

// dispatch debounces bursts: consecutive nudges of the same kind within the
// debounce window collapse into one handler call.
func (s *Server) dispatch() {
	for {
		var kind Kind
		select {
		case <-s.done:
			return
		case kind = <-s.pending:
		}
		timer := time.NewTimer(s.debounce)
	drain:
		for {
			select {
			case k := <-s.pending:
				if k != kind {
					// Different kind: handle the current one now, continue
					// with the new kind.
					s.handler(kind)
					kind = k
				}
			case <-timer.C:
				break drain
			case <-s.done:
				timer.Stop()
				s.handler(kind)
				return
			}
		}
		timer.Stop()
		s.handler(kind)
	}
}

func (s *Server) shutdown() {
	s.closeOnce.Do(func() { close(s.done) })
}

// Close stops the listener and the dispatch loop.
func (s *Server) Close() error {
	err := s.ln.Close()
	s.shutdown()
	return err
}

// Nudge sends a nudge to the daemon control socket at path. Errors are
// returned for surfacing but a failed nudge is never fatal: the kill switch
// is read from the DB every tick regardless.
func Nudge(ctx context.Context, path string, kind Kind) error {
	conn, err := dial(ctx, path)
	if err != nil {
		return err
	}
	defer conn.Close()
	data, _ := json.Marshal(message{Kind: kind})
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err = conn.Write(append(data, '\n'))
	return err
}
