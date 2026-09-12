# agy (Antigravity CLI) — what is different

## menus commit on a bare digit

agy selects a menu option from the digit alone, with no Enter. **Any typed text
containing a digit can pick an option** if a menu is open. Option 3 on a command
approval is usually "always allow … (Persist to settings.json)".

Before sending anything to an agy pane, prove no modal is up:

```sh
scr=$(herdr agent read <pane> --source visible --lines 25)
printf '%s' "$scr" | grep -qE "Run this command\?|Accept this file edit\?|Allow access to this file\?|CLI experience" \
  && echo "modal open - do not send" || hap task <agent> send <n> --yes
```

## forms hap cannot answer

- **Vendor survey** (`[1] Good [2] Fine [3] Bad [0] Skip`) — classified
  `unclassifiable` by design: hap never presses keys into vendor UI unattended.
  Clearing it needs a keystroke (`herdr agent send-keys <pane> 0`), which is one
  of the few legitimate uses of raw keys.
- **Free-text `Write-in...` rows** — refused as a verdict; the row stays for a
  human.
- **Trust prompt** ("Do you trust the contents of this project?") — its "No,
  exit" option terminates the session. Guard it before running agy unattended.

## modes

```sh
hap mode <agent>                  # read
hap mode <agent> acceptEdits --yes  # idempotent; presses shift+tab until it matches
```

`hap mode` refuses while a modal covers the composer footer. If the edit modal
itself offers `shift+tab to auto-approve file edits`, that chord works:

```sh
herdr agent send-keys <pane> shift+tab   # then verify the footer says accept-edits
```

`acceptEdits` is the cure for edit-approval stalls: hap does not answer agy's
`Accept this file edit?` modal, so without it every edit blocks.

## other behaviours worth knowing

- **agy reports parked as `done`, not `idle`.** Anything you key on `idle`
  misses a parked agy agent.
- **Start an agy agent in the directory it will work in.** It scopes permissions
  to its start cwd and raises an approval per file outside it. They graduate to
  one rule and then answer silently, but the first stretch is slow.
- **A composer cannot be cleared from outside** — `herdr agent send-keys`
  rejects `C-u`, `ctrl-u` and `c-u`. Text typed into it can only be submitted or
  left.
- **Text delivered mid-turn queues in the composer** and fires when the turn
  ends — out of order, and it has arrived truncated. Deliver only to a parked
  pane.
- **Background work displaces the footer.** agy paints a task strip while
  running, which older builds could not read past.
