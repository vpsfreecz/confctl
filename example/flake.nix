{
  description = "confctl example (flake)";

  inputs = {
    confctl.url = "path:..";
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    vpsadminos.url = "github:vpsfreecz/vpsadminos/staging";
  };

  outputs =
    inputs@{ self, confctl, ... }:
    let
      channels = {
        nixos = {
          nixpkgs = "nixpkgs";
        };
        vpsadminos = {
          nixpkgs = "nixpkgs";
          vpsadminos = "vpsadminos";
        };
      };

      confctlOutputs = confctl.lib.mkConfctlOutputs {
        confDir = ./.;
        inherit inputs channels;
      };
    in
    {
      confctl = confctlOutputs;

      devShells.x86_64-linux.default = confctl.lib.mkConfigDevShell {
        system = "x86_64-linux";
        mode = "minimal";
      };
    };
}
