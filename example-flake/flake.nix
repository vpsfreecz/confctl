{
  description = "confctl example (flake)";

  inputs = {
    confctl.url = "path:..";
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    inputs@{ self, confctl, ... }:
    let
      confctlOutputs = confctl.lib.mkConfctlOutputs {
        confDir = ./.;
        inputs = inputs;
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
