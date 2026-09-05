package sqlbridge

import (
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// The wire: newline-delimited JSON, one request in flight per connection.
//
//	→ {"id":N,"k":"exec"|"query","q":"<sql>","a":[<value>…]}
//	→ {"id":N,"k":"begin"|"commit"|"rollback"|"ping"}
//	→ {"id":N,"k":"nextid"}                      ← {"id":N,"k":"ok","li":"<int64>"}
//	← {"id":N,"k":"ok","li":"<int64>","ra":"<int64>"}
//	← {"id":N,"k":"rows","c":["col"…],"r":[[<value>…]…]}
//	← {"id":N,"k":"err","m":"<message>"}
//
// A value is {"t":<tag>,"v":…}: n (null), i (int64 as a DECIMAL STRING — the
// store's ids are 63-bit and JSON numbers are float64), f, s, b (base64
// bytes), o (bool), t (RFC3339Nano). The TEXT/BLOB distinction survives the
// trip: a BLOB column (an embedding vector) comes back as []byte, a TEXT one
// as string.

type request struct {
	ID   uint64      `json:"id"`
	Kind string      `json:"k"`
	SQL  string      `json:"q,omitempty"`
	Args []wireValue `json:"a,omitempty"`
}

type response struct {
	ID       uint64        `json:"id"`
	Kind     string        `json:"k"`
	LastID   string        `json:"li,omitempty"`
	Affected string        `json:"ra,omitempty"`
	Columns  []string      `json:"c,omitempty"`
	Rows     [][]wireValue `json:"r,omitempty"`
	Message  string        `json:"m,omitempty"`
}

const (
	kindExec     = "exec"
	kindQuery    = "query"
	kindBegin    = "begin"
	kindCommit   = "commit"
	kindRollback = "rollback"
	kindPing     = "ping"
	// kindNextID asks the daemon for the next INTEGER PRIMARY KEY. Under the
	// turso engine ids carry node bits and a per-node sequence, and every
	// process on the node — the TUI, a CLI verb, the MCP server — inserts
	// rows too; allocating in ONE place (the daemon) is what keeps two
	// processes on one machine from minting the same id in the same
	// millisecond. The id rides in "li".
	kindNextID = "nextid"
	kindOK     = "ok"
	kindRows   = "rows"
	kindErr    = "err"
)

type wireValue struct {
	T string          `json:"t"`
	V json.RawMessage `json:"v,omitempty"`
}

// Error is a statement error relayed from the daemon. Its text is the daemon's
// own, so `hap` prints exactly what a local store would have said.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func encodeValue(v any) (wireValue, error) {
	switch x := v.(type) {
	case nil:
		return wireValue{T: "n"}, nil
	case int64:
		return wireValue{T: "i", V: jsonString(strconv.FormatInt(x, 10))}, nil
	case float64:
		b, err := json.Marshal(x)
		return wireValue{T: "f", V: b}, err
	case bool:
		b, _ := json.Marshal(x)
		return wireValue{T: "o", V: b}, nil
	case []byte:
		return wireValue{T: "b", V: jsonString(base64.StdEncoding.EncodeToString(x))}, nil
	case string:
		return wireValue{T: "s", V: jsonString(x)}, nil
	case time.Time:
		return wireValue{T: "t", V: jsonString(x.Format(time.RFC3339Nano))}, nil
	default:
		// database/sql has already run DefaultParameterConverter, so anything
		// else is a driver-specific type we cannot carry.
		return wireValue{}, fmt.Errorf("sqlbridge: cannot encode a %T", v)
	}
}

func decodeValue(w wireValue) (driver.Value, error) {
	switch w.T {
	case "n", "":
		return nil, nil
	case "i":
		var s string
		if err := json.Unmarshal(w.V, &s); err != nil {
			return nil, err
		}
		return strconv.ParseInt(s, 10, 64)
	case "f":
		var f float64
		err := json.Unmarshal(w.V, &f)
		return f, err
	case "o":
		var b bool
		err := json.Unmarshal(w.V, &b)
		return b, err
	case "b":
		var s string
		if err := json.Unmarshal(w.V, &s); err != nil {
			return nil, err
		}
		return base64.StdEncoding.DecodeString(s)
	case "s":
		var s string
		err := json.Unmarshal(w.V, &s)
		return s, err
	case "t":
		var s string
		if err := json.Unmarshal(w.V, &s); err != nil {
			return nil, err
		}
		return time.Parse(time.RFC3339Nano, s)
	default:
		return nil, fmt.Errorf("sqlbridge: unknown value tag %q", w.T)
	}
}

func encodeValues(vs []any) ([]wireValue, error) {
	out := make([]wireValue, len(vs))
	for i, v := range vs {
		w, err := encodeValue(v)
		if err != nil {
			return nil, err
		}
		out[i] = w
	}
	return out, nil
}

func decodeValues(ws []wireValue) ([]driver.Value, error) {
	out := make([]driver.Value, len(ws))
	for i, w := range ws {
		v, err := decodeValue(w)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// ErrStoreUnavailable is returned when the daemon's store socket cannot be
// reached: under the turso engine every other hap process depends on it.
var ErrStoreUnavailable = errors.New("the store is served by the hap daemon under database.engine = \"turso\" " +
	"and the daemon is not reachable; start it with `hap daemon --ensure`")
