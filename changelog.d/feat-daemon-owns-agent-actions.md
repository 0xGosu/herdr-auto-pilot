- Added an operator-action queue the daemon drains, so a request that has to
  reach a live agent is executed by the daemon rather than by whichever `hap`
  process the operator happened to type into. Each request carries its own
  outcome, which is how a surface that queues one can report whether it landed —
  the control socket has never had a reply channel.
