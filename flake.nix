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
      mkConfctlDevShell = import ./nix/flake/mk-confctl-devshell.nix { inherit nixpkgs; };
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
            pkgs.nix-prefetch-git
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
            pkgs.nix-prefetch-git
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
            export CONFCTL_BIN="${self.packages.${system}.confctl}/bin/confctl"
            export NIX_PATH="nixpkgs=${nixpkgs.outPath}"
            export CONFCTL_TEST_NIXPKGS="${nixpkgs.outPath}"
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
            vpsadminosPath = vpsadminos.outPath;
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
            vpsadminosPath = vpsadminos.outPath;
            confctlSrc = self.outPath;
            confctlPackage = self.packages.${system}.confctl;
          };
        };
    in
    {
      lib.mkConfctlOutputs = import ./nix/flake/mk-confctl-outputs.nix;

      # Dev shells for cluster configuration repos.
      lib.mkConfigDevShell = mkConfigDevShell;

      # Dev shell for working on confctl itself.
      lib.mkConfctlDevShell = mkConfctlDevShell;

      devShells = forAllSystems (system: {
        default = mkConfctlDevShell { inherit system; };
      });

      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          confctl = mkConfctlPackage {
            inherit pkgs;
            src = self.outPath;
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

      checks = forAllSystems (
        system:
        {
          rspec = mkRspecCheck system;
        }
        // nixpkgs.lib.optionalAttrs (hasTestRunner system) (mkTests system)
      );

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
        swpins = import ./nix/modules/confctl/swpins.nix;
        inputs-info = import ./nix/modules/confctl/inputs-info.nix;
        default = {
          imports = [
            (import ./nix/modules/confctl/generations.nix)
            (import ./nix/modules/confctl/cli.nix)
            (import ./nix/modules/confctl/nix.nix)
            (import ./nix/modules/confctl/inputs-info.nix)
          ];
        };
      };
    };
}
