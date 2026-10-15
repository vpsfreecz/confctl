{ lib, ... }:
let
  count = import ../count.nix;
  names = map (i: "lab/nodes/node" + lib.fixedWidthNumber 4 i) (lib.range 1 count);
in
{
  cluster = lib.genAttrs names (name: {
    spin = "nixos";
    managed = true;
    inputs.channels = [ "fixture" ];
    host = {
      name = builtins.baseNameOf name;
      location = "local";
      domain = "example.invalid";
      target = "127.0.0.1";
      port = 2222;
    };
    tags = [
      "fixture"
      "node"
    ];
    labels = {
      site = "lab";
      rack = "a";
      detail = { inherit name; };
    };
    netboot.enable = true;
    buildAttribute = [
      "system"
      "build"
      "confctlMeasurement"
    ];
  });
}
