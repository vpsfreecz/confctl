{
  pkgs,
  src ? ../.,
}:

let
  package = import ./package.nix { inherit pkgs src; };
  hashes = import ./go-dependency-hashes.nix { inherit (pkgs) lib; };
in
(package.overrideAttrs (_: {
  proxyVendor = true;
  vendorHash = hashes.proxyHash;
})).goModules
