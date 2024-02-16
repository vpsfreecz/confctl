let
  pkgs = import <nixpkgs> {};
  lib = pkgs.lib;
  stdenv = pkgs.stdenv;
in stdenv.mkDerivation rec {
  name = "confctl-shell";

  buildInputs = with pkgs; [
    git
    ncurses
    nix-prefetch-git
    openssl
    ruby
  ];

  shellHook = ''
    CONFCTL="${toString ./.}"
    BASEDIR="$(realpath `pwd`)"
    export GEM_HOME="$(pwd)/.gems"
    BINDIR="$(ruby -e 'puts Gem.bindir')"
    mkdir -p "$BINDIR"
    export PATH="$BINDIR:$PATH"
    export RUBYLIB="$GEM_HOME:$CONFCTL/lib"
    export MANPATH="$CONFCTL/man:$(man --path)"
    gem install --no-document bundler rubocop
    pushd "$CONFCTL"
    bundle install
    bundle exec rake md2man:man
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
