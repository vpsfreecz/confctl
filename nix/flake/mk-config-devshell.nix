{ self, nixpkgs }:

{
  system,
  pkgs ? nixpkgs.legacyPackages.${system},
  mode ? "minimal",
  extraPackages ? [ ],
  legacyCompat ? false,
  hookGoModules ? [ ],
}:
let
  confctl = self.packages.${system}.confctl;
  devTools = import ../dev-tools.nix {
    inherit pkgs;
    checker = self.packages.${system}.confctl-hook-checker;
    goModules = hookGoModules;
  };
in
assert builtins.elem mode [
  "minimal"
  "tools"
  "bundled-confctl"
];
pkgs.mkShell {
  packages = [
    confctl
    pkgs.git
    pkgs.openssh
    pkgs.nix
    pkgs.man-db
    pkgs.less
  ]
  ++ pkgs.lib.optionals (mode != "minimal") [
    devTools
    pkgs.go
    pkgs.nixfmt
  ]
  ++ extraPackages;

  # Keep the old mode names and legacyCompat argument accepted. Executable
  # extensions and the packaged Go CLI replace the old bundled Ruby source API.
  shellHook = ''
    export CONFCTL_SRC="${self.outPath}"
    export MANPATH="${confctl}/share/man:''${MANPATH:-}"
    export PS1="(confctl) $PS1"
  '';
}
