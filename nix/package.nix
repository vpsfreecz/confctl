{
  pkgs,
  src ? ../.,
  ruby ? (if pkgs ? ruby_3_4 then pkgs.ruby_3_4 else pkgs.ruby),
  groups ? [ "default" ],
}:
let
  lib = pkgs.lib;

  deps = pkgs.bundlerEnv {
    name = "confctl-deps";
    inherit ruby;
    gemdir = src;
    lockfile = "${src}/Gemfile.lock";
    inherit groups;
  };

  runtimePath = lib.makeBinPath [
    pkgs.git
    pkgs.openssh
    pkgs.nix
    pkgs.nix-prefetch-git
  ];
in
(pkgs.writeShellScriptBin "confctl" ''
  export GEM_HOME="${deps}/${ruby.gemPath}"
  export GEM_PATH="${deps}/${ruby.gemPath}"
  export RUBYLIB="${src}/lib"
  export PATH="${runtimePath}:$PATH"

  exec ${ruby}/bin/ruby ${src}/bin/confctl "$@"
'').overrideAttrs
  (_old: {
    passthru = {
      inherit deps ruby src;
    };
  })
