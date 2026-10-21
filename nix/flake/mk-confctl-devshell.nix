{ nixpkgs }:

{
  system,
  pkgs ? nixpkgs.legacyPackages.${system},
  extraPackages ? [ ],
}:
pkgs.mkShell {
  # Compile the audited no-read entry, preserving the multicall argv[0].
  CONFCTL_LOGNAME = "${pkgs.coreutils}/bin/logname";
  packages = [
    pkgs.git
    pkgs.ncurses
    pkgs.man-db
    pkgs.groff
    pkgs.less
    pkgs.nixfmt
    pkgs.nixfmt-tree
    pkgs.rustc
    pkgs.cargo
    pkgs.rustfmt
    pkgs.clippy
    pkgs.cmake
    pkgs.python3
    pkgs.nix
  ]
  ++ extraPackages;

  shellHook = ''
    export CONFCTL="$PWD"
    export PATH="$CONFCTL/bin:$PATH"
    export MANPATH="$CONFCTL/man:$(man --path)"

    export PS1="(confctl) $PS1"

    echo
    echo "confctl development shell ready"
  '';
}
