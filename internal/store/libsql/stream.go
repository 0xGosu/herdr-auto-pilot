package libsql

import (
	"context"
	"database/sql/driver"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/store/sqlbridge"
)

// stream is one database/sql connection's view of the server: a
// sqlbridge.Backend over Hrana.
//
// Outside a transaction it holds NOTHING on the server — every statement is a
// self-contained [execute, close] pipeline, one round trip, no baton — so an
// idle pooled connection cannot expire. Inside one it holds the stream's
// baton from the first statement to COMMIT/ROLLBACK, and it spends no round
// trip of its own on either end: BEGIN rides with the transaction's first
// statement and the stream's close rides with COMMIT or ROLLBACK, so k
// statements cost k round trips rather than k+2.
type stream struct {
	p Pipeliner
	// baton continues the open stream (inside a transaction only); baseURL
	// is where the server asked that stream to continue ("" = configured).
	baton   *string
	baseURL string
	// inTx: Begin was called; begun: BEGIN has actually reached the server.
	inTx, begun bool
}

var _ sqlbridge.Backend = (*stream)(nil)

// closeTimeout bounds the best-effort close of an abandoned stream.
const closeTimeout = 5 * time.Second

func newStream(p Pipeliner) *stream { return &stream{p: p} }

func (s *stream) Exec(ctx context.Context, query string, args []any) (lastID, affected int64, err error) {
	r, err := s.run(ctx, query, args, false)
	var se *ServerError
	if len(args) == 0 && errors.As(err, &se) && se.Code == codeManyStatements {
		// A DDL batch (the store's schema). Hrana's execute takes exactly one
		// statement; sequence takes many and reports no counters. Asking the
		// server rather than splitting SQL here is exact — a trigger body is
		// one statement full of semicolons — and costs a second round trip
		// only for the batches, which run at migration time.
		seq, err := s.do(ctx, StreamRequest{Type: "sequence", SQL: query})
		if err == nil {
			_, err = resultOf(seq)
		}
		return 0, 0, err
	}
	if err != nil {
		return 0, 0, err
	}
	if r.LastInsertRowid != nil {
		lastID, _ = strconv.ParseInt(*r.LastInsertRowid, 10, 64)
	}
	return lastID, r.AffectedRowCount, nil
}

// codeManyStatements is sqld's error code for an execute carrying more than
// one statement.
const codeManyStatements = "SQL_MANY_STATEMENTS"

func (s *stream) Query(ctx context.Context, query string, args []any) (cols []string, rows [][]driver.Value, err error) {
	r, err := s.run(ctx, query, args, true)
	if err != nil {
		return nil, nil, err
	}
	cols = make([]string, len(r.Cols))
	for i, c := range r.Cols {
		if c.Name != nil {
			cols[i] = *c.Name
		}
	}
	rows = make([][]driver.Value, len(r.Rows))
	for i, raw := range r.Rows {
		row := make([]driver.Value, len(raw))
		for j, v := range raw {
			if row[j], err = DecodeValue(v); err != nil {
				return nil, nil, err
			}
		}
		rows[i] = row
	}
	return cols, rows, nil
}

// run executes one statement.
func (s *stream) run(ctx context.Context, query string, args []any, wantRows bool) (*StmtResult, error) {
	st := &Stmt{SQL: query, WantRows: wantRows}
	for _, a := range args {
		v, err := EncodeValue(a)
		if err != nil {
			return nil, err
		}
		st.Args = append(st.Args, v)
	}
	r, err := s.do(ctx, StreamRequest{Type: "execute", Stmt: st})
	if err != nil {
		return nil, err
	}
	return statementResult(r)
}

// do sends one request on the stream's current footing and returns its
// result: outside a transaction a self-contained [req, close]; on a
// transaction's first statement [BEGIN, req] opening the stream; after that
// [req] on the held baton. A statement error is returned inside the result.
func (s *stream) do(ctx context.Context, req StreamRequest) (StreamResult, error) {
	switch {
	case !s.inTx:
		resp, err := s.send(ctx, nil, []StreamRequest{req, {Type: "close"}})
		if err != nil {
			return StreamResult{}, err
		}
		return resp.Results[0], nil
	case !s.begun:
		resp, err := s.send(ctx, nil, []StreamRequest{
			{Type: "execute", Stmt: &Stmt{SQL: "BEGIN"}}, req,
		})
		if err != nil {
			return StreamResult{}, err
		}
		s.baton, s.begun = resp.Baton, true
		if _, err := resultOf(resp.Results[0]); err != nil {
			return StreamResult{}, err
		}
		return resp.Results[1], nil
	default:
		resp, err := s.send(ctx, s.baton, []StreamRequest{req})
		if err != nil {
			return StreamResult{}, err
		}
		s.baton = resp.Baton
		return resp.Results[0], nil
	}
}

// statementResult is resultOf for an execute, which must carry a result.
func statementResult(r StreamResult) (*StmtResult, error) {
	res, err := resultOf(r)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, errors.New("libsql: malformed response: execute without a result")
	}
	return res, nil
}

// send runs one pipeline on this stream. A failure of the pipeline itself —
// network, HTTP status, a stream the server no longer knows — is a TRANSPORT
// error: the connection is discarded (a transaction on it is gone), where a
// statement error inside a successful pipeline leaves it usable.
func (s *stream) send(ctx context.Context, baton *string, reqs []StreamRequest) (PipelineResponse, error) {
	resp, err := s.p.Pipeline(ctx, s.baseURL, PipelineRequest{Baton: baton, Requests: reqs})
	if err != nil {
		s.baton, s.baseURL = nil, ""
		return resp, sqlbridge.TransportError(err)
	}
	if resp.BaseURL != nil && *resp.BaseURL != "" {
		s.baseURL = *resp.BaseURL
	}
	return resp, nil
}

func (s *stream) Begin(ctx context.Context) error {
	if s.inTx {
		return errors.New("libsql: a transaction is already open on this connection")
	}
	s.inTx, s.begun = true, false
	return nil
}

func (s *stream) Commit(ctx context.Context) error { return s.finish(ctx, "COMMIT") }

func (s *stream) Rollback(ctx context.Context) error { return s.finish(ctx, "ROLLBACK") }

// finish ends the transaction and closes its stream in one round trip. A
// transaction that never ran a statement never reached the server, so there
// is nothing to send.
func (s *stream) finish(ctx context.Context, verb string) error {
	if !s.inTx {
		return errors.New("libsql: no transaction is open on this connection")
	}
	begun, baton := s.begun, s.baton
	s.inTx, s.begun, s.baton = false, false, nil
	defer func() { s.baseURL = "" }()
	if !begun {
		return nil
	}
	resp, err := s.send(ctx, baton, []StreamRequest{
		{Type: "execute", Stmt: &Stmt{SQL: verb}}, {Type: "close"},
	})
	if err != nil {
		return err
	}
	if _, err := resultOf(resp.Results[0]); err != nil {
		// A statement that failed inside the transaction may already have
		// ended it server-side; rolling back nothing is not a failure.
		if verb == "ROLLBACK" && strings.Contains(err.Error(), "no transaction is active") {
			return nil
		}
		return err
	}
	return nil
}

func (s *stream) Ping(ctx context.Context) error {
	_, err := s.run(ctx, "SELECT 1", nil, true)
	return err
}

// Close releases a stream left open (a connection discarded mid-transaction).
// Best effort: the server expires an abandoned stream on its own.
func (s *stream) Close() error {
	if s.baton == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_, _ = s.p.Pipeline(ctx, s.baseURL, PipelineRequest{Baton: s.baton, Requests: []StreamRequest{{Type: "close"}}})
	s.baton, s.inTx, s.begun = nil, false, false
	return nil
}
