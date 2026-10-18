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
  a *Send to Claude* button. Other runners: the run's exit status and a pointer to Output.
- **Output**: the run's output, streamed live, failures in red and warnings in amber (the
  last 2,000 lines / 256 KB; the head is cut first).
- Actions: **Run now**, **Stop** while a run is going, **Send failures to Claude**: a
  message of at most 4 KB (the command, the failing tests, the last 60 output lines)
  asking Claude in that thread to fix them.

While the tests fail the thread is flagged red in the sidebar ("Test Watcher: Tests
failing: TestAdd"); opening the thread or the next passing run clears it.

How it runs: 2 s after files stop changing (debounced), one run per worktree at a time;
a newer change stops the run in progress (its whole process group) and starts again. The
command runs in the worktree with your login shell (`$SHELL -lc`), with `CI=true` and
colours off.

The test command is guessed from the worktree: `go.mod` → `go test ./...`,
`package.json` → `npm test`, a `Makefile` with a `test:` target → `make test`,
`pyproject.toml` → `pytest`. Set your own per project in the Status tab or with
`set-command`; an empty value goes back to the guess.

Which threads are watched: a thread starts being watched the first time its Tests tab is
opened or one of the commands below runs for it, and stays watched (also across
restarts) until it is archived or removed, or you switch watching off for it.

## Install

```
code-foundry plugin install test-watcher
```

(Installing from the registry arrives in a later Code Foundry version. Until then:
download a release tarball for your Mac, unpack it, and run
`code-foundry plugin dev <unpacked folder> --yes`.)

## Permissions and why

| Permission | Why |
|---|---|
| `commands: thread.send` | *Send to Claude* and *Send failures to Claude* post the failure excerpt into the thread. Nothing else is invoked. |
| `events: project, threads` | `project`: worktree changes (status counts, commits, branch switches) and removed worktrees. `threads`: which worktree and project each thread works in, and when a thread is archived or removed. |
| `threadStatus` | The red "Tests failing" flag on the thread while its tests fail. |

No secrets, no session environment, no API access. The plugin keeps its settings (test
commands per project, watched threads, threads with watching off) in
`~/.code-foundry/plugin-data/test-watcher/settings.json`.

## Commands

Each takes the thread from the context (`--context-thread <id>`, the GUI's selection, or
`$CODE_FOUNDRY_THREAD` inside a thread) or `--thread <id>`:

```
code-foundry plugin test-watcher run              # run now (replaces a run in progress)
code-foundry plugin test-watcher stop             # stop the run in progress
code-foundry plugin test-watcher set-command "go test -count=1 ./..."   # "" = back to the guess
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
passing runs, output streaming and caps, the flag lifecycle, the 4 KB excerpt, the
command form and the edge states (no command, command not found, worktree deleted).

Worktree events only fire when a worktree's status counts change, so an edit to a file
that is already modified does not produce one. The plugin also fingerprints each watched
worktree every 2 s (`git status` plus size and mtime of each changed file) to catch those.

The Output tab is one node (`outputNode` in `surface.go`): when Code Foundry renders
plugin-owned terminals, it becomes a `terminal` node and the run moves into it.

### A real run (Code Foundry dev build, isolated home, a scratch Go module)

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

## License

See [LICENSE](LICENSE).
