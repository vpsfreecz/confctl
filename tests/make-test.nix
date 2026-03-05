testFn:
{ vpsadminosPath, ... }@args:
let
  upstream = import (vpsadminosPath + "/tests/make-test.nix") testFn;
  mergedExtraArgs = {
    vpsadminos = vpsadminosPath;
    confctlSrc = args.confctlSrc or null;
    confctlPackage = args.confctlPackage or null;
  }
  // (args.extraArgs or { });
  argsWithExtra = args // {
    extraArgs = mergedExtraArgs;
  };
in
upstream argsWithExtra
