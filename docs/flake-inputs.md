# Flake inputs

Root input commands work with a `flake.nix` and do not require cluster outputs.
Channel and machine commands require outputs from `confctl.lib.mkConfctlOutputs`:

- inputs are normal flake inputs locked in `flake.lock`
- machines select “channels” via `inputs.channels`
- machines can override role→input mapping via `inputs.overrides`
- updates are performed by `confctl inputs ...` (or `nix flake lock --update-input ...`)

This document explains the model and the common workflows.

## Roles, inputs, and channels

A **role** is a named dependency such as:

- `nixpkgs`
- `vpsadminos`
- `vpsadmin`

A role is mapped to a **flake input name** via **channels**.

A **channel** is just a name like `production` or `staging` that selects a set of role mappings.

Example:

```nix
channels = {
  staging = {
    nixpkgs = "nixpkgsStable";
    vpsadminos = "vpsadminosStaging";
    vpsadmin = "vpsadminStaging";
  };

  production = {
    nixpkgs = "nixpkgsStable";
    vpsadminos = "vpsadminosProduction";
    vpsadmin = "vpsadminProduction";
  };
};
```

## `mkConfctlOutputs` flake skeleton

A minimal pattern:

```nix
{
  description = "my cluster config (confctl flake)";

  inputs = {
    confctl.url = "github:vpsfreecz/confctl";

    nixpkgs.follows = "nixpkgsStable";
    nixpkgsStable.url = "github:NixOS/nixpkgs/nixos-25.11";
    vpsadminosStaging.url = "github:vpsfreecz/vpsadminos/staging";
    vpsadminosProduction.url = "github:vpsfreecz/vpsadminos/staging";

    vpsadminStaging = {
      url = "github:vpsfreecz/vpsadmin/2026-02-19-flakes";
      inputs.vpsadminos.follows = "vpsadminosStaging";
    };

    vpsadminProduction = {
      url = "github:vpsfreecz/vpsadmin/2026-02-19-flakes";
      inputs.vpsadminos.follows = "vpsadminosProduction";
    };
  };

  outputs = inputs@{ self, confctl, ... }:
    let
      channels = {
        staging = {
          nixpkgs = "nixpkgsStable";
          vpsadminos = "vpsadminosStaging";
          vpsadmin = "vpsadminStaging";
        };

        production = {
          nixpkgs = "nixpkgsStable";
          vpsadminos = "vpsadminosProduction";
          vpsadmin = "vpsadminProduction";
        };
      };
    in
    {
      confctl = confctl.lib.mkConfctlOutputs {
        confDir = ./.;
        inherit inputs channels;
      };

      # Optional: configuration-repo dev shell
      devShells.x86_64-linux.default = confctl.lib.mkConfigDevShell {
        system = "x86_64-linux";
        mode = "minimal";
      };
    };
}
```

## Selecting channels on a machine

In machine metadata (typically `cluster/<name>/module.nix`), choose channels:

```nix
{ ... }:
{
  cluster."my-machine" = {
    spin = "nixos";
    inputs.channels = [ "production" ];
  };
}
```

Multiple channels can be combined; later channels can override roles from earlier ones.

## Per-machine overrides

To override one role for a single machine:

```nix
cluster."my-machine".inputs.overrides.nixpkgs = "nixpkgsMunin";
```

Per-machine overrides take precedence over every selected channel.

## Updating inputs

Inputs are flake inputs in `flake.lock`.

Common commands:

```bash
confctl inputs ls
confctl inputs update --commit <input...>

confctl inputs channel ls
confctl inputs channel update --commit '{production,staging}' vpsadminos

confctl inputs machine update --commit <machine> nixpkgs
```

- `--no-changelog` disables including `git log --oneline old..new` in the commit message.
- `--downgrade` is useful when you intentionally move to an older revision and still want the changelog direction to make sense.

`confctl inputs ls --json [input-pattern]` prints an array of rows with fields
`input`, `type`, `ref`, `rev` and `url`. JSON revisions are full locked revisions;
sources without a revision have `rev: null`. Input aliases use the same complete
`follows` resolution as other input commands. The default table still shows
eight-character revisions. An unmatched pattern produces an empty array.

## Guarded automated updates

Use `--forward-only` in publishing workflows to retain manually pinned commits
until the upstream branch contains them:

```bash
confctl inputs update --forward-only nixpkgs
confctl inputs update --forward-only --all
confctl inputs channel update --forward-only --commit --no-changelog \
  '{production,staging}' nixpkgs
confctl inputs machine update --forward-only --commit build.example.org nixpkgs
```

Options must come before positional arguments. The guard requires an existing
version-7 `flake.lock` tracked in Git and a clean tracked checkout and index.
Use a checkout with one writer. Cooperating guarded confctl updates use a
process lock; other tools must not write to the checkout concurrently.

Guarded updates require Nix 2.19 or newer, including
`--reference-lock-file` and `--output-lock-file` support. The packaged confctl uses
its bundled Nix; installing another Nix in CI does not necessarily change that
runtime. Unsupported flags or unmatched selectors fail without updating the
real lock. There is no fallback to an update in place.

confctl resolves requested `follows` aliases to their terminal input paths and
deduplicates shared targets. For example, a root `nixpkgs` following
`vpsadminos/nixpkgs`, where `vpsadminos` follows `vpsadmin/vpsadminos`, updates
`vpsadmin/vpsadminos/nixpkgs`. It leaves both parent revisions unchanged.

Nix writes a candidate lock beside the original flake, using a separate snapshot
of the old lock as its reference. confctl validates the complete reachable
graph before atomically installing the candidate. It compares logical dependency
paths, including descendants and aliases, rather than generated node IDs. A
forward root update that regresses a nested dependency is rejected as a whole.
Local lock bytes, root `flake.nix`, Git HEAD, index entries and tracked checkout
state are checked again before installation. No commit runs before acceptance.

Each existing Git revision must equal or be an ancestor of its replacement in
the same supported source. Currently the ancestry provider supports
`github.com` sources with full immutable Git revisions, including
`flake = false` sources. It checks commit ancestry with Git using the same full
repository mirrors as changelogs. It does not use GitHub's compare API or read
API tokens. Git's ordinary URL and authentication configuration remains in
effect; inaccessible sources are evidence errors. Nix resolves the candidate
separately and may still use GitHub APIs through its own fetcher.

Mirrors are stored in `${XDG_CACHE_HOME:-$HOME/.cache}/confctl/git-mirrors`.
Each directory is named by the SHA256 digest of its exact source URL, preserving
the existing cache layout and URL spelling. A cold comparison clones a full
mirror; a warm comparison reuses both exact commit objects when present and
fetches only missing full SHAs. A cold nixpkgs mirror can require substantial
disk space and time. Persist this cache between CI runs; a cache miss never
bypasses validation. `check-update` uses the same cache and may clone or fetch,
while leaving both supplied lockfiles and the checkout untouched.

A sibling lock file serializes initialization, fetching and reads for each URL,
including changelog reads. New mirrors are installed atomically after successful
cloning and validation. Existing shallow, partial, grafted, externally backed,
wrong-origin or malformed mirrors fail with the cache key in the error. They
are not automatically deleted, repaired or replaced; diagnose the reported
entry and arrange recovery separately. Cached objects must be exact commits,
not tags, trees or blobs. A negative ancestry result requires a successful walk
of both tips' complete commit-parent history. Missing history and Git failures
remain errors. Replacement objects and inherited repository redirection cannot
alter the proof.

The cache and ordinary Git configuration belong to the invoking user; this is
not protection against a hostile writer with that user's access. Older confctl
and manual Git writers do not honor the new lock. Reuse their full mirrors
sequentially, without concurrent old/new writers. Mirror locks are released
before commit hooks run. Changelog failures retain their existing soft error
behavior; they cannot turn a failed ancestry check into an accepted update.

Source, fetcher or extraction changes require manual review. The guard also rejects changes to
locked attributes at the same revision, including a changed `narHash`.
Previously reachable paths cannot disappear. New paths must contain supported
immutable Git sources. Unchanged unsupported sources may remain; changed path,
tarball, indirect or other unsupported sources fail with an evidence error.
The check does not prove that live path-source contents stayed unchanged.
Changing the original branch/ref declaration alone is not a source-content
change when the locked source is identical.

A rejection or evidence error leaves the original lock, index and HEAD
untouched by confctl. Nix failures, malformed candidates, cached GitHub metadata
fallback warnings and unavailable ancestry evidence all stop the update.
Concurrent checkout changes stop installation and are left for the writer to
reconcile. A commit-hook failure after installation returns failure and leaves
the validated candidate in place; hook side effects are not rolled back.
Each invocation is a separate transaction: earlier local commits from a
multi-command job are not undone when a later invocation fails.

Exit status is `0` for an accepted update or no change, `3` for a known policy
rejection, and `1` for missing or unsupported evidence and tool errors. Both
nonzero statuses must stop publication. `--downgrade` only changes changelog
direction and cannot bypass `--forward-only`. Unflagged manual updates and
`inputs set` retain their existing behavior for deliberate revision changes.

Ancestry preservation retains the pinned commit in history. It does not detect
later reverts, prove patch equivalence after a cherry pick, or establish software
compatibility. Keep the repository's existing tests. When a pin is ahead of or
diverges from its upstream branch, retain it until the branch contains it, or
make a reviewed manual transition. Do not fall back to an unguarded update when
the check fails.

### Check the final lock before publishing

Save the workflow's original lock outside the checkout. After commit hooks and
tests have run, export `HEAD:flake.lock` and compare that committed snapshot
against the same baseline. This checks the lockfile that Git will publish:

```bash
set -euo pipefail
baseline=$(mktemp)
cp flake.lock "$baseline"
confctl inputs update --forward-only --commit --no-changelog nixpkgs
# Run the repository's required checks here.
final_lock=$(mktemp)
git show HEAD:flake.lock > "$final_lock"
confctl inputs check-update --old-lock "$baseline" --new-lock "$final_lock"
# Publish only after every preceding command succeeded.
```

`check-update` reads both lock files and applies the same complete-graph policy.
It does not invoke Nix, install a lock or commit. Its exit statuses are the same
as guarded update statuses. It can run without confctl cluster outputs.

Use an ordinary non-force Git push after successful checks. If the target branch
moved, retry in a new checkout from a fresh baseline so a
concurrent manual pin is included in the next check. An ancestry check against a
stale local baseline does not establish safety against a changed remote branch.

## Nested inputs and `follows`

If input **A** depends on input **B** and you want the *top-level flake* to decide the revision of **B**, use `follows`.

Example (vpsadmin → vpsadminos):

```nix
inputs.vpsadminStaging = {
  url = "github:vpsfreecz/vpsadmin/2026-02-19-flakes";
  inputs.vpsadminos.follows = "vpsadminosStaging";
};
```

Because `follows` is per-input-name, you typically need separate inputs per environment (staging vs production) to pin independently.

## Source inputs and evaluation

Inputs used only as source directories may use `flake = false`:

```nix
inputs.myToolInput = {
  url = "github:example/my-tool/main";
  flake = false;
};
```

Path inputs and `follows` links are supported. `inputs` in machine modules maps
roles to selected source store paths; use it to import raw files.
`flakeInputs` exposes root inputs, and `inputsInfo.<role>.input` identifies the
selected root input for accessing flake exports. Raw sources have no flake
exports, and revision fields may be absent for sources without Git metadata.

Flake builds are pure by default. If existing `<nixpkgs>`-style imports require
NIX_PATH, set both `confctl.nix.impureEval = true` and
`confctl.nix.legacyNixPath = true`. `legacyNixPathMap` selects the role names
exposed with `-I`; machines with conflicting source paths build in separate
groups. This bridge does not provide a non-flake configuration backend.

The tool and configuration's confctl input must both include `moduleOptions`
support to use the option reference and `confctl ls -L`. That output evaluates
public confctl and cluster options, including `modules/cluster/default.nix`,
without building machine systems.

## Rust input mutations and recovery

`confctl` implements update and set at root, channel and
machine scope. These commands require a flake and run without confirmation.
They hold one `.confctl/mutation.lock` advisory lock through selection, Nix,
changelog preparation, Git hooks/editor and joined completion. Avoid concurrent
Ruby, external Nix or Git writers: they do not share this lock.

Root update preserves explicit names, order and duplicates. `--all` instead uses
the sorted root names in the existing lock and ignores positional names. An
explicit update may create a missing lock; set requires a lock and GitHub or Git
input metadata. Set prepares every override before its first child and restores
all selected original metadata in each published temporary result. Input paths
resolve complete `follows` chains; unknown JSON and URL/query fields are preserved.
These operations do not edit `flake.nix` or refresh generated lists.

Channel selectors preserve brace/comma order and duplicates; glob matches sort
by name. Underlying input names are deduplicated at their first occurrence.
Channel set refuses inputs shared outside the selected channel/role pairs unless
`--allow-shared` (also `--allow_shared`) is supplied, then prints the uses. Channel
update has no such restriction. Machine routes resolve one exact name or key
and role through configured Nix evaluation, without inventory builds or host
contact. Channel and machine summaries read the actual resulting lock.

Null/false channel mappings are empty; an absent, null or false explicitly
requested role input selects nothing. Lock-graph validation rejects empty input
path components.
Malformed selected mappings and other non-string names fail before mutation,
including malformed mappings examined by channel set's shared-use scan. Channel
listing retains its existing tolerant rendering.

Commit defaults to false; changelog and editor default to true. Only selected
revision changes enter Git work with `--commit`. Metadata-only changes may leave
a changed lock while reporting no revision changes. Messages group changes by
repository and old/new revision, retain the first group's raw URL for its mirror
identity and include the full oneline log. `--downgrade` reverses only the log
range; `--no-changelog` avoids mirrors entirely. Mirrors live in the existing
XDG/HOME `confctl/git-mirrors` cache. Ordinary cache availability errors and
complete native clone/fetch/log failures produce a visible changelog-unavailable
note and may still commit. Cancellation, spawn, ownership, drain, incomplete
capture and delivery failures stop before commit. Comparison commands retain
their stricter failure policy.

Git receives `commit [-e] -F FILE -- flake.lock` with an owned message file kept
until its child and terminal restoration finish. `-e` depends on editor=true and
actual stdout being a TTY, independently of live/plain presentation; redirected
stdout suppresses editing, not committing. All streams are inherited. Git owns
its editor, hooks, signing and index behavior. confctl neither stages unrelated
files nor initializes a repository. An untracked lock can fail natively.

Nix output is forwarded and captured. Only the recognized experimental-feature
failure retries once; a cached GitHub download warning fails even after native
exit 0. Nix can already have changed the lock when it fails. A later setter step,
fatal changelog error, hook/editor or commit failure retains completed lock and
cache effects. Commit failure prevents the normal success summary; cancellation
after commit may leave the commit in place. There is no automatic restoration,
retry or rollback. Inspect the reported phase, `flake.lock`, the invocation log
and Git state, then explicitly commit, retry or restore the chosen files.

Lock formats remain readable by the preceding Ruby implementation and Rust.
The public entrypoint now selects Rust. Confined fake-tool and local Git
fixtures do not qualify real Nix input acquisition or site configuration behavior.

## Caller environment

The packaged Rust executable does not start a Ruby interpreter or activate a
caller bundle. It preserves `RUBYOPT` and `BUNDLE_*` for child commands, including
Git hooks. A hook that invokes Ruby still uses its own interpreter and bundle
configuration. Git proof children separately disable replacement objects and
repository/object redirection without changing that caller environment.
