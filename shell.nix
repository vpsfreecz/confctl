let
  pkgs = import <nixpkgs> { };
  lib = pkgs.lib;
  stdenv = pkgs.stdenv;
in
stdenv.mkDerivation rec {
  name = "confctl-shell";

  buildInputs = with pkgs; [
    git
    ncurses
    nix-prefetch-git
    nixfmt
    nixfmt-tree
    openssl
    ruby
  ];

  shellHook = ''
    CONFCTL="${toString ./.}"
    BASEDIR="$(realpath `pwd`)"
    export GEM_ROOT="$(pwd)/.gems"
    export GEM_HOME="$GEM_ROOT/ruby/$(ruby -e 'require "rbconfig"; print RbConfig::CONFIG["ruby_version"]')"
    export GEM_PATH="$GEM_HOME"
    export BUNDLE_PATH="$GEM_ROOT"
    export BUNDLE_APP_CONFIG="$BASEDIR/.bundle"
    export RUBOCOP_CACHE_ROOT="$CONFCTL/.rubocop_cache"
    BINDIR="$GEM_HOME/bin"
    mkdir -p "$BINDIR"

    export PATH="$BINDIR:$PATH"
    export RUBYLIB="$CONFCTL/lib"
    export MANPATH="$CONFCTL/man:$(man --path)"

    bundler_version=""
    if [ -f "$CONFCTL/Gemfile.lock" ]; then
      bundler_version="$(awk '/^BUNDLED WITH$/{getline; sub(/^[[:space:]]+/, "", $0); print; exit}' "$CONFCTL/Gemfile.lock")"
    fi

    if [ -n "$bundler_version" ]; then
      export BUNDLER_VERSION="$bundler_version"
      # Ruby can ship Bundler as a default gem without creating a wrapper in
      # GEM_HOME/bin, but the shell below executes Bundler through that path.
      if [ ! -x "$GEM_HOME/bin/bundle" ] \
         || ! gem list -i bundler -v "$bundler_version" >/dev/null 2>&1; then
        gem install --no-document bundler -v "$bundler_version"
      fi
    elif [ ! -x "$GEM_HOME/bin/bundle" ]; then
      gem install --no-document bundler
    fi
    bundle_bin="$GEM_HOME/bin/bundle"

    pushd "$CONFCTL"

    # Purity disabled because of prism gem, which has a native extension.
    # The extension has its header files in .gems, which gets stripped but
    # cc wrapper in Nix. Without NIX_ENFORCE_PURITY=0, we get prism.h not found
    # error.
    NIX_ENFORCE_PURITY=0 "$bundle_bin" install

    "$bundle_bin" config set --local path "$BUNDLE_PATH"
    "$bundle_bin" config set --local bin "$BINDIR"
    "$bundle_bin" binstubs rubocop --force

    "$bundle_bin" exec rake md2man:man
    popd

    cat <<EOF > "$BINDIR/confctl"
    #!${pkgs.ruby}/bin/ruby
    ENV['BUNDLE_GEMFILE'] = "$CONFCTL/Gemfile"

    require 'bundler'
    Bundler.setup

    load File.join('$CONFCTL', 'bin/confctl')
    EOF
    chmod +x "$BINDIR/confctl"

  '';
}
