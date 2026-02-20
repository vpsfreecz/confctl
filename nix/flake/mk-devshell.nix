{ self, nixpkgs }:

# mkDevShell builds a dev shell intended for *cluster configuration repos*
# that use confctl as a flake input.
#
# It:
# - installs Ruby gems into ./\.gems (current directory)
# - forces bundler to use confctl from the pinned flake input via `local.confctl`
# - creates ./\.bin/confctl wrapper that runs `bundle exec confctl`
# - generates man pages into ./\.man and exports MANPATH so `man confctl` works

{
  system,
  pkgs ? nixpkgs.legacyPackages.${system},
  extraPackages ? [ ],
}:

pkgs.mkShell {
  packages =
    (with pkgs; [
      git
      ncurses
      openssl
      ruby
      man-db
      groff
      less
      # helpful tools used in existing shell.nix / workflows
      nix-prefetch-git
      nixfmt-rfc-style
      nixfmt-tree
    ])
    ++ extraPackages;

  shellHook = ''
    # Pinned confctl source (flake input). When this shell is used from another
    # flake, `self.outPath` points to the input store path.
    export CONFCTL_SRC="${self.outPath}"

    # Keep everything local to the current repo.
    export GEM_HOME="$PWD/.gems"
    export GEM_PATH="$GEM_HOME"
    export BUNDLE_PATH="$GEM_HOME"
    export BUNDLE_APP_CONFIG="$PWD/.bundle"
    export PATH="$PWD/.bin:$GEM_HOME/bin:$PATH"

    mkdir -p "$GEM_HOME" "$PWD/.bin" "$PWD/.man/man8"

    # Ensure bundler is available
    gem install --no-document bundler >/dev/null

    # If the repo has a Gemfile, use it; otherwise create a minimal one.
    if [ ! -f "$PWD/Gemfile" ]; then
      mkdir -p "$PWD/.confctl-shell"

      cat > "$PWD/.confctl-shell/Gemfile" <<EOF
    source "https://rubygems.org"
    gem "confctl", path: "$CONFCTL_SRC"
    EOF

      export BUNDLE_GEMFILE="$PWD/.confctl-shell/Gemfile"
      export BUNDLE_APP_CONFIG="$PWD/.confctl-shell/.bundle"
      mkdir -p "$BUNDLE_APP_CONFIG"
    fi

    # Force bundler to use the pinned confctl sources even if Gemfile says `gem "confctl"`
    # (Bundler supports per-gem local overrides via `local.<name>`.)
    bundle config set --local path "$GEM_HOME" >/dev/null
    bundle config set --local "local.confctl" "$CONFCTL_SRC" >/dev/null

    # Install gems (purity disabled due to native extensions in some gems)
    NIX_ENFORCE_PURITY=0 bundle install >/dev/null

    # Provide `confctl` on PATH
    cat > "$PWD/.bin/confctl" <<'EOF'
    #!/usr/bin/env bash
    exec bundle exec confctl "$@"
    EOF

    chmod +x "$PWD/.bin/confctl"

    # Generate man pages locally from markdown (do NOT write into the flake input path)
    bundle exec md2man-roff "$CONFCTL_SRC/man/man8/confctl.8.md" > "$PWD/.man/man8/confctl.8"
    bundle exec md2man-roff "$CONFCTL_SRC/man/man8/confctl-options.nix.8.md" > "$PWD/.man/man8/confctl-options.nix.8"

    export MANPATH="$PWD/.man:$(man --path)"
    export PS1="(confctl) $PS1"

    echo
    echo "confctl dev shell ready"
  '';
}
