{
  pkgs ? <nixpkgs>,
  system ? builtins.currentSystem,
  suiteArgs ? { },
  testConfig ? { },
  configuration ? null,
  testFramework,
}:
let
  nixpkgs = import pkgs { inherit system; };
  lib = nixpkgs.lib;
  testLib = testFramework.makeTestLib {
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
