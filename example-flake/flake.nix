{
  description = "confctl example (flake)";

  inputs = {
    confctl.url = "path:..";
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    inputs@{ self, confctl, ... }:
    let
      channels = {
        nixos-unstable = {
          nixpkgs = "nixpkgs";
        };
        staging = {
          nixpkgs = "nixpkgs";
        };
        production = {
          nixpkgs = "nixpkgs";
        };
      };

      confctlOutputs = confctl.lib.mkConfctlOutputs {
        confDir = ./.;
        inherit inputs channels;
      };
    in
    {
      confctl = confctlOutputs // {
        settings = confctlOutputs.settings // {
          nix = confctlOutputs.settings.nix // {
            impureEval = true;
          };
        };
      };
    };
}
