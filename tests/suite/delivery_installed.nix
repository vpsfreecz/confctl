import ../make-test.nix (
  { pkgs, confctlSrc, ... }:
  let
    src = if confctlSrc == null then throw "suiteArgs.confctlSrc is required" else confctlSrc;
    packages = import ../delivery/packages.nix { inherit pkgs src; };
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
      ordinaryPackages = "Lead-recorded ordinary outputs are separate observations.";
      cliCells = "Pending the lead-supplied integrated normal CLI/tooling source.";
    };
  in
  {
    name = "delivery-installed";
    description = "Finite installed helper acceptance with guest-only effects";
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
        ];
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
