{
  pkgs,
  goModules ? [ ],
}:
let
  lib = pkgs.lib;
  directories = map (entry: entry.directory) goModules;
  storeOutput =
    value: builtins.match "/nix/store/[a-z0-9]{32}-[A-Za-z0-9+._-]+" (toString value) != null;
  storeSource =
    value:
    builtins.match "/nix/store/[a-z0-9]{32}-[A-Za-z0-9+._-]+(/.*)?" (toString value) != null
    && builtins.all (part: part != "." && part != "..") (lib.splitString "/" (toString value));
  valid =
    entry:
    builtins.attrNames entry == [
      "directory"
      "proxy"
      "source"
    ]
    && builtins.elem entry.directory [
      "."
      "extensions"
    ]
    && storeSource entry.source
    && (entry.proxy == null || storeOutput entry.proxy);
  file =
    source: name:
    let
      path = source + "/${name}";
      present = builtins.pathExists path;
    in
    {
      inherit present;
      sha256 = if present then builtins.hashFile "sha256" path else null;
    };
  modules = map (entry: {
    inherit (entry) directory;
    goMod = file entry.source "go.mod";
    goSum = file entry.source "go.sum";
    proxy = if entry.proxy == null then null else toString entry.proxy;
  }) goModules;
  validateModule = entry: ''
    cd ${lib.escapeShellArg (toString entry.source)}
    test -f go.mod && test ! -L go.mod
    if test -e go.sum || test -L go.sum; then test -f go.sum && test ! -L go.sum; fi
    ${pkgs.go}/bin/go mod edit -json > "$TMPDIR/module.json"
    ${pkgs.jq}/bin/jq -e 'all(.Replace[]?; .New.Version != null and .New.Version != "")' \
      "$TMPDIR/module.json" > /dev/null || {
      echo 'Hook module ${entry.directory} has a filesystem-local replace' >&2
      exit 1
    }
    ${lib.optionalString (entry.proxy == null) ''
      ${pkgs.jq}/bin/jq -e '(.Require // [] | length) == 0' "$TMPDIR/module.json" > /dev/null || {
        echo 'Hook module ${entry.directory} requires a declared file proxy' >&2
        exit 1
      }
    ''}
  '';
in
assert lib.assertMsg (builtins.all valid goModules)
  "hookGoModules accepts only immutable { directory; source; proxy; } entries for . or extensions";
assert lib.assertMsg (
  builtins.length (lib.unique directories) == builtins.length directories
) "duplicate hookGoModules directory";
pkgs.runCommand "confctl-hook-go-tools"
  {
    moduleManifest = builtins.toJSON { inherit modules; };
    passAsFile = [ "moduleManifest" ];
    passthru = { inherit goModules; };
  }
  ''
    export HOME="$TMPDIR/home" GOCACHE="$TMPDIR/cache" GOPATH="$TMPDIR/gopath"
    export GOTOOLCHAIN=local GOPROXY=off GONOPROXY=none GOPRIVATE= GOSUMDB=off GOWORK=off GOENV=off GOFLAGS=
    export GOMODCACHE="$TMPDIR/modcache"
    mkdir -p "$HOME"
    ${lib.concatMapStringsSep "\n" validateModule goModules}
    mkdir -p "$out/bin" "$out/share/confctl-hooks"
    cp "$moduleManifestPath" "$out/share/confctl-hooks/go-modules.json"
    cat > "$out/bin/gofmt" <<'WRAPPER'
    #!${pkgs.runtimeShell}
    exec ${pkgs.go}/bin/gofmt "$@"
    WRAPPER
    cat > "$out/bin/go" <<'WRAPPER'
    #!${pkgs.runtimeShell}
    exec ${pkgs.go}/bin/go "$@"
    WRAPPER
    chmod 755 "$out/bin/gofmt" "$out/bin/go"
  ''
