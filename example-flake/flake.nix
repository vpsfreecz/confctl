{
  description = "confctl example (flake)";

  inputs = {
    confctl.url = "path:..";
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    inputs@{ self, confctl, ... }:
    {
      confctl = confctl.lib.mkConfctlOutputs {
        confDir = ./.;
        inputs = inputs;
      };
    };
}
