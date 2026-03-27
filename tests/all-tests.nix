{
  pkgs ? <nixpkgs>,
  system ? builtins.currentSystem,
  suiteArgs ? { },
  testConfig ? { },
  configuration ? null,
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
      testConfig
      configuration
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
