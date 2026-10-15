# Test-only substitution inside the unchanged original package function. The
# original launcher prepends its tools, so an outer PATH override is insufficient.
{
  pkgs,
  oracleSrc,
  fixtureTools,
}:
let
  fixturePkgs = pkgs // {
    git = fixtureTools;
    openssh = fixtureTools;
    nix = fixtureTools;
  };
in
import (oracleSrc + "/nix/package.nix") {
  pkgs = fixturePkgs;
  src = oracleSrc;
}
