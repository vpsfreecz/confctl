{ pkgs }:
let
  renderer = import ./docgen.nix { inherit pkgs; };
in
renderer.renderMan {
  name = "kexec-netboot";
  src = ./modules/confctl/kexec-netboot/kexec-netboot.8.md;
  date = "2026-10-14";
  footer = "kexec-netboot";
  header = "kexec-netboot";
}
