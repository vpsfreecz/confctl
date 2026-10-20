{
  pkgs,
  src ? ../.,
}:
let
  inherit (pkgs) lib;
  runtimePath = lib.makeBinPath [
    pkgs.nix
    pkgs.git
    pkgs.openssh
    pkgs.coreutils
    pkgs.bash
    pkgs.less
  ];
  cliWrapper = pkgs.writeText "confctl-rust-wrapper" ''
    #!${pkgs.bash}/bin/bash
    export PATH=${lib.escapeShellArg runtimePath}:"$PATH"
    if [ -z "''${SSL_CERT_FILE+x}" ]; then
      export SSL_CERT_FILE=${lib.escapeShellArg "${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"}
    fi
    if [ -z "''${NIX_SSL_CERT_FILE+x}" ]; then
      export NIX_SSL_CERT_FILE=${lib.escapeShellArg "${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"}
    fi
    exec -a "$0" @out@/libexec/confctl/confctl-rust "$@"
  '';
  package = pkgs.rustPlatform.buildRustPackage {
    pname = "confctl-rust";
    version = "3.0.0";
    inherit src;
    cargoLock.lockFile = src + "/Cargo.lock";
    cargoBuildFlags = [ "--bins" ];

    # Keep the lexical multicall entry: login.rs captures this at compile time.
    CONFCTL_LOGNAME = "${pkgs.coreutils}/bin/logname";
    dontUseCmakeConfigure = true;
    dontGzipMan = true;
    nativeBuildInputs = [
      pkgs.cmake
      pkgs.pandoc
      pkgs.asciidoctor
    ];
    nativeCheckInputs = [
      pkgs.git
      pkgs.python3
      pkgs.coreutils
      pkgs.bash
    ];

    postInstall = ''
      mkdir -p "$out/libexec/confctl" "$out/bin" "$out/share/man/man8" "$out/share/licenses/confctl"
      for name in confctl-rust confctl-auto-rollback carrier-env build-netboot-server kexec-netboot; do
        mv "$out/bin/$name" "$out/libexec/confctl/$name"
      done
      substitute ${cliWrapper} "$out/bin/confctl" --subst-var-by out "$out"
      chmod 755 "$out/bin/confctl"
      ln -s confctl "$out/bin/confctl-rust"

      for name in confctl confctl-options.nix; do
        pandoc --from=markdown --to=man --standalone \
          --metadata="title:$name" --metadata=section:8 \
          --output="$out/share/man/man8/$name.8" "man/man8/$name.8.md"
      done
      asciidoctor -b manpage -D "$out/share/man/man8" \
        nix/modules/confctl/kexec-netboot/kexec-netboot.8.adoc
      install -m644 LICENSE.txt "$out/share/licenses/confctl/LICENSE.txt"
      sed -n '/^\/\/ Boost Software License/,/^$/p' src/remote/json/float.rs \
        | sed 's|^// *||' > "$out/share/licenses/confctl/Boost-1.0.txt"
    '';

    passthru.tests.smoke = pkgs.runCommand "confctl-rust-package-smoke" { } ''
      set -x
      unset CONFCTL_INTERNAL_CARRIER_ON_CHANGE CONFCTL_INTERNAL_NETBOOT_CONFIG \
        CONFCTL_INTERNAL_KEXEC_MACHINE_FQDN CONFCTL_INTERNAL_KEXEC_EXECUTABLE
      for name in confctl-rust confctl-auto-rollback carrier-env build-netboot-server kexec-netboot; do
        test -x ${package}/libexec/confctl/$name
      done
      for name in confctl confctl-rust; do
        ${package}/bin/$name --help > "$TMPDIR/$name-help"
        test -s "$TMPDIR/$name-help"
      done
      for name in confctl-auto-rollback carrier-env kexec-netboot; do
        ${package}/libexec/confctl/$name --help > "$TMPDIR/$name-help"
        test -s "$TMPDIR/$name-help"
      done
      for name in confctl confctl-options.nix kexec-netboot; do
        test -s ${package}/share/man/man8/$name.8
      done
      test -s ${package}/share/licenses/confctl/LICENSE.txt
      test -s ${package}/share/licenses/confctl/Boost-1.0.txt
      test ! -e ${package}/src
      test ! -e ${package}/tests
      mkdir "$out"
    '';
    passthru.tests.shellSelection =
      let
        system = pkgs.stdenv.hostPlatform.system;
        rubyPackage = import ./package.nix { inherit pkgs src; };
        shell = import ./flake/mk-config-devshell.nix {
          self = {
            outPath = src;
            packages.${system} = {
              confctl = rubyPackage;
              confctl-rust = package;
            };
          };
          nixpkgs = { };
        };
        select = args: shell ({ inherit system pkgs; } // args);
        includes =
          expected: value: builtins.elem (toString expected) (map toString value.nativeBuildInputs);
      in
      assert includes rubyPackage (select { });
      assert includes package (select {
        implementation = "rust";
      });
      assert includes package (select {
        implementation = "rust";
        mode = "tools";
      });
      assert
        !(builtins.tryEval (select {
          implementation = "unknown";
        })).success;
      assert
        !(builtins.tryEval (select {
          implementation = "rust";
          mode = "bundled-confctl";
        })).success;
      pkgs.runCommand "confctl-rust-shell-selection" { } ''
        mkdir "$out"
      '';
    meta = {
      description = "Opt-in Rust confctl CLI and private remote helper backends";
      license = lib.licenses.gpl3Only;
      platforms = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      mainProgram = "confctl";
    };
  };
in
package
