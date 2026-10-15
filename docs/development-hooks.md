# Development and Git hooks

## Review and approve a hook policy

Run these commands from the repository whose hooks you want to configure.
Preparing a policy writes a candidate inside that repository's own Git admin
directory and prints its path:

```sh
confctl-hooks prepare
```

Review that file. To adopt it, copy its contents to the root `lefthook.yml`,
review the diff and stage it. `prepare` does not overwrite or stage that working
file. The policy has only the fixed pre-commit and commit-msg commands for the
selected checker; arbitrary checkout commands and extra Lefthook configuration
files are refused.

```sh
git diff -- lefthook.yml
git add -- lefthook.yml
confctl-hooks trust
confctl-hooks install
```

`trust` displays the finite old/new policy and tool changes and asks for approval.
Use `confctl-hooks trust --yes` only after reviewing those changes, for example in
CI with an approved checkout. Staged and working policy bytes and modes must
agree. A mismatch, changed tool or stale receipt refuses; running a hook never
approves it. After changing tools or policy, prepare and review the new candidate,
then explicitly trust and install it again. Unrelated source edits do not require
new trust.

The private receipt binds the repository root and own admin directory, selected
checker and tools, fixed child environment and finite policy files. It is local
operator approval, not a sandbox against a user who can replace their own files
and launch environment. It does not sign commits or grant credentials.

## Installation and restore

Installation selects wrappers for pre-commit and commit-msg in the repository's
own Git admin directory. It does not run `lefthook install` or replace shared
hook files. An existing own `core.hooksPath`, an unrecognized wrapper or an extra
active predecessor hook requires an explicit migration rather than being
silently suppressed.

For linked worktrees, installation requires `extensions.worktreeConfig` to have
been enabled through the repository's coordinated setup. The installer does not
enable it or write common config. In a shared bare clone, coordinate this setup
and predecessor-hook migration with all worktree owners before changing it.

To restore the recorded own hook selection:

```sh
confctl-hooks restore
```

Restore verifies the selected directory and wrapper contents before removing its
own wrappers and installation record. The trust receipt and candidate remain.
An ordinary installation write failure attempts to retain the previous recognized
installation; errors include any failed rollback. If a process is interrupted or
an ownership check refuses, inspect the reported own admin files and effective
`core.hooksPath` before further setup. Do not delete shared hooks, reset an index
or overwrite another worktree's configuration to make installation succeed.

## What commits check

Pre-commit validates staged content in an owned detached snapshot, including an
alternate index and partial staging. It never formats files back into the
checkout or stages generated changes. Nix and Go formatting checks use literal
selected paths. A selected Go source file, root `go.mod`/`go.sum`, or
`extensions/go.mod`/`extensions/go.sum` also reaches semantic checks. Before any
Go vet or test starts, both present modules are checked against the packaged
manifest. Children use private temporary and cache directories and do not inherit
the real Git index, Git configuration overrides or operator module caches.

Lefthook starts in a separate private Git repository with the approved policy
and freshly initialized Git metadata. Its empty index and refs do not select
files. The checker uses the validated snapshot for file selection and runs
formatters and Go checks there; the snapshot has no `.git` directory. The parent
joins its children before cleaning up both temporary directories.

Configuration shells can pass `hookGoModules` to `mkConfigDevShell`. Each entry
has `directory`, `source` and `proxy`: directory is exactly `.` or `extensions`,
source is the immutable module source used by its Nix package, and proxy is that
package's `buildGoModule` dependency output built with `proxyVendor = true`.
Use `null` only for a module with no external dependencies. The shell rejects
duplicate entries and directories other than `.` or `extensions`.
Configuration tools must declare their own extension module explicitly.
The empty default works only when no Go module is reached.

The Go-tool output binds exact `go.mod` and `go.sum` hashes and presence, so an
absent sum differs from an empty one. Nix materialization rejects filesystem-local
replace directives using the declared Go's module metadata. Hooks read only the
bound store manifest and detached staged files, then use a declared `file://`
proxy with no fallback, readonly module mode and local toolchain selection.
Missing or corrupt dependencies fail the checks. Go does not download
dependencies from the network, use an operator cache or repair a staged
manifest. Repository tests still execute code and can access the network;
these dependency controls do not sandbox tests.

Changing SDK requirements or module manifests requires rebuilding the proxy and
Go tools and explicitly reviewing and trusting the new binding. The prior
binding refuses the changed manifests. Preparing or entering a shell does not
approve that change.

Commit-msg reads the literal message path and never rewrites it. Empty messages
reject the commit. Subject/body width, trailing periods and missing subject/body
separation are warnings. Git's `core.commentChar` and `core.commentString` are
read in configuration order, including scopes and includes; the last assignment
wins. Empty values and configuration-read errors refuse. With neither key set,
the prefix is `#`. Custom nonempty prefixes are literal, including multi-character
values. `auto` is also literal: these checks do not resolve Git's automatically
chosen character or infer comments from the message.

Normal checks reject reached Ruby source because no RuboCop tool is selected.
For Ruby services or the original CLI, use their explicit Ruby environment and
existing hook policy. Do not silently skip lint or add Ruby to the normal Go
bundle to accommodate that lane.
