// Package hranafake is an in-process libsql server for tests: it answers
// Hrana pipelines (libsql.Pipeliner) against a local SQLite file, so the
// libsql engine — and the whole store suite on top of it — runs with no
// network and no sqld.
//
// It speaks the protocol at the pipeline level, not over HTTP: this is a
// regular package the store suite imports, and internal/privacy allows no
// net/http outside the egress allowlist. The adapter's own tests add the HTTP
// layer with httptest.
//
// It mirrors the behaviours of sqld 0.24 the engine depends on (verified
// live): one statement per execute (SQL_MANY_STATEMENTS otherwise), a
// sequence request for batches, streams continued by baton, and a
// replication index that advances on every write and never on a read.
package hranafake

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsql"

	_ "modernc.org/sqlite"
)

// Server is one fake libsql server over one SQLite file.
type Server struct {
	db *sql.DB

	mu      sync.Mutex
	streams map[string]*sql.Conn
	next    int
	index   uint64

	down     atomic.Bool
	noIndex  atomic.Bool
	pipeline atomic.Int64
}

var _ libsql.Pipeliner = (*Server)(nil)

// New opens (creating) the SQLite file at path as the server's database.
func New(path string) (*Server, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	return &Server{db: db, streams: map[string]*sql.Conn{}}, nil
}

// SetDown makes every pipeline fail as an unreachable server would.
func (s *Server) SetDown(down bool) { s.down.Store(down) }

// SetReportIndex false makes results carry no replication index, as the
// protocol allows.
func (s *Server) SetReportIndex(on bool) { s.noIndex.Store(!on) }

// Pipelines counts the pipelines answered — the round trips a client paid.
func (s *Server) Pipelines() int64 { return s.pipeline.Load() }

// OpenStreams counts streams held open by a baton.
func (s *Server) OpenStreams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

// ExpireStreams drops every open stream, rolling its transaction back, as
// sqld does to a stream left idle past its timeout (~10s, STREAM_EXPIRED).
func (s *Server) ExpireStreams() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, c := range s.streams {
		_, _ = c.ExecContext(context.Background(), "ROLLBACK")
		c.Close()
		delete(s.streams, k)
	}
}

// Close closes every stream and the database.
func (s *Server) Close() error {
	s.mu.Lock()
	for k, c := range s.streams {
		c.Close()
		delete(s.streams, k)
	}
	s.mu.Unlock()
	return s.db.Close()
}

// errUnreachable is what a pipeline returns while the server is down; the
// wording is the network shape the fleet recovery reads as remote.
var errUnreachable = errors.New("libsql: dial tcp 127.0.0.1:1: connect: connection refused")

// Pipeline implements libsql.Pipeliner.
func (s *Server) Pipeline(ctx context.Context, _ string, req libsql.PipelineRequest) (libsql.PipelineResponse, error) {
	if s.down.Load() {
		return libsql.PipelineResponse{}, errUnreachable
	}
	s.pipeline.Add(1)
	var conn *sql.Conn
	if req.Baton != nil {
		s.mu.Lock()
		conn = s.streams[*req.Baton]
		delete(s.streams, *req.Baton)
		s.mu.Unlock()
		if conn == nil {
			return libsql.PipelineResponse{}, &libsql.StreamClosedError{Detail: "HTTP 400 bad request: stream not found"}
		}
	} else {
		var err error
		if conn, err = s.db.Conn(ctx); err != nil {
			return libsql.PipelineResponse{}, fmt.Errorf("libsql: HTTP 500 internal server error: %v", err)
		}
	}
	out := libsql.PipelineResponse{Results: make([]libsql.StreamResult, 0, len(req.Requests))}
	closed := false
	for _, r := range req.Requests {
		if closed {
			out.Results = append(out.Results, errResult("the stream is closed", "STREAM_CLOSED"))
			continue
		}
		switch r.Type {
		case "execute":
			out.Results = append(out.Results, s.execute(ctx, conn, r.Stmt))
		case "sequence":
			out.Results = append(out.Results, s.sequence(ctx, conn, r.SQL))
		case "close":
			closed = true
			out.Results = append(out.Results, libsql.StreamResult{Type: "ok",
				Response: &libsql.StreamResponse{Type: "close"}})
		default:
			out.Results = append(out.Results, errResult("unknown request "+r.Type, "INVALID"))
		}
	}
	if closed {
		// As sqld does, an open transaction dies with its stream.
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		conn.Close()
		return out, nil
	}
	s.mu.Lock()
	s.next++
	baton := "b" + strconv.Itoa(s.next)
	s.streams[baton] = conn
	s.mu.Unlock()
	out.Baton = &baton
	return out, nil
}

func errResult(msg, code string) libsql.StreamResult {
	return libsql.StreamResult{Type: "error", Error: &libsql.ProtocolError{Message: msg, Code: code}}
}

// counters reads what a statement did on conn.
func counters(ctx context.Context, conn *sql.Conn) (total, lastID int64) {
	_ = conn.QueryRowContext(ctx, "SELECT total_changes(), last_insert_rowid()").Scan(&total, &lastID)
	return total, lastID
}

// ddlPrefixes are statement kinds that write without counting in
// total_changes(): the index must still advance for them.
var ddlPrefixes = []string{"CREATE", "DROP", "ALTER"}

func (s *Server) noteWrite(stmt string, before, after int64) {
	up := strings.ToUpper(strings.TrimSpace(stmt))
	isDDL := false
	for _, p := range ddlPrefixes {
		if strings.HasPrefix(up, p) {
			isDDL = true
		}
	}
	if after != before || isDDL {
		s.mu.Lock()
		s.index++
		s.mu.Unlock()
	}
}

func (s *Server) indexString() *string {
	if s.noIndex.Load() {
		return nil
	}
	s.mu.Lock()
	v := strconv.FormatUint(s.index, 10)
	s.mu.Unlock()
	return &v
}

func (s *Server) execute(ctx context.Context, conn *sql.Conn, st *libsql.Stmt) libsql.StreamResult {
	if st == nil {
		return errResult("execute without a statement", "INVALID")
	}
	if manyStatements(st.SQL) {
		return errResult("SQL string contains more than one statement", "SQL_MANY_STATEMENTS")
	}
	args := make([]any, len(st.Args))
	for i, v := range st.Args {
		a, err := libsql.DecodeValue(v)
		if err != nil {
			return errResult(err.Error(), "INVALID_ARGS")
		}
		args[i] = a
	}
	before, _ := counters(ctx, conn)
	rows, err := conn.QueryContext(ctx, st.SQL, args...)
	if err != nil {
		return errResult(err.Error(), "SQLITE_ERROR")
	}
	cols, _ := rows.ColumnTypes()
	res := &libsql.StmtResult{Cols: make([]libsql.Col, len(cols)), Rows: [][]libsql.Value{}}
	for i, c := range cols {
		name := c.Name()
		res.Cols[i] = libsql.Col{Name: &name}
	}
	for rows.Next() {
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return errResult(err.Error(), "SQLITE_ERROR")
		}
		row := make([]libsql.Value, len(raw))
		for i, v := range raw {
			if row[i], err = libsql.EncodeValue(v); err != nil {
				rows.Close()
				return errResult(err.Error(), "SQLITE_ERROR")
			}
		}
		res.Rows = append(res.Rows, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return errResult(err.Error(), "SQLITE_ERROR")
	}
	after, lastID := counters(ctx, conn)
	res.AffectedRowCount = after - before
	if res.AffectedRowCount > 0 {
		id := strconv.FormatInt(lastID, 10)
		res.LastInsertRowid = &id
	}
	s.noteWrite(st.SQL, before, after)
	res.ReplicationIndex = s.indexString()
	return libsql.StreamResult{Type: "ok", Response: &libsql.StreamResponse{Type: "execute", Result: res}}
}

func (s *Server) sequence(ctx context.Context, conn *sql.Conn, sqlText string) libsql.StreamResult {
	before, _ := counters(ctx, conn)
	if _, err := conn.ExecContext(ctx, sqlText); err != nil {
		return errResult(err.Error(), "SQLITE_ERROR")
	}
	after, _ := counters(ctx, conn)
	s.noteWrite(sqlText, before, after)
	return libsql.StreamResult{Type: "ok", Response: &libsql.StreamResponse{Type: "sequence"}}
}

// manyStatements approximates sqld's single-statement check closely enough
// for the store's SQL: text after the first top-level semicolon that is not
// whitespace, ignoring semicolons inside quotes and inside a trigger body.
func manyStatements(q string) bool {
	up := strings.ToUpper(q)
	if strings.Contains(up, "CREATE TRIGGER") {
		// A trigger's BEGIN … END holds semicolons of its own; count only
		// what follows its final END.
		if i := strings.LastIndex(up, "END"); i >= 0 {
			return strings.TrimSpace(strings.TrimLeft(q[i+3:], "; \t\r\n")) != ""
		}
	}
	inQuote := rune(0)
	for i, r := range q {
		switch {
		case inQuote != 0:
			if r == inQuote {
				inQuote = 0
			}
		case r == '\'' || r == '"' || r == '`':
			inQuote = r
		case r == ';':
			return strings.TrimSpace(strings.TrimLeft(q[i:], "; \t\r\n")) != ""
		}
	}
	return false
}
