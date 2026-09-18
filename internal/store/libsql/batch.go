package libsql

import (
	"context"
	"errors"
	"fmt"
)

// Statement is one SQL statement with its arguments, for Batch and Tx.
type Statement struct {
	SQL  string
	Args []any
}

// Rows is one statement's result, decoded.
type Rows struct {
	Cols     []string
	Rows     [][]any
	Affected int64
}

// Batch runs statements in ONE round trip, outside a transaction, and returns
// each one's decoded result. It is the libsql_replica engine's read path: a
// pull fetches many rows by key, and a round trip per key would cost the far
// servers this engine exists for a second or more per sync.
//
// Statements run in order on one stream; the first statement error is
// returned (the rest of the pipeline has still run — nothing here writes).
func (d *DB) Batch(ctx context.Context, stmts []Statement) ([]Rows, error) {
	reqs, err := executeRequests(stmts)
	if err != nil {
		return nil, err
	}
	reqs = append(reqs, StreamRequest{Type: "close"})
	resp, err := d.p.Pipeline(ctx, "", PipelineRequest{Requests: reqs})
	if err != nil {
		return nil, err
	}
	return decodeResults(resp.Results[:len(stmts)], 0)
}

// Tx runs statements ATOMICALLY in two round trips: BEGIN and every statement
// in the first (holding the stream open), then COMMIT — or ROLLBACK when any
// statement failed — in the second. A Hrana pipeline keeps executing after a
// failed statement, so the verdict has to be read before the commit is sent;
// that is what the second round trip buys.
//
// A transport failure between the two leaves the transaction open on a stream
// the server expires on its own, which rolls it back: nothing half-applies.
func (d *DB) Tx(ctx context.Context, stmts []Statement) ([]Rows, error) {
	reqs, err := executeRequests(stmts)
	if err != nil {
		return nil, err
	}
	reqs = append([]StreamRequest{{Type: "execute", Stmt: &Stmt{SQL: "BEGIN"}}}, reqs...)
	resp, err := d.p.Pipeline(ctx, "", PipelineRequest{Requests: reqs})
	if err != nil {
		return nil, err
	}
	base := ""
	if resp.BaseURL != nil {
		base = *resp.BaseURL
	}
	out, stmtErr := decodeResults(resp.Results, 1)
	verb := "COMMIT"
	if stmtErr != nil {
		verb = "ROLLBACK"
	}
	if resp.Baton == nil {
		return nil, errors.Join(stmtErr, errors.New("libsql: the server closed the transaction's stream"))
	}
	end, err := d.p.Pipeline(ctx, base, PipelineRequest{Baton: resp.Baton, Requests: []StreamRequest{
		{Type: "execute", Stmt: &Stmt{SQL: verb}}, {Type: "close"},
	}})
	if stmtErr != nil {
		return nil, stmtErr
	}
	if err != nil {
		return nil, err
	}
	if _, err := resultOf(end.Results[0]); err != nil {
		return nil, err
	}
	return out, nil
}

func executeRequests(stmts []Statement) ([]StreamRequest, error) {
	reqs := make([]StreamRequest, 0, len(stmts)+2)
	for _, s := range stmts {
		st := &Stmt{SQL: s.SQL, WantRows: true}
		for _, a := range s.Args {
			v, err := EncodeValue(a)
			if err != nil {
				return nil, err
			}
			st.Args = append(st.Args, v)
		}
		reqs = append(reqs, StreamRequest{Type: "execute", Stmt: st})
	}
	return reqs, nil
}

// decodeResults decodes results[skip:] — the statements' — after checking
// results[:skip] (a BEGIN) succeeded.
func decodeResults(results []StreamResult, skip int) ([]Rows, error) {
	for _, r := range results[:skip] {
		if _, err := resultOf(r); err != nil {
			return nil, err
		}
	}
	out := make([]Rows, 0, len(results)-skip)
	for i, r := range results[skip:] {
		res, err := statementResult(r)
		if err != nil {
			return nil, fmt.Errorf("statement %d: %w", i, err)
		}
		rows := Rows{Affected: res.AffectedRowCount}
		for _, c := range res.Cols {
			name := ""
			if c.Name != nil {
				name = *c.Name
			}
			rows.Cols = append(rows.Cols, name)
		}
		for _, raw := range res.Rows {
			row := make([]any, len(raw))
			for j, v := range raw {
				if row[j], err = DecodeValue(v); err != nil {
					return nil, err
				}
			}
			rows.Rows = append(rows.Rows, row)
		}
		out = append(out, rows)
	}
	return out, nil
}
