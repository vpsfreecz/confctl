{
  description = "confctl";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    {
      lib.mkConfctlOutputs = import ./nix/flake/mk-confctl-outputs.nix;

      nixosModules = {
        generations = import ./nix/modules/confctl/generations.nix;
        cli = import ./nix/modules/confctl/cli.nix;
        nix = import ./nix/modules/confctl/nix.nix;
        overlays = import ./nix/modules/confctl/overlays.nix;
        swpins = import ./nix/modules/confctl/swpins.nix;
        pins-info = import ./nix/modules/confctl/pins-info.nix;
        default = {
          imports = [
            (import ./nix/modules/confctl/generations.nix)
            (import ./nix/modules/confctl/cli.nix)
            (import ./nix/modules/confctl/nix.nix)
            (import ./nix/modules/confctl/overlays.nix)
            (import ./nix/modules/confctl/swpins.nix)
            (import ./nix/modules/confctl/pins-info.nix)
          ];
        };
      };
    };
}
