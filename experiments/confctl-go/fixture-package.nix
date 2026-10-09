{ pkgs, fixtureTools }:
import ./package.nix {
  pkgs = pkgs // {
    git = fixtureTools;
    openssh = fixtureTools;
    nix = fixtureTools;
  };
}
