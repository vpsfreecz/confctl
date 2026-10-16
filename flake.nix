{
  description = "confctl";

  inputs = {
    vpsadminos.url = "github:vpsfreecz/vpsadminos";
    nixpkgs.follows = "vpsadminos/nixpkgs";
  };

  outputs =
    {
      self,
      nixpkgs,
      vpsadminos,
    }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      testSystems = [ "x86_64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system);
      forTestSystems = f: nixpkgs.lib.genAttrs testSystems (system: f system);
      hasTestRunner = system: builtins.elem system testSystems;

      mkConfigDevShell = import ./nix/flake/mk-config-devshell.nix { inherit self nixpkgs; };
      mkConfctlDevShell = import ./nix/flake/mk-confctl-devshell.nix { inherit self nixpkgs; };
      mkLegacyDevShell = import ./nix/flake/mk-legacy-devshell.nix { inherit nixpkgs; };
      mkConfctlPackage = import ./nix/package.nix;

      mkRspecCheck =
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          ruby = if pkgs ? ruby_3_4 then pkgs.ruby_3_4 else pkgs.ruby;
          deps = pkgs.bundlerEnv {
            name = "confctl-rspec-deps";
            inherit ruby;
            gemdir = self.outPath;
            lockfile = "${self.outPath}/Gemfile.lock";
            groups = [
              "default"
              "development"
              "test"
            ];
          };
          runtimePath = pkgs.lib.makeBinPath [
            pkgs.git
            pkgs.openssh
            pkgs.nix
          ];
          gemBin = "${deps}/${ruby.gemPath}/bin";
        in
        pkgs.stdenv.mkDerivation {
          pname = "confctl-rspec";
          version = self.shortRev or (self.dirtyShortRev or "dev");
          src = self;
          dontUnpack = true;
          dontBuild = true;
          nativeBuildInputs = [
            pkgs.git
            pkgs.nix
            pkgs.openssh
          ];
          installPhase = ''
            export HOME="$TMPDIR/home"
            mkdir -p "$HOME"
            export GEM_HOME="${deps}/${ruby.gemPath}"
            export GEM_PATH="${deps}/${ruby.gemPath}"
            export RUBYLIB="$src/lib"
            export PATH="${gemBin}:${runtimePath}:$PATH"
            export CONFCTL_TTY=0
            export NO_COLOR=1
            export PAGER=
            export CONFCTL_BIN="${self.packages.${system}.confctl-ruby}/bin/confctl"
            export CONFCTL_RSPEC_SANDBOX=1
            export CONFCTL_MAX_JOBS=auto

            cd "$src"
            ${ruby}/bin/ruby -S rspec

            mkdir -p "$out"
            echo ok > "$out/result"
          '';
        };

      mkTests =
        system:
        vpsadminos.lib.testFramework.mkTests {
          inherit system;
          pkgsPath = nixpkgs.outPath;
          testsRoot = ./tests;
          suiteArgs = {
            confctlSrc = self.outPath;
            confctlPackage = self.packages.${system}.confctl;
          };
        };

      mkTestsMeta =
        system:
        vpsadminos.lib.testFramework.mkTestsMeta {
          inherit system;
          pkgsPath = nixpkgs.outPath;
          testsRoot = ./tests;
          suiteArgs = {
            confctlSrc = self.outPath;
            confctlPackage = self.packages.${system}.confctl;
          };
        };
    in
    {
      moduleOptions = import ./nix/module-options.nix {
        nixpkgs = nixpkgs;
        confDir = self.outPath;
        system = "x86_64-linux";
      };

      lib.mkConfctlOutputs = import ./nix/flake/mk-confctl-outputs.nix;

      # Dev shells for cluster configuration repos.
      lib.mkConfigDevShell = mkConfigDevShell;

      # Dev shell for working on confctl itself.
      lib.mkConfctlDevShell = mkConfctlDevShell;

      devShells = forAllSystems (system: {
        default = mkConfctlDevShell { inherit system; };
        ruby-original = mkLegacyDevShell { inherit system; };
        experimental = (import nixpkgs { inherit system; }).mkShell {
          packages = with (import nixpkgs { inherit system; }); [
            go
            nixfmt
            git
            openssh
            nix
          ];
        };
      });

      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          confctl-go-prototype = import ./experiments/confctl-go/package.nix { inherit pkgs; };
          confctl-compat-tools = import ./tests/compat/tools.nix { inherit pkgs; };
          default = self.packages.${system}.confctl;
          confctl = mkConfctlPackage {
            inherit pkgs;
            src = self.outPath;
            docs = self.packages.${system}.confctl-docs;
          };
          confctl-ruby = import ./nix/legacy-package.nix {
            inherit pkgs;
            src = self.outPath;
          };
          confctl-docgen = import ./nix/docgen-tool.nix {
            inherit pkgs;
            src = self.outPath;
          };
          confctl-docs = import ./nix/docs.nix {
            inherit pkgs;
            src = self.outPath;
            moduleOptions = self.moduleOptions;
            version = self.shortRev or (self.dirtyShortRev or "dev");
            date = "2026-10-14";
          };
          kexec-netboot-man = import ./nix/kexec-netboot-man.nix { inherit pkgs; };
          confctl-hook-checker = import ./nix/hook-checker.nix {
            inherit pkgs;
            src = self.outPath;
          };
          confctl-dev-tools = import ./nix/dev-tools.nix {
            inherit pkgs;
            checker = self.packages.${system}.confctl-hook-checker;
            goModules = [
              {
                directory = ".";
                source = self.outPath;
                proxy = import ./nix/go-module-proxy.nix {
                  inherit pkgs;
                  src = self.outPath;
                };
              }
            ];
          };
        }
        // nixpkgs.lib.optionalAttrs (hasTestRunner system) {
          test-runner = vpsadminos.packages.${system}.test-runner;
        }
      );

      apps = forAllSystems (
        system:
        nixpkgs.lib.optionalAttrs (hasTestRunner system) {
          test-runner = {
            type = "app";
            program = "${vpsadminos.packages.${system}.test-runner}/bin/test-runner";
          };
        }
      );

      tests = forTestSystems (system: mkTests system);

      testsMeta = forTestSystems (system: mkTestsMeta system);

      legacyChecks = forAllSystems (system: {
        rspec = mkRspecCheck system;
      });

      checks = forAllSystems (system: {
        confctl = self.packages.${system}.confctl;
        confctl-docgen = self.packages.${system}.confctl-docgen;
        confctl-dev-tools = self.packages.${system}.confctl-dev-tools;
        hook-dependencies =
          (import ./tests/hooks/dependencies.nix {
            pkgs = import nixpkgs { inherit system; };
            src = self.outPath;
          }).check;
        hook-policy = import ./tests/hooks/check.nix {
          pkgs = import nixpkgs { inherit system; };
          src = self.outPath;
        };
        docgen = import ./tests/docgen/check.nix {
          pkgs = import nixpkgs { inherit system; };
        };
      });

      checksMeta = forAllSystems (
        system:
        nixpkgs.lib.optionalAttrs (hasTestRunner system) {
          tests = mkTestsMeta system;
        }
      );

      nixosModules = {
        generations = import ./nix/modules/confctl/generations.nix;
        cli = import ./nix/modules/confctl/cli.nix;
        nix = import ./nix/modules/confctl/nix.nix;
        inputs-info = import ./nix/modules/confctl/inputs-info.nix;
        configuration-info = import ./nix/modules/confctl/configuration-info.nix;
        default = {
          imports = [
            (import ./nix/modules/confctl/generations.nix)
            (import ./nix/modules/confctl/cli.nix)
            (import ./nix/modules/confctl/nix.nix)
            (import ./nix/modules/confctl/inputs-info.nix)
            (import ./nix/modules/confctl/configuration-info.nix)
          ];
        };
      };
    };
}
