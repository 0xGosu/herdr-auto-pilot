package sqlbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"
)

// ServerOptions bounds the socket server.
type ServerOptions struct {
	// IdleTimeout closes a client that sends nothing for this long (0 = 10m).
	// Sessions hold a dedicated database connection each.
	IdleTimeout time.Duration
	// MaxClients caps concurrent sessions (0 = 8). The executor's underlying
	// pool must be sized past it, or a stuck TUI starves the daemon's own
	// writes.
	MaxClients int
}

// DefaultMaxClients is how many front-end processes may hold a session at once.
const DefaultMaxClients = 8

// Server serves an Executor over a unix socket.
type Server struct {
	ln   net.Listener
	e    *Executor
	opts ServerOptions
	sem  chan struct{}
	wg   sync.WaitGroup
	ctx  context.Context
	stop context.CancelFunc
}

// Serve accepts connections on ln until Close. It returns at once; the accept
// loop runs on its own goroutine.
func Serve(ln net.Listener, e *Executor, opts ServerOptions) *Server {
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 10 * time.Minute
	}
	if opts.MaxClients <= 0 {
		opts.MaxClients = DefaultMaxClients
	}
	ctx, stop := context.WithCancel(context.Background())
	s := &Server{ln: ln, e: e, opts: opts, sem: make(chan struct{}, opts.MaxClients), ctx: ctx, stop: stop}
	s.wg.Add(1)
	go s.accept()
	return s
}

// Close stops accepting, cancels every session's statement, and waits.
func (s *Server) Close() error {
	s.stop()
	err := s.ln.Close()
	s.wg.Wait()
	return err
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			slog.Debug("store socket: accept failed", "error", err)
			// A transient accept error must not spin.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		select {
		case s.sem <- struct{}{}:
		default:
			// Over capacity: answer the first frame with an error and close,
			// so the client reports something readable instead of hanging.
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.refuse(c)
			}()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.sem }()
			s.serveConn(c)
		}()
	}
}

func (s *Server) refuse(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	var req request
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&req); err != nil {
		return
	}
	_ = json.NewEncoder(c).Encode(response{ID: req.ID, Kind: kindErr,
		Message: "too many processes are using the store at once (" + strconv.Itoa(s.opts.MaxClients) + "); retry"})
}

func (s *Server) serveConn(c net.Conn) {
	defer c.Close()
	sess, err := s.e.Session(s.ctx)
	if err != nil {
		_ = json.NewEncoder(c).Encode(response{Kind: kindErr, Message: err.Error()})
		return
	}
	defer sess.Close()
	// A statement in flight is abandoned when the server closes.
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	stopWatch := context.AfterFunc(s.ctx, func() { _ = c.SetDeadline(time.Now()) })
	defer stopWatch()

	dec := json.NewDecoder(bufio.NewReaderSize(c, 64<<10))
	enc := json.NewEncoder(c)
	for {
		_ = c.SetReadDeadline(time.Now().Add(s.opts.IdleTimeout))
		var req request
		if err := dec.Decode(&req); err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				slog.Debug("store socket: client left", "error", err)
			}
			return
		}
		_ = c.SetReadDeadline(time.Time{})
		resp := s.handle(ctx, sess, req)
		resp.ID = req.ID
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (s *Server) handle(ctx context.Context, sess *Session, req request) response {
	fail := func(err error) response { return response{Kind: kindErr, Message: err.Error()} }
	switch req.Kind {
	case kindExec:
		args, err := decodeValues(req.Args)
		if err != nil {
			return fail(err)
		}
		lastID, affected, err := sess.Exec(ctx, req.SQL, asAny(args))
		if err != nil {
			return fail(err)
		}
		return response{Kind: kindOK, LastID: strconv.FormatInt(lastID, 10), Affected: strconv.FormatInt(affected, 10)}
	case kindQuery:
		args, err := decodeValues(req.Args)
		if err != nil {
			return fail(err)
		}
		cols, rows, err := sess.Query(ctx, req.SQL, asAny(args))
		if err != nil {
			return fail(err)
		}
		out := make([][]wireValue, len(rows))
		for i, row := range rows {
			enc, err := encodeValues(asAny(row))
			if err != nil {
				return fail(err)
			}
			out[i] = enc
		}
		if cols == nil {
			cols = []string{}
		}
		return response{Kind: kindRows, Columns: cols, Rows: out}
	case kindBegin:
		if err := sess.Begin(ctx); err != nil {
			return fail(err)
		}
	case kindCommit:
		if err := sess.Commit(ctx); err != nil {
			return fail(err)
		}
	case kindRollback:
		if err := sess.Rollback(ctx); err != nil {
			return fail(err)
		}
	case kindPing:
		if err := sess.Ping(ctx); err != nil {
			return fail(err)
		}
	default:
		return fail(errors.New("sqlbridge: unknown request kind " + strconv.Quote(req.Kind)))
	}
	return response{Kind: kindOK}
}

func asAny[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}
