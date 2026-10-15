{ self, nixpkgs }:

{
  system,
  pkgs ? nixpkgs.legacyPackages.${system},
  extraPackages ? [ ],
}:
let
  confctl = self.packages.${system}.confctl;
  devTools = self.packages.${system}.confctl-dev-tools;
in
pkgs.mkShell {
  packages = [
    confctl
    devTools
    pkgs.go
    pkgs.git
    pkgs.openssh
    pkgs.nix
    pkgs.nixfmt
    pkgs.man-db
    pkgs.less
  ]
  ++ extraPackages;

  shellHook = ''
    export CONFCTL="$PWD"
    export MANPATH="${confctl}/share/man:''${MANPATH:-}"
    export PS1="(confctl) $PS1"
  '';
}
