{ confDir, name, pkgs, lib }:
let
  json = builtins.readFile ("${toString confDir}/swpins/files/${name}.json");

  sources = builtins.fromJSON json;

  swpins = lib.mapAttrs (k: v: swpin v) sources;

  swpin = { fetcher, fetcher_options, handler ? null, ... }:
    if handler == null then
      handlers.${fetcher} fetcher_options
    else
      handlers.${handler} handlers.${fetcher} fetcher_options;

  handlers = rec {
    git = opts:
      let
        filter = lib.filterAttrs (k: v: builtins.elem k [
          "url" "rev" "sha256" "fetchSubmodules"
        ]);
      in pkgs.fetchgit (filter opts);

    zip = opts:
      pkgs.fetchzip opts;

    vpsadminos = fetcher: opts:
      let
        repo = fetcher opts;
        shortRev = lib.substring 0 7 (opts.rev);
      in
        pkgs.runCommand "os-version-suffix" {} ''
          cp -a ${repo} $out
          chmod 700 $out
          echo "${shortRev}" > $out/.git-revision
          echo ".git.${shortRev}" > $out/.version-suffix
        '';
  };
in swpins
