- Fixed the daemon losing bookkeeping writes while it published the running
  agents. Publishing took a database transaction on the loop that handles every
  agent, on the same paths as the daemon's own work — and because those
  transactions began as readers, one that committed in between made another
  fail outright with `database is locked (517)`, which is not retried. The
  daemon logs such a failure and carries on, so the write simply never
  happened: an agent's first-seen time was left unreset after its pane had been
  recycled. Publishing now runs off that loop entirely, transactions take their
  lock up front, and the database hands out enough connections that a read no
  longer waits behind an unrelated write
- Changed when the herd is republished. It happens at startup, on the
  once-a-minute sweep, and every two seconds while a TUI is open — no longer on
  every operator action. Nothing reads any fresher for it: `hap agents` and
  `hap status` are pure reads and never woke the daemon in the first place
- Fixed an agent's status briefly reverting after it started working. The daemon
  lists its agents and records that listing a moment later, so a status change
  arriving in between was overwritten by the older reading — and with no TUI
  open the next listing is a minute away. An agent that had just gone to work
  could read as free for that minute, including to `hap task send`
