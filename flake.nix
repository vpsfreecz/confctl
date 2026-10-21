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
      mkRustPackage = import ./nix/rust-package.nix;

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
      });

      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          confctl = self.packages.${system}.confctl-rust;
          confctl-rust = mkRustPackage {
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
        {
          confctl-rust = {
            type = "app";
            program = "${self.packages.${system}.confctl-rust}/bin/confctl-rust";
          };
        }
        // nixpkgs.lib.optionalAttrs (hasTestRunner system) {
          test-runner = {
            type = "app";
            program = "${vpsadminos.packages.${system}.test-runner}/bin/test-runner";
          };
        }
      );

      tests = forTestSystems (system: mkTests system);

      testsMeta = forTestSystems (system: mkTestsMeta system);

      checks = forAllSystems (system: nixpkgs.lib.optionalAttrs (hasTestRunner system) (mkTests system));

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
        runtime-kernels = import ./nix/modules/confctl/runtime-kernels.nix;
        netboot-inventory = import ./nix/modules/confctl/netboot-inventory.nix;
        default = {
          imports = [
            (import ./nix/modules/confctl/generations.nix)
            (import ./nix/modules/confctl/cli.nix)
            (import ./nix/modules/confctl/nix.nix)
            (import ./nix/modules/confctl/inputs-info.nix)
            (import ./nix/modules/confctl/configuration-info.nix)
            (import ./nix/modules/confctl/runtime-kernels.nix)
            (import ./nix/modules/confctl/netboot-inventory.nix)
          ];
        };
      };
    };
}
