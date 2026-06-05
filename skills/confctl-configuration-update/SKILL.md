---
name: confctl-configuration-update
description: >-
  Upgrade confctl-managed flake configuration repositories from one
  NixOS/nixpkgs release to another, such as 25.11 to 26.05. Use only for NixOS
  release upgrades that require reading target release notes, moving nixpkgs
  release channels, updating release-coupled inputs such as home-manager when
  needed, handling NixOS release deprecations, and validating machines with
  confctl build. Do not use for routine flake input bumps, service updates,
  dependency refreshes, or ordinary staging/production rollouts that do not
  change the NixOS/nixpkgs release.
---

# confctl NixOS Release Upgrade

## Overview

Use this skill only for configuration repositories built by `confctl` when
upgrading machines from one NixOS/nixpkgs release to another. The goal is not
only to move release inputs, but to produce a reviewable, deployable release
port: release notes understood, generated input commits isolated, machines
built or explicitly blocked, warnings fixed, and compatibility implications
recorded.

Do not use this skill for routine flake input bumps, ordinary service updates,
dependency refreshes, or staging/production rollouts that keep the same NixOS
release.

## Rules

- Read local repository instructions before editing. Do not assume a specific
  organization, directory layout, branch naming scheme, or tracking-file
  convention.
- Use the repository's documented development environment. If it provides a
  Nix shell, prefer `nix develop` before running `confctl`.
- Use `confctl` commands for flake input revision changes. Do not edit
  `flake.lock` manually.
- Let `confctl` create generated input commits with `--commit`; use
  `--no-editor` in non-interactive runs.
- Use `--no-changelog` for noisy inputs such as `nixpkgs`, `home-manager`, or
  other large upstreams when they are moved as part of the release upgrade.
  Keep changelogs for smaller controlled repositories when the log is useful.
- Treat Nix evaluation warnings and deprecation notices as work items. Fix
  them before calling the port done unless the user explicitly accepts a
  documented deferral.
- Try to build all existing machines. If local secrets, ISO images, private
  paths, or other operator-only inputs block the sweep, record the exact
  target and path, then continue with representative builds.

## Preparation

Before changing release inputs, identify and record the upgrade plan in
whatever place the project uses: an issue, PR description, local notes, or the
conversation. Include:

- source and target NixOS/nixpkgs releases;
- affected configuration repository and branch;
- intended channel order, for example shared stable hosts first, then staging,
  then production;
- compatibility checks for persisted state, database schemas, service APIs,
  generated configs, rollback, and mixed-version operation.

Read current release material from official NixOS sources. Do not rely on
memory for current release notes:

- NixOS release announcement;
- NixOS release notes for the target release;
- relevant nixpkgs, NixOS module, or service documentation linked from the
  notes.

Extract a short checklist of likely issues: removed options, renamed packages
or aliases, default flips, required option changes, service module removals,
systemd behavior changes, filesystem changes, compiler/runtime changes, and
rollback-sensitive state changes.

## Inventory

From the configuration repository root:

```shell
nix develop
confctl inputs ls
confctl inputs channel ls
confctl ls
rg -n 'nixos-[0-9][0-9]\.[0-9][0-9]|release-[0-9][0-9]\.[0-9][0-9]|nixpkgs' \
  flake.nix cluster configs modules overlays environments packages
```

If the repository does not use `nix develop`, use its documented shell or run
the same `confctl` commands with `confctl` available on `PATH`. Omit inventory
paths that do not exist in the target repository.

Map each release-related channel to the machines it affects. Common channel
roles include:

- `nixpkgs`: the nixpkgs input used by machines in a channel;
- `home-manager`: a release branch that often follows the same nixpkgs
  release;
- `confctl`: the tool version used by the configuration shell, if the current
  tool cannot evaluate or build the target release;
- service-specific roles: application, module, package, or operating-system
  inputs consumed by a subset of machines and coupled to the NixOS release;
- environment channels: names such as `stable`, `staging`, `production`, or
  project-specific equivalents.

Decide whether each channel should move now or remain a separate rollout. Do
not assume every environment channel must move together.

## Updating Inputs

Use `confctl` from the configuration repo's normal tool environment. Update
only inputs that are part of the release upgrade or are required to make the
target release evaluate. Common patterns:

```shell
nix develop -c confctl inputs channel update \
  --commit --no-changelog --no-editor stable nixpkgs

nix develop -c confctl inputs channel update \
  --commit --no-changelog --no-editor home-manager home-manager

nix develop -c confctl inputs channel set \
  --commit --no-changelog --no-editor '{production,staging}' nixpkgs <rev>

nix develop -c confctl inputs channel set \
  --commit --no-editor <channel> <role> <rev>

# Only when the current confctl input cannot handle the target release:
nix develop -c confctl inputs update \
  --commit --no-changelog --no-editor confctl

# Only when pinning a specific confctl revision for the release upgrade:
nix develop -c confctl inputs set \
  --commit --no-changelog --no-editor confctl <rev>
```

Use `channel update` when following the input's configured target release
branch/ref. Use `channel set` or `inputs set` for an exact revision, especially
when pinning an unmerged release-port branch or a known branch tip.

If a channel selector touches an input that is also used by another channel,
`confctl` can require `--allow-shared`. Only pass it after confirming the
shared move is intentional, and record the affected channels wherever the
project tracks validation.

Current `confctl` `set`/`update` commands own `flake.lock` revision changes
and commit only `flake.lock`. If a release port also needs `flake.nix` URL
refs changed, such as `github:NixOS/nixpkgs/nixos-25.11` to `nixos-26.05`,
treat that as a separate configuration edit. Never hand-edit `flake.lock`.

After each generated commit:

```shell
git show --stat --format=fuller HEAD
nix develop -c confctl inputs channel ls '<affected-channel-pattern>'
```

Keep generated `confctl` commit messages in their generated form unless
repository-local rules explicitly say to edit them.

## Build And Warning Loop

Start with a focused sample if the release upgrade is broad, then attempt the
full fleet:

```shell
nix develop -c confctl build -y '<critical-pattern>'
nix develop -c confctl build -y
```

Choose representative targets that cover every release-affected input and
machine type. Examples include one machine per environment channel, one
machine per custom nixpkgs input, critical infrastructure hosts, service hosts
that use release-coupled application inputs, and machines with unusual local
hardware or filesystem configuration.

Watch evaluation output and `confctl` logs for warnings:

```shell
rg -n -i 'warning:|deprecated|deprecat|renamed|obsolete|removed|will be removed' \
  .confctl/logs
```

Fix warnings and evaluation failures in small logical commits. Common fixes
include:

- removed NixOS option: stop reading or setting it, or gate the common path;
- renamed package/alias: use the new package name;
- required filesystem option: set an explicit `fsType`;
- stale custom nixpkgs fork: replace the missing behavior locally, then remove
  the unused input;
- package wrapper regression: add a local overlay/patch and verify the built
  wrapper;
- local-only build input missing: record the path and target, then validate
  neighboring machines that do not require it.

If the full build is blocked, keep reducing to direct targets until every
category touched by the release upgrade has either built or has a documented
external blocker. Record generation IDs, log paths, and blocked targets in the
project's normal validation notes.

## Compatibility Fix Commits

Keep release input bumps and functional fixes separate.

Recommended ordering:

1. Generated release-input commits.
2. Configuration fixes required by evaluation/build failures.
3. Warning/deprecation cleanup.
4. Removal of obsolete inputs after no machines use them.
5. Follow-up service-specific release fixes discovered by targeted builds.

For manual commits, use the repository's required commit workflow. Explain what
broke, why the new release requires the change, and what hosts/services are
affected. Do not put command transcripts or test logs in commit messages;
record validation in the project's normal notes or PR text.

## Completion

Before finishing:

- run repository formatting/hooks required by local rules;
- run `git status --short --branch`;
- record commands, validation results, warnings fixed, blockers, commit
  hashes, and cleanup notes in the project-appropriate place;
- push the branch if requested or if that is the normal project flow;
- check CI when the repository has branch workflows.
