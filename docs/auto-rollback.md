# Auto-rollback helper

`confctl.build.<key>.autoRollback` is an executable file built with the machine's
selected `pkgs`. Clients invoke that file directly:

```text
AUTO_ROLLBACK [-t|--timeout TIMEOUT] TOPLEVEL ACTION CHECK_FILE
```

The default timeout is 60 seconds. Signed integers, including zero, negative
values, octal, binary, hexadecimal and digit separators, are accepted without
a size limit. Later timeout options replace earlier ones. Options may follow
operands unless `POSIXLY_CORRECT` is present, including an empty value. `--`
ends option parsing. Exactly three operands are required; the action and paths
are passed through without a whitelist. `-h` and `--help` print usage.

The helper prints `Deploying with auto-rollback` before parsing, including on
help and errors. The helper supports the Bash and Zsh completion options from
the original parser. `--version` and `--version=PKG` fail with
`PROGRAM: version unknown` after the startup banner. Package queries are
unsupported, including empty or opaque arguments; the helper does not load
Ruby packages.

## Confirmation and timeout

The helper reads the literal symlink text of `/run/current-system`, writes
`switching` to the supplied check file without a newline, and starts
`TOPLEVEL/bin/switch-to-configuration ACTION`. The child inherits stdin,
stdout, stderr, environment and working directory. After waiting, the helper
writes `switched` without a newline and starts its wall-clock confirmation
timer. A confirmation written during switching can therefore be overwritten.
The switch child has no deadline.

Every poll sleeps 0.5 seconds before reading the file. Confirmation is the
exact byte string `confirmed` after stripping ASCII space, tab, CR, LF, vertical
tab, form feed and NUL at the edges. The client's `echo confirmed` newline is
accepted. Confirmation wins before the helper checks expiry, even with a zero
or negative timeout. Success prints `Configuration confirmed`, unlinks the
file and exits zero.

Expiry uses the strict wall-clock condition `start + timeout < now`. At the
deadline itself the helper continues polling. On expiry it executes
`READLINK_TARGET/bin/switch-to-configuration ACTION`, retaining the original
action and relative symlink text. It waits, prints `Rollback complete`, returns
one and leaves the check file in place. It does not update the system profile.

Normal nonzero and signaled child termination are deliberately ignored for
both activation and rollback, matching the source helper. These statuses do
not prove either operation succeeded. Spawn and wait-system errors stop the
helper. Filesystem and output errors also stop it without automatic rollback,
retry or check-file cleanup. State already written and activation effects
remain; a failed switch may leave `switching`, which can keep an older client
polling. Confirmation establishes connectivity, not system health.

## Stored generations and recovery

The flat executable output, `toplevel`/`autoRollback` output names and existing
`auto_rollback` generation field are unchanged. New generations contain the Go
helper. Existing generations retain their exact helper paths and Ruby closures;
clients continue to execute those stored files directly, including generations
with no helper. The `switching`/`switched`/`confirmed` handshake requires no new
state format or coordinated fleet update.

After a failure, inspect the current system, profile and retained check file
before choosing a recovery action. Restoring the prior helper/tool revision
does not reverse a switch that has already run.

## Diagnostics and focused checks

Malformed options use concise diagnostics instead of Ruby exception frames.
The startup banner stays on stdout; diagnostics stay on stderr with status one.
`PROGRAM` below is the actual invocation path. The Ruby column names the source
exception contract, not a captured backtrace.

| Input | Ruby source contract | Go diagnostic body |
| --- | --- | --- |
| `-t oops` | Uncaught `OptionParser::InvalidArgument` | `PROGRAM: invalid timeout: oops` |
| `-t` | Uncaught `OptionParser::MissingArgument` | `PROGRAM: missing argument: -t` |
| `--unknown` | Uncaught `OptionParser::InvalidOption` | `PROGRAM: invalid option: --unknown` |
| `--help=x` | Uncaught `OptionParser::NeedlessArgument` | `PROGRAM: needless argument: --help=x` |
| `--*-completion` | Uncaught `OptionParser::AmbiguousOption` | `PROGRAM: ambiguous option: --*-completion` |
| Wrong operand count | `Invalid arguments` followed by parser usage | Same ordinary message and usage |

Focused checks in the declared development environment are:

```sh
go test ./cmd/auto-rollback -count=1
go vet ./cmd/auto-rollback
```

Tests cover the twenty source-nominated cases, literal output/state bytes,
ordering, arbitrary signed timeout arithmetic, confirmation precedence and
partial failures with injected filesystem and wall-clock dependencies. Fake
Go switch children and disposable files test actual Start/Wait, separate
streams, stdin, environment, CWD and nonzero exits. Synthetic exit records
exercise status classification without claiming native signal observations.
Tests never invoke the production entrypoint against the host's current system.
