# Flake inputs

confctl supports flake-based configuration repositories via `confctl.lib.mkConfctlOutputs`.

In flake configs:

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

Use this sparingly. The default model is: select channels, update channels.

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
