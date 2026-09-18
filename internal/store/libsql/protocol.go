package libsql

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// This file is the Hrana v2 protocol as JSON types — the HTTP transport lives
// in hrana.go alone, so the fake server (hranafake) and every test can speak
// the protocol without importing net/http.
//
// Reference: libsql's docs/HRANA_2_SPEC.md and HTTP_V2_SPEC.md. A pipeline
// request carries an optional baton (the stream it continues) and a list of
// stream requests; the response carries the next baton (null once the stream
// is closed), an optional base_url the stream must continue on, and one
// result per request.

// PipelineRequest is the body of POST /v2/pipeline.
type PipelineRequest struct {
	Baton    *string         `json:"baton"`
	Requests []StreamRequest `json:"requests"`
}

// StreamRequest is one request inside a pipeline: "execute" (Stmt),
// "sequence" (SQL: several statements, no arguments, no result) or "close".
type StreamRequest struct {
	Type string `json:"type"`
	Stmt *Stmt  `json:"stmt,omitempty"`
	SQL  string `json:"sql,omitempty"`
}

// Stmt is one SQL statement with positional arguments.
type Stmt struct {
	SQL      string  `json:"sql"`
	Args     []Value `json:"args,omitempty"`
	WantRows bool    `json:"want_rows"`
}

// PipelineResponse is the body of a successful pipeline call.
type PipelineResponse struct {
	Baton   *string        `json:"baton"`
	BaseURL *string        `json:"base_url"`
	Results []StreamResult `json:"results"`
}

// StreamResult is one request's outcome: "ok" with a response, or "error".
type StreamResult struct {
	Type     string          `json:"type"`
	Response *StreamResponse `json:"response,omitempty"`
	Error    *ProtocolError  `json:"error,omitempty"`
}

// StreamResponse is an "ok" result's payload; Result is set for "execute".
type StreamResponse struct {
	Type   string      `json:"type"`
	Result *StmtResult `json:"result,omitempty"`
}

// StmtResult is an executed statement's rows and counters.
type StmtResult struct {
	Cols             []Col     `json:"cols"`
	Rows             [][]Value `json:"rows"`
	AffectedRowCount int64     `json:"affected_row_count"`
	// LastInsertRowid is a decimal string, or null.
	LastInsertRowid *string `json:"last_insert_rowid"`
	// ReplicationIndex is the server's position after the statement (sqld:
	// the primary's frame number), a decimal string, or null when the server
	// does not report one. It is the libsql engine's cheap change token.
	ReplicationIndex *string `json:"replication_index,omitempty"`
}

// Col names one result column.
type Col struct {
	Name     *string `json:"name"`
	Decltype *string `json:"decltype,omitempty"`
}

// ProtocolError is an error the server reported for one request.
type ProtocolError struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

// Value is one Hrana value. Integers travel as DECIMAL STRINGS, and must be
// parsed as such: routed through float64 they lose everything past 2^53, which
// is exactly where the store's node-scoped ids live (store.TimeOrderedIDs).
type Value struct {
	Type   string          `json:"type"`
	Value  json.RawMessage `json:"value,omitempty"`
	Base64 string          `json:"base64,omitempty"`
}

// timeLayout is how a time.Time argument is stored: modernc's own text form,
// so a value written under libsql reads back as it would under sqlite.
const timeLayout = "2006-01-02 15:04:05.999999999-07:00"

// EncodeValue converts a database/sql argument (already through
// DefaultParameterConverter) into a Hrana value.
func EncodeValue(v any) (Value, error) {
	switch x := v.(type) {
	case nil:
		return Value{Type: "null"}, nil
	case int64:
		return Value{Type: "integer", Value: quote(strconv.FormatInt(x, 10))}, nil
	case bool:
		n := "0"
		if x {
			n = "1"
		}
		return Value{Type: "integer", Value: quote(n)}, nil
	case float64:
		b, err := json.Marshal(x)
		if err != nil {
			return Value{}, fmt.Errorf("libsql: encode %v: %w", x, err)
		}
		return Value{Type: "float", Value: b}, nil
	case string:
		return Value{Type: "text", Value: quote(x)}, nil
	case []byte:
		return Value{Type: "blob", Base64: base64.StdEncoding.EncodeToString(x)}, nil
	case time.Time:
		return Value{Type: "text", Value: quote(x.Format(timeLayout))}, nil
	default:
		return Value{}, fmt.Errorf("libsql: cannot encode a %T", v)
	}
}

// DecodeValue converts a Hrana value into the Go type modernc would have
// returned for the same column: int64, float64, string, []byte or nil.
func DecodeValue(v Value) (any, error) {
	switch v.Type {
	case "null", "":
		return nil, nil
	case "integer":
		var s string
		if err := json.Unmarshal(v.Value, &s); err != nil {
			// Tolerate a bare JSON number from a lax server — but only one
			// that is an exact integer literal, parsed without float64.
			return strconv.ParseInt(string(v.Value), 10, 64)
		}
		return strconv.ParseInt(s, 10, 64)
	case "float":
		var f float64
		if err := json.Unmarshal(v.Value, &f); err != nil {
			return nil, fmt.Errorf("libsql: decode float: %w", err)
		}
		return f, nil
	case "text":
		var s string
		if err := json.Unmarshal(v.Value, &s); err != nil {
			return nil, fmt.Errorf("libsql: decode text: %w", err)
		}
		return s, nil
	case "blob":
		b, err := base64.StdEncoding.DecodeString(v.Base64)
		if err != nil {
			// The spec allows unpadded base64.
			b, err = base64.RawStdEncoding.DecodeString(v.Base64)
		}
		if err != nil {
			return nil, fmt.Errorf("libsql: decode blob: %w", err)
		}
		if b == nil {
			b = []byte{}
		}
		return b, nil
	default:
		return nil, fmt.Errorf("libsql: unknown value type %q", v.Type)
	}
}

func quote(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// ServerError is a statement error the SERVER reported — a constraint, a
// missing table, SQLITE_BUSY. It leaves the stream usable. Its text always
// starts with domain.LibSQLServerErrorPrefix: sqld relays SQLite's wording
// verbatim, and "database is locked" is a PROCESS-LOCAL shape to the fleet
// recovery (domain.SyncFailureProcessLocal), which must never restart the
// daemon over a busy server.
type ServerError struct {
	Message string
	Code    string
}

func (e *ServerError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s (%s)", domain.LibSQLServerErrorPrefix, e.Message, e.Code)
	}
	return domain.LibSQLServerErrorPrefix + ": " + e.Message
}

// ErrUnauthorized reports that the server refused the token (HTTP 401/403).
var ErrUnauthorized = errors.New("libsql: the server rejected the auth token (unauthorized)")

// ErrNotHrana reports a server that does not answer Hrana over HTTP at
// /v2/pipeline — the URL is not a libsql server, or not its HTTP endpoint.
var ErrNotHrana = errors.New("libsql: the server does not serve Hrana over HTTP (/v2/pipeline not found)")

// StreamClosedError reports that the server no longer knows the stream a
// baton named (it expired or the server restarted). The transaction it held
// is gone.
type StreamClosedError struct{ Detail string }

func (e *StreamClosedError) Error() string {
	return "libsql: the server closed the stream: " + e.Detail
}

// resultOf turns one StreamResult into the statement result or its error.
func resultOf(r StreamResult) (*StmtResult, error) {
	switch r.Type {
	case "ok":
		if r.Response == nil {
			return nil, errors.New("libsql: malformed response: ok without a response")
		}
		return r.Response.Result, nil
	case "error":
		if r.Error == nil {
			return nil, &ServerError{Message: "unspecified error"}
		}
		return nil, &ServerError{Message: r.Error.Message, Code: r.Error.Code}
	default:
		return nil, fmt.Errorf("libsql: malformed response: result type %q", r.Type)
	}
}

// parseIndex parses a replication index; ok is false when there is none.
func parseIndex(s *string) (uint64, bool) {
	if s == nil || *s == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(*s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
