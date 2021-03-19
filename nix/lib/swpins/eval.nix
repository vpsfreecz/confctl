{ confDir, name, channels, pkgs, lib }:
let
  clusterFileName = builtins.replaceStrings [ "/" ] [ ":" ] name;

  clusterFilePath = "${toString confDir}/swpins/cluster/${clusterFileName}.json";

  clusterFileJson = builtins.readFile clusterFilePath;

  clusterFileSpecs =
    if builtins.pathExists clusterFilePath then
      builtins.fromJSON clusterFileJson
    else
      {};

  clusterFileSwpins = lib.mapAttrs (k: v: swpin v) clusterFileSpecs;

  channelPath = chan: "${toString confDir}/swpins/channels/${chan}.json";

  channelJson = chan: builtins.readFile (channelPath chan);

  channelSpecs = chan: builtins.fromJSON (channelJson chan);

  channelSwpins = chan: lib.mapAttrs (k: v: swpin v) (channelSpecs chan);

  allChannelSwpins = map channelSwpins channels;

  allSwpins = (lib.foldl (a: b: a // b) {} allChannelSwpins) // clusterFileSwpins;

  swpin = { fetcher, ... }:
    fetchers.${fetcher.type} fetcher.options;

  fetchers = rec {
    git = opts:
      let
        filter = lib.filterAttrs (k: v: builtins.elem k [
          "url" "rev" "sha256" "fetchSubmodules"
        ]);
      in pkgs.fetchgit (filter opts);

    zip = opts:
      pkgs.fetchzip opts;

    git-rev = opts:
      let
        repo = fetchers.${opts.wrapped_fetcher.type} opts.wrapped_fetcher.options;
        shortRev = lib.substring 0 7 (opts.rev);
      in
        pkgs.runCommand "git-${shortRev}" {} ''
          cp -a ${repo} $out
          chmod 700 $out
          echo "${shortRev}" > $out/.git-revision
          echo ".git.${shortRev}" > $out/.version-suffix
        '';
  };

  clusterFileInfos = lib.mapAttrs (k: v: v.info or {}) clusterFileSpecs;

  channelInfos = chan: lib.mapAttrs (k: v: v.info or {}) (channelSpecs chan);

  allChannelInfos = map channelInfos channels;

  allInfos = (lib.foldl (a: b: a // b) {} allChannelInfos) // clusterFileInfos;

in {
  evaluated = allSwpins;

  infos = allInfos;
}
