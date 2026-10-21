{ self, nixpkgs }:

# mkConfigDevShell builds a dev shell intended for cluster configuration repos
# that use confctl as a flake input.
#
# Modes:
# - minimal: confctl comes from the flake package, no Bundler bootstrap
# - tools: repo-local Bundler environment for repo tooling, confctl stays packaged

{
  system,
  pkgs ? nixpkgs.legacyPackages.${system},
  mode ? "minimal",
  implementation ? "rust",
  extraPackages ? [ ],
  legacyCompat ? false,
}:
let
  lib = pkgs.lib;
  confctlPackage = self.packages.${system}.confctl;
  bundlerMode = mode == "tools";

  fail = message: ''
    echo ${lib.escapeShellArg message} >&2
    return 1 2>/dev/null || exit 1
  '';

  bundlerBootstrap = ''
    export GEM_ROOT="$PWD/.gems"
    export GEM_HOME="$GEM_ROOT/ruby/$(ruby -e 'require "rbconfig"; print RbConfig::CONFIG["ruby_version"]')"
    export GEM_PATH="$GEM_HOME"
    export BUNDLE_PATH="$GEM_ROOT"
    export BUNDLE_APP_CONFIG="''${BUNDLE_APP_CONFIG:-$PWD/.bundle}"
    export RUBOCOP_CACHE_ROOT="$PWD/.rubocop_cache"

    mkdir -p "$GEM_ROOT" "$GEM_HOME" "$PWD/.bin"
    rm -f "$PWD/.bin/bundle" "$PWD/.bin/bundler"

    bundler_version=""
    if [ -f "$PWD/Gemfile.lock" ]; then
      bundler_version="$(awk '/^BUNDLED WITH$/{getline; sub(/^[[:space:]]+/, "", $0); print; exit}' "$PWD/Gemfile.lock")"
    fi

    if [ -n "$bundler_version" ]; then
      export BUNDLER_VERSION="$bundler_version"
      if [ ! -x "$GEM_HOME/bin/bundle" ] \
         || ! gem list -i bundler -v "$bundler_version" >/dev/null 2>&1; then
        gem install --no-document bundler -v "$bundler_version" >/dev/null
      fi
    elif [ ! -x "$GEM_HOME/bin/bundle" ]; then
      gem install --no-document bundler >/dev/null
    fi
    bundle_bin="$GEM_HOME/bin/bundle"

    "$bundle_bin" config set --local path "$BUNDLE_PATH" >/dev/null
    "$bundle_bin" config set --local bin "$PWD/.bin" >/dev/null
  '';

  modeShellHook =
    if mode == "minimal" then
      ''
        export PATH="${confctlPackage}/bin:$PATH"
      ''
    else
      ''
        if [ ! -f "$PWD/Gemfile" ]; then
          ${fail "mkConfigDevShell mode 'tools' requires ./Gemfile"}
        fi

        ${bundlerBootstrap}

        export PATH="${confctlPackage}/bin:$PWD/.bin:$GEM_HOME/bin:$PATH"

        NIX_ENFORCE_PURITY=0 "$bundle_bin" install >/dev/null

        if "$bundle_bin" info rubocop >/dev/null 2>&1; then
          "$bundle_bin" binstubs rubocop --force >/dev/null
        fi
      '';

in
assert lib.assertMsg (
  implementation == "rust"
) "mkConfigDevShell only supports implementation 'rust'; remove the old Ruby selection";
assert lib.assertMsg (
  !legacyCompat
) "mkConfigDevShell legacyCompat=true is no longer supported; use mode 'tools' for repository gems";
assert lib.assertMsg
  (builtins.elem mode [
    "minimal"
    "tools"
  ])
  "mkConfigDevShell supports 'minimal' and 'tools'; replace 'bundled-confctl' with 'tools' and remove confctl from the Gemfile";
pkgs.mkShell {
  packages =
    (with pkgs; [
      confctlPackage
      git
      ncurses
      man-db
      groff
      less
      nixfmt
      nixfmt-tree
    ])
    ++ lib.optionals bundlerMode (
      with pkgs;
      [
        openssl
        ruby
      ]
    )
    ++ extraPackages;

  shellHook = ''
    export CONFCTL_SRC="${self.outPath}"

    ${modeShellHook}

    export MANPATH="${confctlPackage}/share/man:$(man --path)"
    export PS1="(confctl) $PS1"

    echo
    echo "confctl config dev shell ready (${mode})"
  '';
}
