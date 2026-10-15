# Default source-derived metadata gate: evaluation only, no system builds.
{
  oracleSrc,
  nixpkgsSrc,
  confDir ? ./template,
}:
let
  nixpkgs =
    ((import (nixpkgsSrc + "/flake.nix")).outputs {
      self = {
        outPath = nixpkgsSrc;
      };
    })
    // {
      outPath = nixpkgsSrc;
    };
  outputs = import (oracleSrc + "/nix/flake/mk-confctl-outputs.nix") {
    inherit confDir;
    inputs = {
      self.outPath = confDir;
      confctl = oracleSrc;
      inherit nixpkgs;
    };
    system = "x86_64-linux";
    channels.fixture.nixpkgs = "nixpkgs";
  };
in
{
  settings = outputs.settings;
  machineKeys = outputs.machineKeys;
  machines = outputs.machines;
  metadataDerivation = outputs.machinesJson.drvPath;
}
