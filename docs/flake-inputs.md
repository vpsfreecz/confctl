# Flake inputs

confctl requires a configuration flake whose outputs use
`confctl.lib.mkConfctlOutputs`:

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

The development `confctl-rust` implements update and set at root, channel and
machine scope. These commands require a flake and run without confirmation.
They hold one `.confctl/mutation.lock` advisory lock through selection, Nix,
changelog preparation, Git hooks/editor and joined completion. Avoid concurrent
Ruby, external Nix or Git writers: they do not share this lock.

Root update preserves explicit names, order and duplicates. `--all` instead uses
the sorted root names in the existing lock and ignores positional names. An
explicit update may create a missing lock; set requires a lock and GitHub or Git
input metadata. Set prepares every override before its first child and restores
all selected original metadata in each published temporary result. The existing
first-node interpretation of `follows`, unknown JSON and URL/query fields remain
unchanged. These operations do not edit `flake.nix` or refresh generated lists.

Channel selectors preserve brace/comma order and duplicates; glob matches sort
by name. Underlying input names are deduplicated at their first occurrence.
Channel set refuses inputs shared outside the selected channel/role pairs unless
`--allow-shared` (also `--allow_shared`) is supplied, then prints the uses. Channel
update has no such restriction. Machine routes resolve one exact name or key
and role through configured Nix evaluation, without inventory builds or host
contact. Channel and machine summaries read the actual resulting lock.

Null/false channel mappings are empty; an absent, null or false explicitly
requested role input selects nothing. Empty strings remain literal input names.
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

Lock formats remain readable by Ruby and Rust, and Ruby remains the default
entry point. Confined fake-tool and local Git fixtures do not qualify real Nix
input acquisition or site configuration behavior.
