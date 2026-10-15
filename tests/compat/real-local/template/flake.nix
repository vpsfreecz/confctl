{
  inputs.confctl = {
    url = "path:@CONFCTL_SOURCE@";
    flake = false;
  };
  inputs.nixpkgs.url = "path:@NIXPKGS_SOURCE@";
  outputs =
    inputs@{ self, ... }:
    {
      confctl = (import (inputs.confctl + "/nix/flake/mk-confctl-outputs.nix")) {
        confDir = ./.;
        inherit inputs;
        system = "x86_64-linux";
        channels.fixture = {
          nixpkgs = "nixpkgs";
        };
      };
    };
}
