{
  pkgs,
  src ? ../..,
}:
pkgs.runCommand "confctl-hook-policy-check"
  {
    nativeBuildInputs = [
      pkgs.buildPackages.go
      pkgs.buildPackages.nix
    ];
  }
  ''
    cp -R ${pkgs.lib.cleanSource src} source
    chmod -R u+w source
    cd source
    export HOME="$TMPDIR/home" GOCACHE="$TMPDIR/go-cache"
    export GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off
    mkdir -p "$HOME"
    go test ./tests/hooks -run '^TestNixPolicyMatchesFixedConfig$' -count=1
    touch "$out"
  ''
