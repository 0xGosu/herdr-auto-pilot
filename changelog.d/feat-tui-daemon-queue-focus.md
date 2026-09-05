- Changed focusing an agent (`f` on the Agents, Escalations and Tasks tabs) to go
  through the daemon instead of the TUI driving herdr itself. It now needs a
  running daemon and reports `no healthy hap daemon is running` without one. The
  keypress no longer waits for herdr, so it now says it ASKED rather than
  claiming the pane moved, and a focus that never happens is reported in the
  daemon log. A request the daemon could not act on within two minutes — a
  daemon that died before draining it — is refused rather than replayed into
  whatever pane you had moved on to
- Added a build-time guard that stops the TUI and CLI from reaching herdr
  directly, per call site rather than per file. Each one that still does is
  listed with the reason and the migration stage that removes it, and an entry
  that is no longer needed now fails the build, so the list can only shrink
