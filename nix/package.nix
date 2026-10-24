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
  ];
in
(pkgs.writeShellScriptBin "confctl" ''
  export GEM_HOME="${deps}/${ruby.gemPath}"
  export GEM_PATH="${deps}/${ruby.gemPath}"
  export RUBYLIB="${src}/lib"
  export PATH="${runtimePath}:$PATH"

  # Ignore caller preloads at startup while retaining their environment for hooks.
  exec ${ruby}/bin/ruby --disable=rubyopt ${src}/bin/confctl "$@"
'').overrideAttrs
  (_old: {
    passthru = {
      inherit deps ruby src;
    };
  })
