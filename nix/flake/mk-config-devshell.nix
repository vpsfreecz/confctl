{ self, nixpkgs }:

# Configuration repositories compose their independent tools around this shell.
{
  system,
  pkgs ? nixpkgs.legacyPackages.${system},
  mode ? "minimal",
  implementation ? "rust",
  extraPackages ? [ ],
  legacyCompat ? false,
}:
let
  lib = pkgs.lib;
  confctlPackage = self.packages.${system}.confctl;
in
assert lib.assertMsg (
  implementation == "rust"
) "mkConfigDevShell only supports implementation 'rust'; remove the old Ruby selection";
assert lib.assertMsg (!legacyCompat)
  "mkConfigDevShell legacyCompat=true is no longer supported; compose repository tools in the configuration's own shell";
assert lib.assertMsg (mode != "tools")
  "mkConfigDevShell mode 'tools' is retired; use mode 'minimal', pass repository tools through extraPackages, and keep Gemfile/Bundler setup in the configuration's own shellHook";
assert lib.assertMsg (mode == "minimal")
  "mkConfigDevShell only supports mode 'minimal'; remove bundled-confctl and compose repository tools in the configuration's own shell";
pkgs.mkShell {
  packages =
    (with pkgs; [
      confctlPackage
      git
      ncurses
      man-db
      groff
      less
      nixfmt
      nixfmt-tree
    ])
    ++ extraPackages;

  shellHook = ''
    export CONFCTL_SRC="${self.outPath}"
    export PATH="${confctlPackage}/bin:$PATH"
    export MANPATH="${confctlPackage}/share/man:$(man --path)"
    export PS1="(confctl) $PS1"

    echo
    echo "confctl config dev shell ready"
  '';
}
