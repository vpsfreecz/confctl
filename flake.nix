{
  description = "confctl";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system);

      mkDevShell = import ./nix/flake/mk-devshell.nix { inherit self nixpkgs; };
    in
    {
      lib.mkConfctlOutputs = import ./nix/flake/mk-confctl-outputs.nix;

      # Reusable dev shell for cluster configuration repos (vpsfconf etc.)
      lib.mkDevShell = mkDevShell;

      devShells = forAllSystems (system: {
        default = mkDevShell { inherit system; };
      });

      nixosModules = {
        generations = import ./nix/modules/confctl/generations.nix;
        cli = import ./nix/modules/confctl/cli.nix;
        nix = import ./nix/modules/confctl/nix.nix;
        swpins = import ./nix/modules/confctl/swpins.nix;
        inputs-info = import ./nix/modules/confctl/inputs-info.nix;
        default = {
          imports = [
            (import ./nix/modules/confctl/generations.nix)
            (import ./nix/modules/confctl/cli.nix)
            (import ./nix/modules/confctl/nix.nix)
            (import ./nix/modules/confctl/inputs-info.nix)
          ];
        };
      };
    };
}
