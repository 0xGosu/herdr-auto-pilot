// Package fakeherdr fakes the Herdr surface for tests (Testing Strategy:
// integration against a faked Herdr): an events socket server speaking the
// observed herdr 0.7 protocol — one events.subscribe request per
// connection, {"event": name, "data": {...}} frames, pane.created replay on
// subscribe, per-pane pane.agent_status_changed filters — plus a fake herdr
// CLI script that records invocations and serves canned pane content.
package fakeherdr

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type subscription struct {
	Type   string `json:"type"`
	PaneID string `json:"pane_id"`
}

type clientConn struct {
	conn net.Conn
	subs []subscription
}

// Server is a fake Herdr events socket.
type Server struct {
	SocketPath string

	ln    net.Listener
	mu    sync.Mutex
	conns map[net.Conn]*clientConn
	panes map[string]string // paneID → workspaceID (replayed on subscribe)
}

// NewServer starts a fake events socket in dir.
func NewServer(dir string) (*Server, error) {
	path := filepath.Join(dir, "herdr.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	s := &Server{
		SocketPath: path, ln: ln,
		conns: map[net.Conn]*clientConn{},
		panes: map[string]string{},
	}
	go s.accept()
	return s, nil
}

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serve(conn)
	}
}

func (s *Server) serve(conn net.Conn) {
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		var req struct {
			ID     string `json:"id"`
			Method string `json:"method"`
			Params struct {
				Subscriptions []subscription `json:"subscriptions"`
			} `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		if req.Method != "events.subscribe" {
			continue
		}
		cc := &clientConn{conn: conn, subs: req.Params.Subscriptions}
		s.mu.Lock()
		s.conns[conn] = cc
		// Real herdr replays existing panes to pane.created subscribers.
		var replays [][]byte
		for _, sub := range cc.subs {
			if sub.Type == "pane.created" {
				for paneID, wsID := range s.panes {
					replays = append(replays, wrap("pane_created", map[string]any{
						"type": "pane_created",
						"pane": map[string]any{"pane_id": paneID, "workspace_id": wsID},
					}))
				}
			}
		}
		s.mu.Unlock()

		ack, _ := json.Marshal(map[string]any{
			"id": req.ID, "result": map[string]any{"type": "subscription_started"},
		})
		conn.Write(append(ack, '\n'))
		for _, r := range replays {
			conn.Write(r)
		}
		// The connection is now a pure event stream (mirror real herdr).
	}
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

func wrap(event string, data map[string]any) []byte {
	msg, _ := json.Marshal(map[string]any{"event": event, "data": data})
	return append(msg, '\n')
}

// AddPane registers a pane (replayed to future pane.created subscribers and
// announced to current ones).
func (s *Server) AddPane(paneID, workspaceID string) {
	s.mu.Lock()
	s.panes[paneID] = workspaceID
	s.mu.Unlock()
	s.broadcast("pane.created", map[string]any{
		"type": "pane_created",
		"pane": map[string]any{"pane_id": paneID, "workspace_id": workspaceID},
	}, "")
}

// PushAgentDetected announces an agent label for a pane.
func (s *Server) PushAgentDetected(paneID, workspaceID, agentLabel string) {
	s.mu.Lock()
	if _, ok := s.panes[paneID]; !ok {
		s.panes[paneID] = workspaceID
	}
	s.mu.Unlock()
	s.broadcast("pane.agent_detected", map[string]any{
		"type": "pane_agent_detected", "pane_id": paneID,
		"workspace_id": workspaceID, "agent": agentLabel,
	}, "")
}

// PushTransition pushes a pane.agent_status_changed event to subscribers
// whose pane filter matches.
func (s *Server) PushTransition(paneID, workspaceID, agentLabel, status string) {
	s.broadcast("pane.agent_status_changed", map[string]any{
		"pane_id": paneID, "workspace_id": workspaceID,
		"agent": agentLabel, "agent_status": status,
	}, paneID)
}

// broadcast delivers the event to every connection subscribed to it. For
// pane.agent_status_changed, the subscription's pane filter must match.
func (s *Server) broadcast(eventType string, data map[string]any, paneID string) {
	frame := wrap(eventType, data)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cc := range s.conns {
		for _, sub := range cc.subs {
			if sub.Type != eventType {
				continue
			}
			if eventType == "pane.agent_status_changed" && sub.PaneID != paneID {
				continue
			}
			cc.conn.Write(frame)
			break
		}
	}
}

// DropConnections severs all live subscriber connections (reconnect tests).
func (s *Server) DropConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.conns {
		conn.Close()
	}
	s.conns = map[net.Conn]*clientConn{}
}

// Close shuts the fake server down.
func (s *Server) Close() {
	s.ln.Close()
	s.DropConnections()
}

// FakeCLI is a generated shell script standing in for the herdr binary.
// Every invocation is appended to LogPath; `pane read` outputs the content
// of PaneFile; `agent list` outputs the JSON envelope real herdr prints;
// other commands succeed silently.
type FakeCLI struct {
	BinPath  string
	LogPath  string
	PaneFile string
	FailFlag string // when this file exists, every invocation fails
}

// NewFakeCLI writes the fake herdr script into dir.
func NewFakeCLI(dir string) (*FakeCLI, error) {
	f := &FakeCLI{
		BinPath:  filepath.Join(dir, "fake-herdr"),
		LogPath:  filepath.Join(dir, "herdr-calls.log"),
		PaneFile: filepath.Join(dir, "pane-content.txt"),
		FailFlag: filepath.Join(dir, "fail-flag"),
	}
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
if [ -e %q ]; then
  echo "fake herdr: induced failure" >&2
  exit 1
fi
case "$1 $2" in
  "pane read")
    cat %q 2>/dev/null
    ;;
  "agent list")
    cat %q.agents 2>/dev/null
    ;;
esac
exit 0
`, f.LogPath, f.FailFlag, f.PaneFile, f.PaneFile)
	if err := os.WriteFile(f.BinPath, []byte(script), 0o700); err != nil {
		return nil, err
	}
	return f, nil
}

// SetPaneContent sets what `pane read` returns.
func (f *FakeCLI) SetPaneContent(content string) error {
	return os.WriteFile(f.PaneFile, []byte(content), 0o600)
}

// SetAgentList sets the raw JSON `agent list` returns.
func (f *FakeCLI) SetAgentList(content string) error {
	return os.WriteFile(f.PaneFile+".agents", []byte(content), 0o600)
}

// SetFailing toggles induced CLI failure.
func (f *FakeCLI) SetFailing(fail bool) error {
	if fail {
		return os.WriteFile(f.FailFlag, nil, 0o600)
	}
	err := os.Remove(f.FailFlag)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Calls returns the recorded invocations, one line per call.
func (f *FakeCLI) Calls() []string {
	data, err := os.ReadFile(f.LogPath)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// SentInputs returns the inputs delivered via `agent send`, in order.
func (f *FakeCLI) SentInputs() []string {
	var sent []string
	for _, call := range f.Calls() {
		if rest, ok := strings.CutPrefix(call, "agent send "); ok {
			parts := strings.SplitN(rest, " ", 2)
			if len(parts) == 2 {
				sent = append(sent, parts[1])
			}
		}
	}
	return sent
}

// Notifications returns the titles shown via `notification show`.
func (f *FakeCLI) Notifications() []string {
	var titles []string
	for _, call := range f.Calls() {
		if rest, ok := strings.CutPrefix(call, "notification show "); ok {
			if i := strings.Index(rest, " --body"); i > 0 {
				rest = rest[:i]
			}
			titles = append(titles, rest)
		}
	}
	return titles
}

// ClearLog resets the invocation log.
func (f *FakeCLI) ClearLog() {
	os.Remove(f.LogPath)
}
