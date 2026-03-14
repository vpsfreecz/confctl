{ self, nixpkgs }:

# mkConfigDevShell builds a dev shell intended for cluster configuration repos
# that use confctl as a flake input.
#
# Modes:
# - minimal: confctl comes from the flake package, no Bundler bootstrap
# - tools: repo-local Bundler environment for repo tooling, confctl stays packaged
# - bundled-confctl: repo-local Bundler environment and confctl runs via bundle exec

{
  system,
  pkgs ? nixpkgs.legacyPackages.${system},
  mode ? "minimal",
  extraPackages ? [ ],
  legacyCompat ? false,
}:
let
  lib = pkgs.lib;
  confctlPackage = self.packages.${system}.confctl;
  bundlerMode = builtins.elem mode [
    "tools"
    "bundled-confctl"
  ];

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
    else if mode == "tools" then
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
      ''
    else
      ''
        if [ ! -f "$PWD/Gemfile" ]; then
          ${
            if legacyCompat then
              ''
                  mkdir -p "$PWD/.confctl-shell"

                  cat > "$PWD/.confctl-shell/Gemfile" <<EOF
                source "https://rubygems.org"
                gem "confctl", path: "$CONFCTL_SRC"
                EOF

                  export BUNDLE_GEMFILE="$PWD/.confctl-shell/Gemfile"
                  export BUNDLE_APP_CONFIG="$PWD/.confctl-shell/.bundle"
                  mkdir -p "$BUNDLE_APP_CONFIG"
              ''
            else
              fail "mkConfigDevShell mode 'bundled-confctl' requires ./Gemfile containing `gem \"confctl\"`"
          }
        fi

        ${bundlerBootstrap}

        export PATH="$PWD/.bin:$GEM_HOME/bin:${confctlPackage}/bin:$PATH"

        "$bundle_bin" config set --local "local.confctl" "$CONFCTL_SRC" >/dev/null

        NIX_ENFORCE_PURITY=0 "$bundle_bin" install >/dev/null

        if ! "$bundle_bin" info confctl >/dev/null 2>&1; then
          ${fail "mkConfigDevShell mode 'bundled-confctl' requires ./Gemfile to include `gem \"confctl\"`"}
        fi

        if "$bundle_bin" info rubocop >/dev/null 2>&1; then
          "$bundle_bin" binstubs rubocop --force >/dev/null
        fi

        cat > "$PWD/.bin/confctl" <<'EOF'
        #!/usr/bin/env bash
        bundle_bin="$GEM_HOME/bin/bundle"
        exec "$bundle_bin" exec confctl "$@"
        EOF

        chmod +x "$PWD/.bin/confctl"
      '';
in
assert builtins.elem mode [
  "minimal"
  "tools"
  "bundled-confctl"
];
pkgs.mkShell {
  packages =
    (with pkgs; [
      confctlPackage
      git
      ncurses
      man-db
      groff
      less
      nix-prefetch-git
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

    export MANPATH="$CONFCTL_SRC/man:$(man --path)"
    export PS1="(confctl) $PS1"

    echo
    echo "confctl config dev shell ready (${mode})"
  '';
}
