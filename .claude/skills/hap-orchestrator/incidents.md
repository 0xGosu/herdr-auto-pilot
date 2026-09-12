# incidents — playbooks

## the LLM backend stops answering

Symptom: escalations carrying `[llm_no_submit] llm CLI failed without
submit_decision: exit status 1`, on every prompt rather than occasionally.

Reproduce it directly before concluding anything — the CLI itself will say why:

```sh
cd /tmp && <the llm command from `hap config fields`> 'reply with the single word ok'
```

A quota or auth failure needs the operator. If they choose another backend,
**use the presets** — never hand-write the recipes:

```sh
cp "$(hap config path)" ~/config-backup-$(date +%F).toml   # restore point first
for k in llm.command llm.task_generate_command llm.learn_from_user_command llm.reranking_command; do
  hap config set "$k" ""                 # a preset installs only into an UNSET key
  hap config set "$k" --preset <claude|codex|agy>
done
hap config fields | grep '^llm\.'
```

Hand-writing them through `hap config set <key> '<argv>'` truncates the ~1 KB
prompts at the first apostrophe — the presets write argv directly to avoid it.
Not every backend serves every key; the picker only offers what exists.

Then watch for the first successful consult (`hap audit` showing `auto:` with an
`llm=` score) rather than assuming it worked.

## the daemon restarted

`daemon.started` **with no preceding `daemon.stopped`** means it died rather
than being restarted. Nothing in `hap status` or `hap escalations` reports a
crash:

```sh
grep -c '^panic:' ~/.local/state/herdr/plugins/herd-auto-prompter/daemon.stderr.log
tail -40 ~/.local/state/herdr/plugins/herd-auto-prompter/daemon.stderr.log
```

Check the stack's file paths: a worktree path means someone swapped in a dev
build; trimmed module paths mean the installed release. Report a shipped-code
crash to the operator with the stack.

## upgrading the plugin

```sh
herdr plugin install <owner>/<repo> --yes    # --yes is required non-interactively
hap daemon --ensure
hap version && hap status | head -4
```

Then **restart the event stream** so it runs the new binary, resuming from the
last seq you handled. Expect a brief daemon restart; agents keep working.

## quota pressure

Agent panes show the account's weekly usage. When it is close to the limit,
remember hap's own answering, task generation, learning and re-ranking all run
on whichever backend is configured — so the herd and the agents draw on the same
budget. Park agents whose lists are finished (`hap disable`), and prefer a
cheaper model for the re-ranking judge, which is the most frequent caller.

## memory pressure

Language servers, not agents, are usually the largest consumers, and each
worktree adds one:

```sh
ps -eo rss,etime,comm --sort=-rss | head -10
```

Report it with numbers and let the operator decide what to reclaim — do not kill
processes you did not start.
