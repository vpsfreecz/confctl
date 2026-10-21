let
  pkgs = import <nixpkgs> { };
  shell = import ./nix/flake/mk-confctl-devshell.nix { nixpkgs = { }; } {
    inherit pkgs;
    system = pkgs.stdenv.hostPlatform.system;
  };
in
shell.overrideAttrs (old: {
  shellHook = old.shellHook + ''
    export CONFCTL="${toString ./.}"
    export PATH="$CONFCTL/bin:$PATH"
    export MANPATH="$CONFCTL/man:$(man --path)"
  '';
})
