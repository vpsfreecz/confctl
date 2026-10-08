# confctl example configuration
There are five skeleton deployments:

* `nixos-machine` to deploy a standard NixOS machine
* `vpsadminos-machine` to deploy vpsAdminOS machine
* `vpsadminos-container` to deploy to a container running on vpsAdminOS
* `vpsfreecz-vps` to deploy to a VPS at [vpsFree.cz](https://vpsfree.org),
  which is the same as `vpsadminos-container`
* `nested/nixos-machine` to demonstrate a machine in a nested directory

The `confctl` input uses `path:..` to select this repository. When copying the
example elsewhere, change it to `github:vpsfreecz/confctl` or the intended local
source path before generating a lock. `shell.nix` is an optional wrapper for
confctl source development; use the flake shell for cluster operations.

## Usage

1. Enter `nix develop`:
```
nix develop
```

2. Update flake inputs:
```
confctl inputs update --all
```

3. Edit configurations in `cluster/` to your liking.

For physical machines, supply a `hardware.nix` file. `nixos-generate-config`
creates it during NixOS installation as `/etc/nixos/hardware-configuration.nix`.
The machine must already be installed and running: confctl deploys the new
system over SSH and activates it.

Set the target IP address with `host.target` in `cluster/*/module.nix`.

Configure SSH keys in `data/ssh-keys.nix`.

4. Build and deploy
```
confctl build vpsfreecz-vps
confctl deploy vpsfreecz-vps
```
