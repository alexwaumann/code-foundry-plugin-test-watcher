# Test Watcher for Code Foundry

A [Code Foundry](https://github.com/alexwaumann/code-foundry) plugin that runs your
project's tests in a thread's worktree whenever its files change, and shows the result in
a **Tests** tab of the thread's side panel:

- **Header**: project and branch, a status pill (passing, failing, running, idle), how
  long the last run took and when, and a Run now button.
- **Status**: the test command, the last run, its duration, how many files changed since,
  a *Watch for changes* switch (per thread, on by default) and a form to set the
  project's test command.
- **Failures**: the failing tests parsed from `go test`, vitest and jest output, each with
  its package or file and a *Send to Claude* button. Other runners: the run's exit status
  and a pointer to Output.
- **Output**: the run in a real terminal (Code Foundry's terminal view, with *Open in
  bottom panel*); it stays after the run ends, until the next run replaces it. On a Code
  Foundry without plugin terminals (or when the worktree is outside your home folder):
  the output streamed live into a log, failures in red and warnings in amber (the last
  2,000 lines / 256 KB; the head is cut first).
- Actions: **Run now**, **Clear flag** while the thread is flagged, **Stop** while a run
  is going, **Send failures to Claude**: a message of at most 4 KB (the command, the
  failing tests, the last 60 output lines) asking Claude in that thread to fix them.

While the tests fail the thread is flagged red in the sidebar ("Test Watcher: Tests
failing: TestAdd"). The flag stays when you open the thread: it says the tests are still
failing, not that something happened once. It goes when:

- a run passes;
- you Stop a run, or switch watching off for the thread;
- you clear it: **Clear flag** in the Tests tab, the `clear-flag` command (also in the
  palette), the × on the flag in the thread header, or `code-foundry thread flag clear
  <thread> --plugin test-watcher`.

A flag you cleared stays down while the same tests fail; it comes back when the set of
failing tests changes (another test fails, or one of them passes while others still
fail), and after a passing run any failure raises it again. With watching off, only
Run now flags the thread.

How it runs: 2 s after files stop changing (debounced), one run per worktree at a time;
a newer change stops the run in progress (its whole process group) and starts again. The
command runs in the worktree with your login shell (`$SHELL -lc`) and `CI=true` (no watch
modes). In a terminal it runs under `script(1)`, which records what the terminal shows into
the plugin's data folder: that is where the failures list, the flag and *Send failures to
Claude* read the output from, colours stripped. Stop signals the run's process group, so
the terminal keeps the output up to the stop. Without a terminal it runs as the plugin's
own child with colours off.

The test command is guessed from the worktree: `go.mod` → `go test ./...`,
`package.json` → `npm test`, a `Makefile` with a `test:` target → `make test`,
`pyproject.toml` → `pytest`. Set your own per project in the Status tab or with
`set-command`; an empty value goes back to the guess.

Which threads are watched: a thread starts being watched the first time its Tests tab is
opened or one of the commands below runs for it, and stays watched (also across
restarts) until it is archived or removed, or you switch watching off for it.

## Install

```
code-foundry plugin install test-watcher --enable
code-foundry plugin install alexwaumann/code-foundry-plugin-test-watcher@v0.1.3 --enable   # a release directly
```

## Permissions and why

| Permission | Why |
|---|---|
| `commands: thread.send` | *Send to Claude* and *Send failures to Claude* post the failure excerpt into the thread. Nothing else is invoked. |
| `events: project, threads` | `project`: worktree changes (status counts, commits, branch switches) and removed worktrees. `threads`: which worktree and project each thread works in, and when a thread is archived or removed. |
| `threadStatus` | The red "Tests failing" flag on the thread while its tests fail (until they pass or you clear it). |
| `terminals` | The Output tab's terminal: each run is a terminal the plugin owns, in the thread's worktree. Disabling the plugin closes them. |

No secrets, no session environment, no API access. The plugin keeps its settings (test
commands per project, watched threads, threads with watching off, the failing tests each
flag was raised for) in
`~/.code-foundry/plugin-data/test-watcher/settings.json`, and the latest terminal run's
recorded output per worktree in `runs/` beside it.

## Commands

Each takes the thread from the context (`--context-thread <id>`, the GUI's selection, or
`$CODE_FOUNDRY_THREAD` inside a thread) or `--thread <id>`:

```
code-foundry plugin test-watcher run              # run now (replaces a run in progress)
code-foundry plugin test-watcher stop             # stop the run in progress
code-foundry plugin test-watcher set-command "go test -count=1 ./..."   # "" = back to the guess
code-foundry plugin test-watcher send-failures    # the failing tests to Claude in the thread
code-foundry plugin test-watcher clear-flag       # take the failing-tests flag off the thread
code-foundry plugin test-watcher toggle-watch     # watching on/off for the thread
code-foundry plugin test-watcher show tests       # open the Tests tab in the app
```

## Development

Needs Go 1.24+ and a running Code Foundry.

```
make build                                              # bin/test-watcher
code-foundry plugin dev "$PWD" --follow=false --yes     # link and enable as a dev plugin
code-foundry plugin restart test-watcher                # after every rebuild
code-foundry plugin surface test-watcher tests --context-thread <id>
code-foundry plugin act test-watcher tests tab --key tab-failures --context-thread <id>
code-foundry plugin logs test-watcher --lines 50
make test                                               # plugintest unit tests
make dist                                               # release tarballs + SHA256SUMS
```

The tests cover the debounce and newest-wins (process groups included), the `go test`,
vitest and jest parsers on captured output (`testdata/`), the views for failing and
passing runs, output streaming and caps, terminal runs against plugintest's fake
terminals (the argv, the recorded output, the exit status, replacing and stopping), the
flag lifecycle (sticky; cleared by a pass, Stop, watching off or the user; not raised again
for the same failures, across restarts too), the 4 KB excerpt, the command form and the edge states (no command,
command not found, worktree deleted).

Worktree events only fire when a worktree's status counts change, so an edit to a file
that is already modified does not produce one. The plugin also fingerprints each watched
worktree every 2 s (`git status` plus size and mtime of each changed file) to catch those.

The Output tab is one node (`outputNode` in `surface.go`): a `terminal` node while the
run's terminal is open, else the log node.

### A real run, 0.1.3 (Code Foundry dev build 19052860, isolated home, a scratch Go module)

`cf` runs that build with `CODE_FOUNDRY_HOME` set to the isolated home; edits to
`calc_test.go` are picked up by the watcher 2 s later.

```
$ cf plugin dev "$PWD" --follow=false --yes
enabled test-watcher 0.1.3 (starting)
$ # TestAdd now wants 6
$ cf thread flags s-c05266bdd960
test-watcher  error  Tests failing: TestAdd  2026-10-18 19:34:42  no      # UNTIL SEEN: no
$ cf plugin act test-watcher tests tab --key tab-failures --thread s-c05266bdd960
$ cf thread flags s-c05266bdd960
test-watcher  error  Tests failing: TestAdd  2026-10-18 19:34:42  no
$ cf plugin restart test-watcher
$ cf thread flags s-c05266bdd960
test-watcher  error  Tests failing: TestAdd  2026-10-18 19:34:42  no
$ cf plugin surface test-watcher tests --thread s-c05266bdd960   # elements
  clear-flag     click    clear-flag     Clear flag
$ cf plugin test-watcher clear-flag --thread s-c05266bdd960
Cleared the flag; it comes back when other tests fail
$ # TestAdd now wants 7: the run fails on the same test
test run ended exit=1 run=4
$ cf thread flags s-c05266bdd960
thread tw 0.1.3 check (s-c05266bdd960) has no flags
$ # TestMul now wants 7 as well: a different set
$ cf thread flags s-c05266bdd960
test-watcher  error  Tests failing: TestAdd and 1 more  2026-10-18 19:35:48  no
$ cf thread flag clear s-c05266bdd960 --plugin test-watcher   # the header chip's ×
$ # TestMul now wants 8: same set, no flag; the Clear flag button is gone
$ cf thread flags s-c05266bdd960
thread tw 0.1.3 check (s-c05266bdd960) has no flags
```

### A real run, 0.1.1 (Code Foundry dev build 73685d8b, isolated home, a scratch Go module)

```
$ code-foundry plugin test-watcher run --thread s-9dac5b289495
Running `go test ./...` in /Users/alex/.cf-plugfix/scratch/calc
$ code-foundry plugin act test-watcher tests tab --key tab-output --thread s-9dac5b289495
$ code-foundry plugin surface test-watcher tests --thread s-9dac5b289495
header pill: failing · tabs: Status | Failures(2) | Output*
node output-terminal {"terminal": {"terminalId": "t-d9c6b3aa70bd", "title": "go test ./...", "height": "fill"}}
$ code-foundry plugin terminals test-watcher
t-d9c6b3aa70bd  exited (1)  go test ./...  script  /Users/alex/.cf-plugfix/scratch/calc
$ code-foundry thread flags s-9dac5b289495
test-watcher  error  Tests failing: TestAdd and 1 more
$ code-foundry plugin test-watcher send-failures --thread s-9dac5b289495
Sent the failing tests to Claude
$ # a 60 s run, stopped: the group dies, the terminal stays with its output
t-dcb02521c7cb  exited (15)  echo slow start; sleep 60; go test ./...
$ # Claude fixed calc.go; the watcher's run in a new terminal passed and cleared the flag
t-a9b722e3770f  exited (0)  go test ./...
```

### A real run, 0.1.0 (Code Foundry dev build, isolated home, a scratch Go module)

```
$ code-foundry project add ~/cf-scratch/tw-scratch
registered tw-scratch (d32dcec60726)
$ code-foundry thread new --project d32dcec60726 --model 'claude-4.5-haiku[1m]' --name "test watcher run"
created thread test watcher run (s-f9b8cf835989) in /Users/alex/cf-scratch/tw-scratch
$ code-foundry plugin dev "$PWD" --follow=false --yes
$ # edit calc_test.go so TestAdd wants 6; 2 s later:
$ code-foundry plugin surface test-watcher tests --context-thread s-f9b8cf835989
header: tw-scratch @ main [failing] took 256ms · just now
  Command: go test ./... (detected); Last run: failed · just now; Duration: 256ms
$ code-foundry thread flags s-f9b8cf835989
test-watcher  error  Tests failing: TestAdd  2026-10-18 07:41:52  yes
$ # edit the same (already modified) file again: TestMul wants 7
tabs: Status | Failures(2)* | Output
  failures: TestAdd (example.com/scratch/calc); TestMul (example.com/scratch/calc)
test-watcher  error  Tests failing: TestAdd and 1 more
$ code-foundry plugin act test-watcher tests send-failures --context-thread s-f9b8cf835989
$ # Claude restores the expected values; the watcher reruns 2 s after its edit:
header: tw-scratch @ main [passing] took 129ms · just now
$ code-foundry thread flags s-f9b8cf835989
thread test watcher run (s-f9b8cf835989) has no flags
```

## Changelog

### 0.1.3

- **The failing-tests flag is sticky**: opening the thread no longer clears it. It goes
  when a run passes, when you Stop a run or switch watching off, or when you clear it.
- **Clear flag**: a button in the Tests tab while the thread is flagged, and the
  `clear-flag` command (palette and CLI). The thread header's × and `code-foundry thread
  flag clear` work too. A cleared flag is not raised again for the same failing tests,
  also across restarts; a different set raises it.
- With watching off for a thread, only Run now flags it (not runs another thread's
  watching starts in the same worktree).
- Same permissions as 0.1.2, so updating keeps it enabled.

### 0.1.2

- Built against the released Code Foundry SDK, `pluginsdk` v0.1.0 (code-foundry
  `4ed617fd`): the surface's `Close` takes the close reason the host now sends. No
  behaviour change; same permissions as 0.1.1, so updating keeps it enabled.

### 0.1.1

- **Asks for one more permission, `terminals`**: the Output tab now shows the run in a
  real terminal (with *Open in bottom panel*) instead of a log. Because the permissions
  grew, updating from 0.1.0 disables the plugin until you agree again
  (`code-foundry plugin enable test-watcher` lists what it asks for). A Code Foundry
  without plugin terminals keeps the log.
- The Failures list shows each test's package or file again (it was a dense list, which
  hides a row's second line).
- *Send failures to Claude* is also a command (`send-failures`, in the palette); the
  view's buttons are commands the app runs with the panel's thread.
- Icons only from the set Code Foundry draws (`folder-x` drew a plain circle).
- Built against the Code Foundry SDK at 73685d8b; CI runs vet and race tests on every
  push and pull request; `make registry-entry` writes the registry entry.

### 0.1.0

- First release.

## License

See [LICENSE](LICENSE).
