{
  pkgs,
  oracleSrc,
  nixpkgsSrc,
  count ? 1,
}:
assert builtins.elem count [
  1
  10
  100
  1000
];
let
  # Reuse one contextful spelling at every URL, fetch and lock boundary.
  oraclePath = "${oracleSrc}";
  nixpkgsPath = "${nixpkgsSrc}";
  oracleTree = builtins.fetchTree {
    type = "path";
    path = oraclePath;
  };
  nixpkgsTree = builtins.fetchTree {
    type = "path";
    path = nixpkgsPath;
  };
  lock = {
    version = 7;
    root = "root";
    nodes = {
      root.inputs = {
        confctl = "confctl";
        nixpkgs = "nixpkgs";
      };
      confctl = {
        flake = false;
        locked = {
          type = "path";
          path = oraclePath;
          narHash = oracleTree.narHash;
          lastModified = 1;
        };
        original = {
          type = "path";
          path = oraclePath;
        };
      };
      nixpkgs = {
        locked = {
          type = "path";
          path = nixpkgsPath;
          narHash = nixpkgsTree.narHash;
          lastModified = 1;
        };
        original = {
          type = "path";
          path = nixpkgsPath;
        };
      };
    };
  };
in
pkgs.runCommandLocal "confctl-real-local-config-${toString count}" { } ''
  mkdir -p "$out"
  cp -a ${./template}/. "$out/"
  chmod -R u+w "$out"
  substituteInPlace "$out/flake.nix" \
    --replace-fail '@CONFCTL_SOURCE@' '${oraclePath}' \
    --replace-fail '@NIXPKGS_SOURCE@' '${nixpkgsPath}'
  cp ${pkgs.writeText "fixture-flake.lock" (builtins.toJSON lock)} "$out/flake.lock"
  echo ${toString count} > "$out/count.nix"
''
