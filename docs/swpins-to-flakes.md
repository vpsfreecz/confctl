# Migrating from swpins to flakes

This guide explains how to convert an existing **confctl configuration repository** from the legacy *swpins* workflow (pins and channels defined in `configs/swpins.nix`, state in `swpins/*.json`, updates via `confctl swpins ...`) to a **flake-based** workflow (pins are normal flake inputs locked in `flake.lock`, updates via `confctl inputs ...`).

After the migration:

- your repository has `flake.nix` and `flake.lock`
- flake inputs replace swpins definitions
- machine metadata selects channels via `cluster.<name>.inputs.channels`
- per-machine pin differences are expressed via `cluster.<name>.inputs.overrides`
- you no longer need `configs/swpins.nix` or the generated `swpins/` directory

If you are new to the flake model in confctl, read `docs/flake-inputs.md` after this guide.

---

## Before you start

- Do the migration on a branch.
- Make sure your Nix has flakes enabled (`nix-command` + `flakes`).
- Your configuration repo layout is assumed to be the usual confctl layout with `cluster/`, `configs/`, etc.

---

## Automated migration (recommended)

confctl includes an interactive migration helper that performs the steps in this guide:

```bash
confctl migrate swpins-to-flakes --dry-run
confctl migrate swpins-to-flakes --yes
```

You can also run individual steps (`flake`, `machines`, `imports`, `clean`); see `confctl migrate swpins-to-flakes --help`.

## Step 1: Add `flake.nix`

Create `flake.nix` in the root of your configuration repository.

A minimal, practical skeleton looks like this:

```nix
{
  description = "my cluster config (confctl flake)";

  inputs = {
    # confctl itself
    confctl.url = "github:vpsfreecz/confctl";

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
