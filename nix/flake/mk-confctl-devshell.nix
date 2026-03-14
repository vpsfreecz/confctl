{ nixpkgs }:

{
  system,
  pkgs ? nixpkgs.legacyPackages.${system},
  extraPackages ? [ ],
}:
let
  ruby = if pkgs ? ruby_3_4 then pkgs.ruby_3_4 else pkgs.ruby;
in
pkgs.mkShell {
  packages = [
    pkgs.git
    pkgs.ncurses
    pkgs.openssl
    ruby
    pkgs.man-db
    pkgs.groff
    pkgs.less
    pkgs.nix-prefetch-git
    pkgs.nixfmt
    pkgs.nixfmt-tree
  ]
  ++ extraPackages;

  shellHook = ''
    export CONFCTL="$PWD"
    export GEM_ROOT="$PWD/.gems"
    export GEM_HOME="$GEM_ROOT/ruby/$(ruby -e 'require "rbconfig"; print RbConfig::CONFIG["ruby_version"]')"
    export GEM_PATH="$GEM_HOME"
    export BUNDLE_PATH="$GEM_ROOT"
    export BUNDLE_APP_CONFIG="$PWD/.bundle"
    export RUBOCOP_CACHE_ROOT="$PWD/.rubocop_cache"

    mkdir -p "$GEM_ROOT" "$GEM_HOME" "$PWD/.bin"
    rm -f "$PWD/.bin/bundle" "$PWD/.bin/bundler"

    export PATH="$PWD/.bin:$GEM_HOME/bin:$PATH"
    export RUBYLIB="$CONFCTL/lib"
    export MANPATH="$CONFCTL/man:$(man --path)"

    bundler_version=""
    if [ -f "$CONFCTL/Gemfile.lock" ]; then
      bundler_version="$(awk '/^BUNDLED WITH$/{getline; sub(/^[[:space:]]+/, "", $0); print; exit}' "$CONFCTL/Gemfile.lock")"
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

    pushd "$CONFCTL" >/dev/null

    "$bundle_bin" config set --local path "$BUNDLE_PATH" >/dev/null
    "$bundle_bin" config set --local bin "$PWD/.bin" >/dev/null

    # Purity disabled because of prism gem, which has a native extension.
    NIX_ENFORCE_PURITY=0 "$bundle_bin" install >/dev/null

    if "$bundle_bin" info rubocop >/dev/null 2>&1; then
      "$bundle_bin" binstubs rubocop --force >/dev/null
    fi

    popd >/dev/null

    cat > "$PWD/.bin/confctl" <<EOF
    #!/usr/bin/env bash
    export BUNDLE_GEMFILE="$CONFCTL/Gemfile"
    bundle_bin="$GEM_HOME/bin/bundle"
    exec "$bundle_bin" exec confctl "$@"
    EOF
    chmod +x "$PWD/.bin/confctl"

    export PS1="(confctl) $PS1"

    echo
    echo "confctl development shell ready"
  '';
}
