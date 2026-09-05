- Changed focusing an agent (`f` on the Agents, Escalations and Tasks tabs) to go
  through the daemon instead of the TUI driving herdr itself. It now needs a
  running daemon and reports `no healthy hap daemon is running` without one. The
  keypress no longer waits for herdr, so it now says it ASKED rather than
  claiming the pane moved, and a focus that never happens is reported in the
  daemon log. A request the daemon could not act on within two minutes — a
  daemon that died before draining it — is refused rather than replayed into
  whatever pane you had moved on to
- Changed `hap capture` the same way. It now TELLS YOU when it cannot do what
  you asked: naming an agent that does not exist, or catching one mid-work, used
  to print `capture queued` and then silently do nothing, because the request
  went out as a fire-and-forget signal with no way to answer back. Both now fail
  with the reason. It also refuses a daemon that holds its lock but has stopped
  making progress, or whose binary was replaced underneath it — cases the old
  version check reported as running
- Changed every surface that shows the herd — the TUI's agent list, `hap status`,
  `hap agents`, and anything resolving an agent by name — to read what the daemon
  publishes instead of asking herdr itself. One process now asks, however many
  windows you have open, and a working directory is read once for everyone rather
  than once per agent per window. The daemon refreshes faster while a TUI is
  actually open and otherwise only when something changes, so an install nobody
  is watching does no polling at all. One consequence is worth knowing: an agent
  that DISAPPEARS produces no event, so with no TUI open a pane that has closed
  can still be listed by `hap agents` and `hap status` for up to a minute, until
  the next sweep reconciles the herd
- Fixed a stale report being treated as live agents. When the last report is too
  old to trust, the herd now reads as UNKNOWN rather than handing every surface
  a list of agents to act on — a pane id from an old report may since have been
  recycled onto a different process. The surfaces say how old the report is
- Fixed the herd reading as EMPTY when nothing had looked at it. "No agents are
  running" and "no daemon has reported yet" are different answers, and surfaces
  that act on an agent's absence — retiring a task source, confirming an agent is
  idle — now refuse rather than guess when the report is missing or too old
- Removed the last daemon signal that carried data. A manual capture used to
  smuggle its target into the signal itself, which is why it had no way to
  report a result; signals are now purely "there is new work in the database",
  as they were always meant to be
- Added a build-time guard that stops the TUI and CLI from reaching herdr
  directly, per call site rather than per file. Each one that still does is
  listed with the reason and the migration stage that removes it, and an entry
  that is no longer needed now fails the build, so the list can only shrink
