# Upgrading software-pin configurations with confctl v3

Use confctl v3 to convert a software-pin configuration to flakes before upgrading
to the flake-only tool. Software pins and channels are defined in
`configs/swpins.nix`, with state in `swpins/*.json` and updates through
`confctl swpins ...`. After conversion, dependencies are flake inputs locked in
`flake.lock` and managed through `confctl inputs ...`.

After the migration:

- your repository has `flake.nix` and `flake.lock`
- flake inputs replace swpins definitions
- machine metadata selects channels via `cluster.<name>.inputs.channels`
- per-machine pin differences are expressed via `cluster.<name>.inputs.overrides`
- you no longer need `configs/swpins.nix` or the generated `swpins/` directory

See the [flake input guide](flake-inputs.md) for channels and input management.

---

## Before you start

This guide applies to a software-pin configuration managed with confctl v3,
upgrading to a confctl version that supports only flakes. Perform migration
before updating the tool or configuration's confctl input. The new executable
has no `swpins`, `migrate`, `init --swpins` or `init --legacy` commands.

- Work on a branch and retain the previous configuration revision and pin files.
- Keep a runnable v3 binary or source and its Nix/Bundler environment. The name
  `confctl-v3` below refers to that retained executable; it is not a command
  provided by the new tool. Other `confctl` commands in steps 1 through 8 also use v3.
- Enable Nix's `nix-command` and `flakes` features.
- Preserve local generations and their GC roots, and required remote profiles.
  JSON backups do not preserve store closures.
- Review custom Ruby scripts and imported confctl APIs for removed pin constants,
  the old backend/cache API, `nixosModules.swpins`, and pin options. External
  netboot-index readers must accept input metadata instead of pin metadata.


---

## Automated migration (recommended)

confctl v3 includes the interactive helper for this migration:

```bash
confctl-v3 migrate swpins-to-flakes --dry-run
confctl-v3 migrate swpins-to-flakes --yes
```

You can also run individual steps (`flake`, `machines`, `imports`, `clean`); see `confctl-v3 migrate swpins-to-flakes --help`.

## Step 1: Add `flake.nix`

Create `flake.nix` in the root of your configuration repository.

A minimal, practical skeleton looks like this:

```nix
{
  description = "my cluster config (confctl flake)";

  inputs = {
    # confctl itself
    # Keep the current v3 input revision during migration.
    confctl.url = "github:vpsfreecz/confctl/<retained-v3-revision>";

    # mkConfctlOutputs needs an input named `nixpkgs` for evaluation.
    # If you want a specific nixpkgs for that purpose, pin it here.
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    # Add your real pinned inputs here (examples):
    # nixpkgsStable.url = "github:NixOS/nixpkgs/nixos-25.11";
    # vpsadminosStaging.url = "github:vpsfreecz/vpsadminos/staging";
  };

  outputs = inputs@{ self, confctl, ... }:
    let
      channels = {
        # Filled in Step 2
      };
    in
    {
      confctl = confctl.lib.mkConfctlOutputs {
        confDir = ./.;
        inherit inputs channels;
      };

      # Recommended: configuration-repo dev shell
      devShells.x86_64-linux.default = confctl.lib.mkConfigDevShell {
        system = "x86_64-linux";
        mode = "minimal";
      };
    };
}
```

Then generate the lock file:

```bash
nix flake lock
```

Commit both `flake.nix` and `flake.lock`.

---

## Step 2: Convert `configs/swpins.nix` into flake inputs + `channels` mapping

Open your existing `configs/swpins.nix`. In swpins, you usually have a structure like:

- channels (e.g. `nixos-unstable`, `vpsadminos-staging`)
- roles inside channels (e.g. `nixpkgs`, `vpsadminos`, `vpsadmin`)
- for each role a pin spec (`type = "git-rev"; git-rev.url = ...; git-rev.update.ref = ...`)

In flakes, you split this into:

1) **flake inputs** (named however you like)
2) **channels mapping** from role → input name

### 2.1 Define flake inputs

For a typical swpins `git-rev` pin:

```nix
# swpins
# url = https://github.com/NixOS/nixpkgs
# update.ref = refs/heads/nixos-unstable
```

Use a flake URL:

```nix
inputs.nixpkgsUnstable.url = "github:NixOS/nixpkgs/nixos-unstable";
```

If you need an input for a specific branch/tag that doesn’t have a shorthand, you can use:

```nix
inputs.someRepo.url = "git+https://example.com/repo?ref=my-branch";
```

If the repository does **not** provide `flake.nix`, mark it as non-flake:

```nix
inputs.someRepo = {
  url = "github:example/someRepo/main";
  flake = false;
};
```

### 2.2 Create `channels` mapping

For every swpins channel, create a flake channel mapping of role → input name.

Example:

```nix
channels = {
  nixos-unstable = {
    nixpkgs = "nixpkgsUnstable";
  };

  os-staging = {
    nixpkgs = "nixpkgsUnstable";
    vpsadminos = "vpsadminosStaging";
  };
};
```

Notes:

- A **role** is what your machine configurations refer to (`inputs.nixpkgs`, `inputs.vpsadminos`, ...).
- An **input name** is a flake input key under `inputs = { ... }`.
- Machines can select multiple channels; later channels override roles from earlier ones.

---

## Step 3: Switch machine metadata from `swpins.channels` to `inputs.channels`

For every machine `cluster/**/module.nix`:

- replace `swpins.channels = [ ... ];` with `inputs.channels = [ ... ];`

Example:

```nix
{ config, ... }:
{
  cluster."my-machine" = {
    spin = "nixos";

    # Flake config: select channels from flake.nix
    inputs.channels = [ "nixos-unstable" "os-staging" ];

    host.target = "203.0.113.10";
  };
}
```

---

## Step 4: Convert per-machine `swpins.pins` to `inputs.overrides`

Legacy swpins allowed defining full pin specs per machine:

```nix
swpins.pins.nixpkgs = {
  type = "git-rev";
  git-rev = {
    url = "https://github.com/NixOS/nixpkgs";
    update.ref = "refs/heads/some-branch";
  };
};
```

In flakes you **do not** write fetch specs in machine metadata. Instead:

1) add a dedicated flake input in `flake.nix`
2) override the role → input mapping on the machine

Example `flake.nix`:

```nix
inputs.nixpkgsSomeBranch.url = "github:NixOS/nixpkgs/some-branch";
```

Example `cluster/<path>/module.nix`:

```nix
cluster."my-machine".inputs.overrides.nixpkgs = "nixpkgsSomeBranch";
```

`inputs.overrides` can also add machine-only roles:

```nix
cluster."my-machine".inputs.overrides.myTool = "myToolInput";
```

---

## Step 5: Fix Nix code that relied on `<...>` imports

If your configuration imports modules via NIX_PATH, for example:

```nix
imports = [ <vpsadminos/os/lib/nixos-container/vpsadminos.nix> ];
```

flake evaluation is typically pure and `<...>` may stop working. Prefer explicit imports using the confctl-provided `inputs` module argument:

```nix
{ inputs, ... }:
{
  imports = [
    (inputs.vpsadminos + "/os/lib/nixos-container/vpsadminos.nix")
  ];
}
```

Similarly:

- `<nixpkgs/...>` → `(inputs.nixpkgs + "/...")`
- `<vpsadmin/...>` → `(inputs.vpsadmin + "/...")`

### Accessing flake-exported modules (`nixosModules`)

If you need to import modules exported from a flake input (e.g. `nixosModules`), use `flakeInputs` and `inputsInfo` so the import follows the selected channel/override:

```nix
{ flakeInputs, inputsInfo, ... }:
let
  vpsadminInput = inputsInfo.vpsadmin.input;
in
{
  imports = [
    flakeInputs.${vpsadminInput}.nixosModules.someModule
  ];
}
```

---

## Step 6: Temporary compatibility mode (optional)

If you want to migrate in smaller steps, you can temporarily keep legacy `<...>` imports working during flake builds.

In `configs/confctl.nix`:

```nix
{ config, ... }:
{
  confctl.nix.impureEval = true;
  confctl.nix.legacyNixPath = true;

  # Optional: which roles should be exposed as <name> in NIX_PATH
  # confctl.nix.legacyNixPathMap = [ "nixpkgs" "vpsadminos" "vpsadmin" ];
}
```

Notes:

- `legacyNixPath` requires `impureEval = true`.
- If you build multiple machines that use different pinned inputs for the same role,
  `<nixpkgs>`-style imports can become ambiguous. Prefer migrating imports fully.

---

## Step 7: Verify builds

Enter the development shell and verify you can evaluate/build at least one machine:

```bash
nix develop
confctl ls
confctl build my-machine
```

---

## Step 8: Remove swpins files and switch update workflow

Once you have verified that flake-based builds work:

1) Remove legacy pin configuration/state from the repo:

- delete `configs/swpins.nix`
- delete the generated `swpins/` directory

2) Update your workflow/CI scripts:

- replace `confctl swpins update` / `confctl swpins channel update` with flake input updates

Common update commands:

```bash
confctl inputs ls
confctl inputs update --commit --all
confctl inputs channel update --commit production
confctl inputs machine update --commit my-machine nixpkgs
```

`flake.lock` is now the source of truth for pinned revisions.


## Switch to the flake-only tool

After reviewing the channel and override mappings and completing a representative
flake build with v3, select the new confctl tool and update the configuration's
confctl input together. The new input provides `confctl.moduleOptions`, which
`confctl ls -L` needs for custom metadata listing. Remove obsolete configured
pin options and update any affected user scripts before using the new tool.

Verify machine listing, input status, explicit generation selection and a
representative build. Running nodes can continue using their existing system
profiles while operator tooling changes. Deployment is a separate operation;
retain required remote profiles throughout the rollback window. Nodes without
input metadata show unknown inputs until a flake-built system is deployed.

## Saved generations and GC roots

The new tool accepts only records with explicit `mode: "flakes"` in
`.confctl/generations/<escaped-host>/<generation>/generation.json`. It retains
the existing flake schema, input links and root names; v3 can read new flake
generations too. Missing-mode, software-pin, unknown-mode and invalid records
are reported with path and reason and are excluded from usable local generations.

Excluded records, `*.swpin` links, old `.confctl/build` caches, pin state and
associated GC roots are left untouched. Roots live beneath
`/nix/var/nix/gcroots/per-user/<login>/confctl-<configuration-path-hash>`;
old root names include the host, generation and `swpin.<name>`, toplevel or
rollback suffixes. These roots may keep disk use above the new retention count.
Do not move or delete `.confctl` casually: roots point into it and their namespace
depends on the real configuration path.

If an existing `current` link targets an excluded, invalid or missing record,
the new tool leaves the link unchanged and refuses `current` selection and local
`old` removal or rotation. It does not substitute a supported generation.
Numeric local selectors fail whenever records have been excluded; select a
supported generation explicitly. A successful new build may update `current`
through the ordinary build path.

Use retained v3's local generation commands to remove an unwanted old generation
while v3 can still load it, or keep it until rollback is no longer required.
Broader cache/root cleanup needs a separate inventory; the new tool cannot remove
excluded records. Remote profile listing and removal remain available regardless
of which tool built them. Local roots do not protect remote copies; deleting
remote profiles or collecting remote garbage can remove needed closures.

## Rollback

Retain the previous configuration Git revision and lock, runnable v3 environment,
and required local and remote roots. To restore the operator tooling, select v3
and the previous configuration revision. To restore a pre-flake configuration,
restore its matching configuration and pin files too; v2 cannot use flake
configurations. New builds may have changed `current`, so select the required
old generation explicitly with v3.

Machine rollback uses retained system profiles and closures. Changing the tool
version cannot recover a closure that has already been removed or collected.
There is no automatic state conversion to reverse.
