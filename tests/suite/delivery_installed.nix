import ../make-test.nix (
  {
    pkgs,
    confctlSrc,
    appSrc ? null,
    appDocs ? null,
    oracleSrc ? null,
    sitePairs ? [ ],
    ...
  }:
  let
    src = if confctlSrc == null then throw "suiteArgs.confctlSrc is required" else confctlSrc;
    packages = import ../delivery/packages.nix {
      inherit
        pkgs
        src
        appSrc
        appDocs
        oracleSrc
        sitePairs
        ;
    };
    bindings = {
      root = packages.root;
      carrier = "${packages.carrier}/bin/carrier-env";
      rollback = toString packages.rollback; # The public output is a flat file.
      builder = "${packages.builder}/bin/build-netboot-server";
      kexec = "${packages.kexec}/bin/kexec-netboot";
      tools = "${packages.tools}/bin";
      ruby = "${pkgs.ruby}/bin/ruby";
      bash = pkgs.runtimeShell;
      originalIndex = "${src}/internal/netboot/testdata/original/machines.json";
      fixtureSrc = toString src;
      ordinaryPackages = "Lead-recorded ordinary outputs are separate observations.";
      # Null defaults preserve the original helper-only selection. A full run
      # supplies an immutable corrected appSrc and its normal documentation.
      cli = if packages.cli == null then null else "${packages.cli}/bin/confctl";
      operationCLI =
        if packages.operationCLI == null then null else "${packages.operationCLI}/bin/confctl";
      originalCLI = if packages.originalCLI == null then null else "${packages.originalCLI}/bin/confctl";
      # Raw state adapters need the same locked Ruby/gems as the legacy wrapper.
      # Keep these separate from the guest transport and ordinary Go env.
      oracleRuby = if packages.originalCLI == null then null else "${packages.originalCLI.ruby}/bin/ruby";
      oracleGemPath =
        if packages.originalCLI == null then
          null
        else
          "${packages.originalCLI.deps}/${packages.originalCLI.ruby.gemPath}";
      oracleSrc = if oracleSrc == null then null else toString oracleSrc;
      peer = "${packages.peer}/bin/delivery-peer";
      git = "${pkgs.git}/bin/git";
      coreutils = "${pkgs.coreutils}/bin";
      sitePairs = map (pair: {
        inherit (pair) name;
        source = toString pair.source;
        package = toString pair.package;
        registry = toString pair.registry;
        rebuiltSource = if pair ? rebuiltSource then toString pair.rebuiltSource else null;
        rebuiltRegistry = if pair ? rebuiltRegistry then toString pair.rebuiltRegistry else null;
        rebuiltPackage = if pair ? rebuiltPackage then toString pair.rebuiltPackage else null;
      }) sitePairs;
    };
  in
  {
    name = "delivery-installed";
    description = "Finite installed CLI, state, site and helper acceptance with guest-only effects";
    tags = [ "ci" ];
    machines.fixture = {
      spin = "nixos";
      diskSize = 8192;
      config = {
        networking.hostName = "delivery-fixture";
        # kexec-netboot reads cmdline even with an explicit server URL.
        boot.kernelParams = [ "httproot=http://127.0.0.1:18080/node.test/1/initrd" ];
        environment.systemPackages = [
          pkgs.ruby
          pkgs.python3
          pkgs.coreutils
          pkgs.git
          pkgs.bash
        ];
        # Host-side script bindings do not enter the guest image closure.
        # The helpers retain their bound fake kexec and recording callback;
        # src supplies the original index without a host source/store mount.
        system.extraDependencies = [
          packages.carrier
          packages.rollback
          packages.builder
          packages.kexec
          packages.tools
          src
          packages.peer
        ]
        ++ pkgs.lib.optionals (appSrc != null) [
          appSrc
          appDocs
          packages.cli
          packages.operationCLI
        ]
        ++ pkgs.lib.optionals (oracleSrc != null) [
          oracleSrc
          packages.originalCLI
          packages.originalCLI.ruby
          packages.originalCLI.deps
        ]
        ++ pkgs.lib.concatMap (
          pair:
          [
            pair.source
            pair.package
            pair.registry
          ]
          ++ pkgs.lib.optional (pair ? rebuiltSource) pair.rebuiltSource
          ++ pkgs.lib.optional (pair ? rebuiltRegistry) pair.rebuiltRegistry
          ++ pkgs.lib.optional (pair ? rebuiltPackage) pair.rebuiltPackage
        ) sitePairs;
        systemd.tmpfiles.rules = [ "d ${packages.root} 0700 root root -" ];
        systemd.services.delivery-http = {
          description = "Loopback-only installed helper fixture HTTP";
          serviceConfig = {
            ExecStart = "${pkgs.python3}/bin/python3 -m http.server 18080 --bind 127.0.0.1 --directory ${packages.root}/http";
            Restart = "no";
          };
        };
      };
    };
    # All effectful operations in this driver cross the VM boundary. No host
    # profiles, /run, home, roots or configuration directories are shared.
    testScript = ''
      require 'json'
      DELIVERY_BINDINGS = JSON.parse(${builtins.toJSON (builtins.toJSON bindings)})
    ''
    + builtins.readFile ../delivery/installed_driver.rb;
  }
)
