{
  pkgs ? <nixpkgs>,
  system ? builtins.currentSystem,
  suiteArgs ? { },
}:
let
  vpsadminosPath = suiteArgs.vpsadminosPath or (throw "suiteArgs.vpsadminosPath is required");
  nixpkgs = import pkgs { inherit system; };
  lib = nixpkgs.lib;
  testLib = import (vpsadminosPath + "/test-runner/nix/lib.nix") {
    inherit
      pkgs
      system
      lib
      suiteArgs
      ;
    suitePath = ./suite;
  };
in
testLib.makeTests [
  "carrier/deploy"
  "carrier/netboot"
  "deploy/flakes"
  "deploy/swpins"
  "auto_rollback"
]
