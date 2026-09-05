// Package sqlbridge puts a database/sql driver in front of a *sql.DB.
//
// It exists for the turso engine, where exactly ONE process may hold the sync
// database and two things have to sit between that handle and its users:
//
//   - a GATE: the Turso sync engine's Push/Pull rewrite the local file and
//     must not overlap a statement (verified: unguarded, they fail with
//     "database is locked" and can hang a Push for good). Every statement
//     takes the gate's read lock, a transaction holds it from BEGIN to
//     COMMIT, and a sync operation takes the write lock. Rows are returned
//     EAGERLY so no cursor is ever left open across a sync — an open reader
//     also blocks writers on other connections until busy_timeout.
//
//   - a SOCKET: the TUI, the one-shot CLI verbs and the MCP server are other
//     processes, so they reach the daemon's handle over a unix socket in the
//     state dir. The store's code sees a *sql.DB in both places and never
//     knows which.
//
// One executor serves both: the in-process driver (Connector) and the socket
// server (Serve) run every statement through the same Session, and the remote
// driver (DialConnector) speaks the same frames the server does.
package sqlbridge

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
)

// Executor runs statements against a *sql.DB behind the sync gate.
type Executor struct {
	db *sql.DB
	// gate: statements read-lock it, Push/Pull/Checkpoint write-lock it.
	gate sync.RWMutex
	// onWrite is called after a committed write (nil-safe): the daemon's sync
	// loop debounces a push on it. Statements inside a transaction report at
	// COMMIT, since nothing is durable before it.
	onWrite func()
}

// NewExecutor wraps db. onWrite may be nil.
func NewExecutor(db *sql.DB, onWrite func()) *Executor {
	return &Executor{db: db, onWrite: onWrite}
}

// DB is the wrapped handle.
func (e *Executor) DB() *sql.DB { return e.db }

// Lock takes the sync side of the gate: no statement runs until Unlock.
func (e *Executor) Lock() { e.gate.Lock() }

// Unlock releases the sync side of the gate.
func (e *Executor) Unlock() { e.gate.Unlock() }

func (e *Executor) noteWrite() {
	if e.onWrite != nil {
		e.onWrite()
	}
}

// Session is one client's view of the database: a dedicated *sql.Conn (so a
// transaction has somewhere to live) and at most one open transaction.
type Session struct {
	e    *Executor
	conn *sql.Conn
	tx   *sql.Tx
	mu   sync.Mutex
}

// Session opens a dedicated connection.
func (e *Executor) Session(ctx context.Context) (*Session, error) {
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	return &Session{e: e, conn: conn}, nil
}

// errTxOpen and errNoTx guard the transaction state machine.
var (
	errTxOpen = errors.New("sqlbridge: a transaction is already open on this session")
	errNoTx   = errors.New("sqlbridge: no transaction is open on this session")
)

// acquire takes the read side of the gate for one statement — unless the
// session holds it already for an open transaction, in which case a second
// RLock would deadlock against a waiting sync op (Go's RWMutex blocks new
// readers while a writer waits).
func (s *Session) acquire() func() {
	if s.tx != nil {
		return func() {}
	}
	s.e.gate.RLock()
	return s.e.gate.RUnlock
}

// Exec runs a statement. lastID and affected are best effort (0 when the
// driver cannot say).
func (s *Session) Exec(ctx context.Context, query string, args []any) (lastID, affected int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	release := s.acquire()
	defer release()
	var res sql.Result
	if s.tx != nil {
		res, err = s.tx.ExecContext(ctx, query, args...)
	} else {
		res, err = s.conn.ExecContext(ctx, query, args...)
	}
	if err != nil {
		return 0, 0, err
	}
	lastID, _ = res.LastInsertId()
	affected, _ = res.RowsAffected()
	if s.tx == nil {
		s.e.noteWrite()
	}
	return lastID, affected, nil
}

// Query runs a statement and returns EVERY row, so the cursor is closed before
// the gate is released.
func (s *Session) Query(ctx context.Context, query string, args []any) (cols []string, rows [][]driver.Value, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	release := s.acquire()
	defer release()
	var rs *sql.Rows
	if s.tx != nil {
		rs, err = s.tx.QueryContext(ctx, query, args...)
	} else {
		rs, err = s.conn.QueryContext(ctx, query, args...)
	}
	if err != nil {
		return nil, nil, err
	}
	defer rs.Close()
	cols, err = rs.Columns()
	if err != nil {
		return nil, nil, err
	}
	for rs.Next() {
		// Scanned into *any (NOT *driver.Value: database/sql special-cases
		// the unnamed interface pointer and refuses NULL into the named one),
		// which yields the driver's own type per column.
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		row := make([]driver.Value, len(cols))
		for i, v := range raw {
			// A []byte is copied: the driver may reuse its buffer on Next.
			if b, ok := v.([]byte); ok {
				v = append([]byte(nil), b...)
			}
			row[i] = v
		}
		rows = append(rows, row)
	}
	if err := rs.Err(); err != nil {
		return nil, nil, err
	}
	return cols, rows, nil
}

// Begin opens a transaction and holds the gate until Commit or Rollback.
func (s *Session) Begin(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tx != nil {
		return errTxOpen
	}
	s.e.gate.RLock()
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		s.e.gate.RUnlock()
		return err
	}
	s.tx = tx
	return nil
}

// Commit commits the open transaction and releases the gate.
func (s *Session) Commit(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tx == nil {
		return errNoTx
	}
	err := s.tx.Commit()
	s.tx = nil
	s.e.gate.RUnlock()
	if err == nil {
		s.e.noteWrite()
	}
	return err
}

// Rollback discards the open transaction and releases the gate.
func (s *Session) Rollback(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tx == nil {
		return errNoTx
	}
	err := s.tx.Rollback()
	s.tx = nil
	s.e.gate.RUnlock()
	return err
}

// Ping checks the connection.
func (s *Session) Ping(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	release := s.acquire()
	defer release()
	return s.conn.PingContext(ctx)
}

// Close rolls back any open transaction and returns the connection.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tx != nil {
		_ = s.tx.Rollback()
		s.tx = nil
		s.e.gate.RUnlock()
	}
	return s.conn.Close()
}
