testFn:
{ testFramework, ... }@args:
let
  upstream = testFramework.makeTest testFn;
  mergedExtraArgs = {
    vpsadminos = testFramework.sourcePath;
    confctlSrc = args.confctlSrc or null;
    confctlPackage = args.confctlPackage or null;
  }
  // (args.extraArgs or { });
  argsWithExtra = args // {
    extraArgs = mergedExtraArgs;
  };
in
upstream argsWithExtra
