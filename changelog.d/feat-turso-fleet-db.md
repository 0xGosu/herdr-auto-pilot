- Added an opt-in central database: `[database] engine = "turso"` keeps the whole store in a
  Turso sync database the daemon syncs with the Turso Cloud database you own, so several of
  your machines share one hap — every machine's agents, escalations, audit, learned rules and
  pause state in one TUI, and an escalation raised on one machine can be confirmed, answered
  or dismissed from another. The default stays `sqlite`, a local file, with no outbound call.
- Added a per-machine node id (`<state>/node-id`) to every row a machine owns, so identical
  herdr pane ids on two machines never collide, and the daemon only ever acts on its own
  agents' rows — including at startup, where one machine's restart no longer reclaims another
  machine's in-flight work.
- Changed `hap status` to report the shared database's sync state (`fleet sync:`) under the
  turso engine, and `hap config show` to print the engine when it is not the default.
- Added `database.turso_database_url`, `database.turso_auth_token` (rendered redacted; falls
  back to `TURSO_AUTH_TOKEN`), `database.turso_sync_interval_seconds` and `database.node_label`.
  The section is read when a process opens its store, so changing it needs `hap daemon --ensure`.
- Under the turso engine the TUI, the `hap` verbs and the MCP server reach the store through
  the running daemon; with no daemon they report that the store is served by the daemon
  instead of showing an empty database.
- Added a unified fleet view under the turso engine: `hap agents` and the TUI Agents tab list
  every machine's agents (remote rows carry the machine as their last field / `name@label`),
  escalations, audit and kill history carry `node=<label>`, and `hap status` lists the other
  machines and their sync freshness. Confirm, answer, correct, dismiss and retry work on
  another machine's escalation and report `queued for node <label>`; `hap pause --node` /
  `hap resume --node` pause one machine; rename, enable/disable and focus of a remote agent
  are refused because those rows belong to the owning daemon.
- Added the `sqlite` task-source provider (`task_source_provider.provider = "sqlite"`, or per
  source): a checklist kept inside hap's own database rather than a file, addressed as
  `db://<node>/<name>`, so under the turso engine every machine's Tasks tab shows and edits
  every other machine's lists, and `hap task --node <machine> <agent> …` opens one from the
  CLI. Lists belong to one machine — only its daemon hands items out — and edits are
  compare-and-swapped on a revision, so no file lock is needed.
