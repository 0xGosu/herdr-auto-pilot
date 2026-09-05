package sqlbridge

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
)

// backend is what a driver connection talks to: the in-process Session, or a
// remote one over the socket. Both take plain values and return eager rows.
type backend interface {
	Exec(ctx context.Context, query string, args []any) (lastID, affected int64, err error)
	Query(ctx context.Context, query string, args []any) (cols []string, rows [][]driver.Value, err error)
	Begin(ctx context.Context) error
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
	Ping(ctx context.Context) error
	Close() error
}

// freshnessChecker is implemented by a backend whose peer can go away between
// uses (the socket): ResetSession asks it before database/sql reuses a pooled
// connection, and a negative answer makes the pool discard the connection and
// open a new one — so a TUI that outlives a daemon restart, or an idle client
// the server hung up on, recovers on its next statement instead of erroring.
type freshnessChecker interface {
	Fresh(ctx context.Context) bool
}

// Connector is a driver.Connector whose connections run in-process through an
// Executor — the daemon's own path to the gated database.
type Connector struct{ e *Executor }

// NewConnector returns the in-process connector for e.
func NewConnector(e *Executor) *Connector { return &Connector{e: e} }

// Connect opens a Session.
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	s, err := c.e.Session(ctx)
	if err != nil {
		return nil, err
	}
	return &conn{b: s}, nil
}

// Driver implements driver.Connector.
func (c *Connector) Driver() driver.Driver { return bridgeDriver{} }

// OpenGated returns a *sql.DB whose every statement runs through e's gate.
// maxOpen bounds how many of e's underlying connections it may hold at once.
func OpenGated(e *Executor, maxOpen int) *sql.DB {
	db := sql.OpenDB(NewConnector(e))
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	return db
}

// bridgeDriver only exists to satisfy driver.Connector.Driver; connections are
// always made through a Connector.
type bridgeDriver struct{}

func (bridgeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("sqlbridge: open through a Connector, not a DSN")
}

// conn adapts a backend to database/sql's driver interfaces.
type conn struct {
	b   backend
	bad bool
}

var (
	_ driver.Conn              = (*conn)(nil)
	_ driver.ConnBeginTx       = (*conn)(nil)
	_ driver.ExecerContext     = (*conn)(nil)
	_ driver.QueryerContext    = (*conn)(nil)
	_ driver.Pinger            = (*conn)(nil)
	_ driver.Validator         = (*conn)(nil)
	_ driver.SessionResetter   = (*conn)(nil)
	_ driver.NamedValueChecker = (*conn)(nil)
	_ driver.StmtExecContext   = (*stmt)(nil)
	_ driver.StmtQueryContext  = (*stmt)(nil)
)

// Prepare returns a statement that re-sends the query text on every use; the
// bridge has no server-side prepared statements.
func (c *conn) Prepare(query string) (driver.Stmt, error) { return &stmt{c: c, query: query}, nil }

func (c *conn) Close() error { return c.b.Close() }

func (c *conn) Begin() (driver.Tx, error) { return c.BeginTx(context.Background(), driver.TxOptions{}) }

func (c *conn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if err := c.b.Begin(ctx); err != nil {
		return nil, c.wrap(err)
	}
	return &tx{c: c}, nil
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	lastID, affected, err := c.b.Exec(ctx, query, values(args))
	if err != nil {
		return nil, c.wrap(err)
	}
	return result{lastID: lastID, affected: affected}, nil
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	cols, data, err := c.b.Query(ctx, query, values(args))
	if err != nil {
		return nil, c.wrap(err)
	}
	return &rows{cols: cols, data: data}, nil
}

func (c *conn) Ping(ctx context.Context) error { return c.wrap(c.b.Ping(ctx)) }

// IsValid lets database/sql discard a connection whose transport failed.
func (c *conn) IsValid() bool { return !c.bad }

func (c *conn) ResetSession(ctx context.Context) error {
	if c.bad {
		return driver.ErrBadConn
	}
	if f, ok := c.b.(freshnessChecker); ok && !f.Fresh(ctx) {
		c.bad = true
		return driver.ErrBadConn
	}
	return nil
}

// CheckNamedValue accepts what the wire can carry, converting the rest the way
// database/sql would have.
func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	v, err := driver.DefaultParameterConverter.ConvertValue(nv.Value)
	if err != nil {
		return err
	}
	nv.Value = v
	return nil
}

// wrap marks the connection bad on a TRANSPORT error — a statement error (a
// constraint, a missing table), whether local or relayed from the daemon as an
// *Error, leaves the connection perfectly usable.
func (c *conn) wrap(err error) error {
	if err == nil {
		return nil
	}
	var t *transportError
	if errors.As(err, &t) {
		c.bad = true
	}
	return err
}

// transportError is a socket failure: the connection cannot be reused.
type transportError struct{ err error }

func (t *transportError) Error() string { return "sqlbridge: " + t.err.Error() }
func (t *transportError) Unwrap() error { return t.err }

func values(args []driver.NamedValue) []any {
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out
}

type tx struct{ c *conn }

func (t *tx) Commit() error   { return t.c.wrap(t.c.b.Commit(context.Background())) }
func (t *tx) Rollback() error { return t.c.wrap(t.c.b.Rollback(context.Background())) }

type stmt struct {
	c     *conn
	query string
}

func (s *stmt) Close() error  { return nil }
func (s *stmt) NumInput() int { return -1 }
func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.c.ExecContext(context.Background(), s.query, named(args))
}
func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.c.QueryContext(context.Background(), s.query, named(args))
}
func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.c.ExecContext(ctx, s.query, args)
}
func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.c.QueryContext(ctx, s.query, args)
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, a := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return out
}

type result struct{ lastID, affected int64 }

func (r result) LastInsertId() (int64, error) { return r.lastID, nil }
func (r result) RowsAffected() (int64, error) { return r.affected, nil }

// rows is an eager result set.
type rows struct {
	cols []string
	data [][]driver.Value
	i    int
}

func (r *rows) Columns() []string { return r.cols }
func (r *rows) Close() error      { r.data = nil; return nil }
func (r *rows) Next(dest []driver.Value) error {
	if r.i >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.i])
	r.i++
	return nil
}
