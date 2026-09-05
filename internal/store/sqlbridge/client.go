package sqlbridge

import (
	"bufio"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// DialConnector is a driver.Connector whose connections speak to a Server over
// the unix socket at path — a front end's path to the daemon's database.
//
// It dials LAZILY, per connection: sql.OpenDB never touches the socket, so a
// process that opens the store and then only reads config keeps working with
// no daemon running.
type DialConnector struct {
	Path string
	// Dial overrides the dialer (tests). nil dials the unix socket at Path.
	Dial func(ctx context.Context) (net.Conn, error)
}

// OpenRemote returns a *sql.DB served by the daemon at path.
func OpenRemote(path string) *sql.DB {
	db := sql.OpenDB(&DialConnector{Path: path})
	// Two, like the local store: a session is a daemon connection held for as
	// long as this one lives.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(time.Minute)
	return db
}

// Connect dials the socket. A refused or missing socket is ErrStoreUnavailable,
// returned as-is: database/sql hands a connector's error straight to the
// caller, so `hap` prints the sentence rather than "driver: bad connection".
func (d *DialConnector) Connect(ctx context.Context) (driver.Conn, error) {
	var c net.Conn
	var err error
	if d.Dial != nil {
		c, err = d.Dial(ctx)
	} else {
		var dialer net.Dialer
		c, err = dialer.DialContext(ctx, "unix", d.Path)
	}
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrStoreUnavailable, err)
	}
	return &conn{b: &remoteSession{c: c, r: bufio.NewReaderSize(c, 64<<10), lastUsed: time.Now()}}, nil
}

// Driver implements driver.Connector.
func (d *DialConnector) Driver() driver.Driver { return bridgeDriver{} }

// remoteSession is one socket connection: one request in flight at a time.
type remoteSession struct {
	c    net.Conn
	r    *bufio.Reader
	mu   sync.Mutex
	next uint64
	// lastUsed drives Fresh: a connection reused within freshnessGrace is
	// trusted without a round trip, so a TUI refresh's burst of queries pays
	// one ping at most.
	lastUsed time.Time
}

// freshnessGrace is how long a pooled connection is trusted without a ping.
const freshnessGrace = time.Second

// Fresh pings the daemon when the connection has sat idle long enough that
// the server may have hung up (its idle timeout) or restarted.
func (s *remoteSession) Fresh(ctx context.Context) bool {
	s.mu.Lock()
	idle := time.Since(s.lastUsed)
	s.mu.Unlock()
	if idle < freshnessGrace {
		return true
	}
	return s.Ping(ctx) == nil
}

func (s *remoteSession) roundTrip(ctx context.Context, req request) (response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	req.ID = s.next
	// Cancellation kills the socket: there is no way to abandon one request
	// and keep the stream in step. database/sql discards the connection.
	stop := context.AfterFunc(ctx, func() { _ = s.c.SetDeadline(time.Now()) })
	defer stop()
	if err := json.NewEncoder(s.c).Encode(req); err != nil {
		return response{}, s.transport(ctx, err)
	}
	var resp response
	if err := json.NewDecoder(s.r).Decode(&resp); err != nil {
		return response{}, s.transport(ctx, err)
	}
	if resp.ID != req.ID {
		return response{}, &transportError{err: errors.New("response out of step with request")}
	}
	s.lastUsed = time.Now()
	if resp.Kind == kindErr {
		return response{}, &Error{Msg: resp.Message}
	}
	return resp, nil
}

func (s *remoteSession) transport(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return &transportError{err: ctx.Err()}
	}
	return &transportError{err: err}
}

func (s *remoteSession) Exec(ctx context.Context, query string, args []any) (int64, int64, error) {
	wargs, err := encodeValues(args)
	if err != nil {
		return 0, 0, err
	}
	resp, err := s.roundTrip(ctx, request{Kind: kindExec, SQL: query, Args: wargs})
	if err != nil {
		return 0, 0, err
	}
	lastID, _ := strconv.ParseInt(resp.LastID, 10, 64)
	affected, _ := strconv.ParseInt(resp.Affected, 10, 64)
	return lastID, affected, nil
}

func (s *remoteSession) Query(ctx context.Context, query string, args []any) ([]string, [][]driver.Value, error) {
	wargs, err := encodeValues(args)
	if err != nil {
		return nil, nil, err
	}
	resp, err := s.roundTrip(ctx, request{Kind: kindQuery, SQL: query, Args: wargs})
	if err != nil {
		return nil, nil, err
	}
	rows := make([][]driver.Value, len(resp.Rows))
	for i, r := range resp.Rows {
		vals, err := decodeValues(r)
		if err != nil {
			return nil, nil, &transportError{err: err}
		}
		rows[i] = vals
	}
	return resp.Columns, rows, nil
}

func (s *remoteSession) Begin(ctx context.Context) error {
	_, err := s.roundTrip(ctx, request{Kind: kindBegin})
	return err
}

func (s *remoteSession) Commit(ctx context.Context) error {
	_, err := s.roundTrip(ctx, request{Kind: kindCommit})
	return err
}

func (s *remoteSession) Rollback(ctx context.Context) error {
	_, err := s.roundTrip(ctx, request{Kind: kindRollback})
	return err
}

func (s *remoteSession) Ping(ctx context.Context) error {
	_, err := s.roundTrip(ctx, request{Kind: kindPing})
	return err
}

func (s *remoteSession) Close() error { return s.c.Close() }
