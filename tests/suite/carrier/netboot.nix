import ../../make-test.nix (
  {
    pkgs,
    confctlPackage,
    confctlSrc,
    vpsadminosPath,
    ...
  }:
  let
    confctlBin =
      if confctlPackage == null then
        throw "suiteArgs.confctlPackage is required"
      else
        "${confctlPackage}/bin/confctl";

    confctlSource = if confctlSrc == null then throw "suiteArgs.confctlSrc is required" else confctlSrc;
    vpsadminosSource =
      if vpsadminosPath == null then throw "suiteArgs.vpsadminosPath is required" else vpsadminosPath;

    carrierMgmtMac = "52:54:00:10:00:01";
    carrierPxeMac = "52:54:00:10:00:02";
    nixosPxeMac = "52:54:00:10:00:11";
    vpsadminosPxeMac = "52:54:00:10:00:12";
    pxeMachineMemory = 4096;
    pxeNetworkTag = "carrier-pxe";
  in
  {
    name = "carrier-netboot";

    description = ''
      Verify carried NixOS and vpsAdminOS machines can be served and booted over PXE from a NixOS carrier.
    '';

    tags = [
      "ci"
    ];

    machines = {
      carrier = {
        spin = "nixos";
        diskSize = 20480;
        networks = [
          {
            type = "user";
            macAddress = carrierMgmtMac;
            opts = {
              hostForward = "tcp::net1-:22";
              network = "10.0.2.0/24";
              host = "10.0.2.2";
              dns = "10.0.2.3";
            };
          }
          {
            type = "socket";
            macAddress = carrierPxeMac;
            model = "e1000";
            mcast.port = pxeNetworkTag;
          }
        ];
        config = {
          services.openssh = {
            enable = true;
            settings = {
              PermitRootLogin = "yes";
              PasswordAuthentication = false;
            };
          };
          services.logrotate.checkConfig = false;
          networking.firewall.enable = false;
          environment.systemPackages = with pkgs; [
            git
          ];
        };
      };

      nixos = {
        spin = "nixos";
        config = null;
        memory = pxeMachineMemory;
        cpus = 2;
        cpu = {
          cores = 2;
          threads = 1;
          sockets = 1;
        };
        networks = [
          {
            type = "socket";
            macAddress = nixosPxeMac;
            model = "e1000";
            mcast.port = pxeNetworkTag;
          }
        ];
      };

      vpsadminos = {
        spin = "vpsadminos";
        config = null;
        memory = pxeMachineMemory;
        cpus = 2;
        cpu = {
          cores = 2;
          threads = 1;
          sockets = 1;
        };
        networks = [
          {
            type = "socket";
            macAddress = vpsadminosPxeMac;
            model = "e1000";
            mcast.port = pxeNetworkTag;
          }
        ];
      };
    };

    testScript = ''
      require 'base64'
      require 'fileutils'
      require 'json'
      require 'shellwords'

      CARRIER_MACHINE = 'carrier'
      NIXOS_MACHINE = 'nixos-machine'
      VPSADMINOS_MACHINE = 'vpsadminos-machine'
      VPSADMINOS_PATH = '${vpsadminosSource}'

      NIXOS_FQDN = "#{NIXOS_MACHINE}.test.invalid"
      VPSADMINOS_FQDN = "#{VPSADMINOS_MACHINE}.test.invalid"
      NIXOS_BOOT_TIMEOUT = 600
      VPSADMINOS_BOOT_TIMEOUT = 360
      NETBOOT_HTTP_TIMEOUT = 180
      PXE_ADDRESS = '192.168.100.1'
      PXE_SUBNET = '192.168.100.0/24'

      NIXOS_PXE_MAC = '${nixosPxeMac}'
      VPSADMINOS_PXE_MAC = '${vpsadminosPxeMac}'

      def nixos_virtualisation_options_module
        <<~NIX
          (
            { lib, ... }:
            {
              options.virtualisation.cores = lib.mkOption {
                type = lib.types.int;
                default = 2;
              };
              options.virtualisation.memorySize = lib.mkOption {
                type = lib.types.int;
                default = 2048;
              };
              options.virtualisation.mountHostNixStore = lib.mkOption {
                type = lib.types.bool;
                default = false;
              };
              options.virtualisation.sharedDirectories = lib.mkOption {
                type = lib.types.attrsOf lib.types.anything;
                default = { };
              };
            }
          )
        NIX
      end

      def nixos_test_base_import
        '(inputs.vpsadminos + "/tests/configs/nixos/base.nix")'
      end

      def vpsadminos_test_base_import
        '(inputs.vpsadminos + "/tests/configs/vpsadminos/base.nix")'
      end

      def write_flake_root!(conf_dir)
        File.write(File.join(conf_dir, 'flake.nix'), <<~NIX)
          {
            description = "confctl carrier netboot test fixture";

            inputs = {
              confctl.url = "path:#{confctl_src}";
              nixpkgs.follows = "confctl/nixpkgs";
              vpsadminos.url = "path:#{VPSADMINOS_PATH}";
            };

            outputs = inputs@{ self, confctl, ... }:
              let
                channels = {
                  carrier = {
                    nixpkgs = "nixpkgs";
                    vpsadminos = "vpsadminos";
                  };
                };

                confctlOutputs = confctl.lib.mkConfctlOutputs {
                  confDir = ./.;
                  inherit inputs channels;
                };
              in
              {
                confctl = confctlOutputs;

                devShells.x86_64-linux.default = confctl.lib.mkConfigDevShell {
                  system = "x86_64-linux";
                  mode = "minimal";
                };
              };
          }
        NIX
      end

      def write_hardware_config!(conf_dir, machine_name)
        dir = File.join(conf_dir, 'cluster', machine_name)
        FileUtils.mkdir_p(dir)

        File.write(File.join(dir, 'hardware.nix'), <<~NIX)
          {
            config,
            pkgs,
            lib,
            ...
          }:
          {
            # Test fixture hardware configuration.
          }
        NIX
      end

      def write_carrier_config!(conf_dir)
        write_hardware_config!(conf_dir, 'carrier')

        File.write(File.join(conf_dir, 'cluster/carrier/config.nix'), <<~NIX)
          { inputs, lib, ... }:
          {
            imports = [
              ../../environments/base.nix
              ./hardware.nix
              #{nixos_virtualisation_options_module}
              #{nixos_test_base_import}
            ];

            networking.hostName = "carrier";
            time.timeZone = lib.mkForce "UTC";
            services.openssh = {
              enable = true;
              settings = {
                PermitRootLogin = "yes";
                PasswordAuthentication = false;
              };
            };

            networking.firewall.enable = false;
            networking.useNetworkd = true;
            systemd.network.enable = true;

            systemd.network.networks = {
              "10-mgmt" = {
                matchConfig.Name = "eth0";
                networkConfig.DHCP = "ipv4";
                linkConfig.RequiredForOnline = "routable";
              };

              "10-pxe" = {
                matchConfig.Name = "eth1";
                address = [ "#{PXE_ADDRESS}/24" ];
                networkConfig.DHCP = "no";
                linkConfig.RequiredForOnline = "no";
              };
            };

            services.dnsmasq = {
              enable = true;
              settings = {
                interface = "eth1";
                bind-interfaces = true;
                port = 0;
                dhcp-authoritative = true;
                log-dhcp = true;
                dhcp-range = "192.168.100.50,192.168.100.150,255.255.255.0";
                dhcp-option = [
                  "3,#{PXE_ADDRESS}"
                  "6,#{PXE_ADDRESS}"
                ];
                dhcp-boot = "pxelinux.0,carrier,#{PXE_ADDRESS}";
              };
            };
            services.logrotate.checkConfig = false;

            confctl.carrier.netboot = {
              enable = true;
              host = "#{PXE_ADDRESS}";
              allowedIPv4Ranges = [ "#{PXE_SUBNET}" ];
            };
          }
        NIX
      end

      def write_nixos_config!(conf_dir, marker:)
        write_hardware_config!(conf_dir, NIXOS_MACHINE)

        File.write(File.join(conf_dir, "cluster/#{NIXOS_MACHINE}/config.nix"), <<~NIX)
          { config, lib, pkgs, confMachine, inputs, inputsInfo, ... }:
          let
            toplevel = builtins.unsafeDiscardStringContext config.system.build.toplevel;
            kernelParams = [
              "console=ttyS0"
              "init=''${toplevel}/init"
              "nohibernate"
              "loglevel=4"
              "lsm=landlock,yama,bpf"
            ];

            machineJson = pkgs.writeText "machine-#{NIXOS_MACHINE}.json" (builtins.toJSON {
              spin = "nixos";
              fqdn = confMachine.host.fqdn;
              label = confMachine.host.fqdn;
              inherit toplevel kernelParams;
              version = config.system.nixos.version;
              revision = config.system.nixos.revision;
              macs = confMachine.netboot.macs;
              inputs-info = inputsInfo;
            });
          in
          {
            imports = [
              ../../environments/base.nix
              ./hardware.nix
              #{nixos_virtualisation_options_module}
              #{nixos_test_base_import}
              (inputs.nixpkgs + "/nixos/modules/installer/netboot/netboot-minimal.nix")
            ];

            networking.hostName = "#{NIXOS_MACHINE}";
            time.timeZone = lib.mkForce "UTC";
            networking.useDHCP = lib.mkForce true;
            networking.nameservers = [ "#{PXE_ADDRESS}" ];
            virtualisation.memorySize = lib.mkForce ${toString pxeMachineMemory};
            boot.initrd.availableKernelModules = lib.mkAfter [
              "e1000"
              "e1000e"
            ];

            confctl.programs.kexec-netboot.enable = true;

            environment.etc."confctl-marker".text = "#{marker}\n";

            system.build.dist = pkgs.symlinkJoin {
              name = "#{NIXOS_MACHINE}-netboot";
              paths = [
                config.system.build.netbootRamdisk
                config.system.build.kernel
                config.system.build.netbootIpxeScript
              ];

              postBuild = '''
                ln -s ''${machineJson} $out/machine.json
              ''';
            };
          }
        NIX
      end

      def write_vpsadminos_config!(conf_dir, marker:)
        write_hardware_config!(conf_dir, VPSADMINOS_MACHINE)

        File.write(File.join(conf_dir, "cluster/#{VPSADMINOS_MACHINE}/config.nix"), <<~NIX)
          { config, pkgs, lib, confMachine, inputsInfo, inputs, ... }:
          let
            machineJson = pkgs.writeText "machine-#{VPSADMINOS_MACHINE}.json" (builtins.toJSON {
              spin = "vpsadminos";
              fqdn = confMachine.host.fqdn;
              label = confMachine.host.fqdn;
              toplevel = builtins.unsafeDiscardStringContext config.system.build.toplevel;
              version = config.system.vpsadminos.version;
              revision = config.system.vpsadminos.revision;
              macs = confMachine.netboot.macs;
              inputs-info = inputsInfo;
            });
          in
          {
            imports = [
              ../../environments/base.nix
              ./hardware.nix
              #{vpsadminos_test_base_import}
            ];

            networking.hostName = "#{VPSADMINOS_MACHINE}";
            networking.static.enable = false;
            networking.useDHCP = true;
            networking.nameservers = [ "#{PXE_ADDRESS}" ];

            osctld.waitForNetworkOnline = false;
            osctld.waitForSetClock = false;

            boot.kernelParams = [
              "console=ttyS0"
            ];
            boot.initrd.network = {
              enable = true;
              useDHCP = true;
              preferredDHCPInterfaceMacAddresses = [ "#{VPSADMINOS_PXE_MAC}" ];
            };
            boot.initrd.kernelModules = lib.mkAfter [
              "e1000"
              "e1000e"
            ];

            confctl.programs.kexec-netboot.enable = true;

            environment.etc."confctl-marker".text = "#{marker}\n";

            system.distBuilderCommands = '''
              ln -s ''${machineJson} $out/machine.json
            ''';

            system.stateVersion = "20.09";
          }
        NIX
      end

      def write_machine_modules!(conf_dir, carrier_port:)
        FileUtils.mkdir_p(File.join(conf_dir, 'cluster', 'carrier'))
        FileUtils.mkdir_p(File.join(conf_dir, 'cluster', NIXOS_MACHINE))
        FileUtils.mkdir_p(File.join(conf_dir, 'cluster', VPSADMINOS_MACHINE))

        File.write(File.join(conf_dir, 'cluster/carrier/module.nix'), <<~NIX)
          { config, ... }:
          {
            cluster."carrier" = {
              spin = "nixos";
              inputs.channels = [ "carrier" ];
              host = {
                name = "carrier";
                domain = "test.invalid";
                target = "127.0.0.1";
                port = #{carrier_port};
              };
              healthChecks.machineCommands = [
                {
                  description = "true";
                  command = [ "true" ];
                }
              ];

              carrier = {
                enable = true;
                machines = [
                  {
                    machine = "#{NIXOS_MACHINE}";
                    alias = "#{NIXOS_MACHINE}";
                    buildAttribute = [ "system" "build" "dist" ];
                  }
                  {
                    machine = "#{VPSADMINOS_MACHINE}";
                    alias = "#{VPSADMINOS_MACHINE}";
                    buildAttribute = [ "system" "build" "dist" ];
                  }
                ];
              };
            };
          }
        NIX

        File.write(File.join(conf_dir, "cluster/#{NIXOS_MACHINE}/module.nix"), <<~NIX)
          { config, ... }:
          {
            cluster."#{NIXOS_MACHINE}" = {
              spin = "nixos";
              inputs.channels = [ "carrier" ];
              host = {
                name = "#{NIXOS_MACHINE}";
                domain = "test.invalid";
                target = null;
              };
              netboot = {
                enable = true;
                macs = [ "#{NIXOS_PXE_MAC}" ];
              };
            };
          }
        NIX

        File.write(File.join(conf_dir, "cluster/#{VPSADMINOS_MACHINE}/module.nix"), <<~NIX)
          { config, ... }:
          {
            cluster."#{VPSADMINOS_MACHINE}" = {
              spin = "vpsadminos";
              inputs.channels = [ "carrier" ];
              host = {
                name = "#{VPSADMINOS_MACHINE}";
                domain = "test.invalid";
                target = null;
              };
              netboot = {
                enable = true;
                macs = [ "#{VPSADMINOS_PXE_MAC}" ];
              };
            };
          }
        NIX
      end

      def prepare_fixture!(conf_dir:, carrier_port:, pubkey:)
        prepare_fixture_dir!(conf_dir)
        write_flake_root!(conf_dir)
        FileUtils.rm_f(File.join(conf_dir, 'flake.lock'))
        run_local!(%w[nix flake lock], chdir: conf_dir)
        write_machine_modules!(conf_dir, carrier_port:)
        write_carrier_config!(conf_dir)
        write_nixos_config!(conf_dir, marker: 'A')
        write_vpsadminos_config!(conf_dir, marker: 'A')
        write_admin_ssh_keys!(conf_dir, pubkey)
        write_cluster_modules!(
          conf_dir,
          [
            'carrier/module.nix',
            "#{NIXOS_MACHINE}/module.nix",
            "#{VPSADMINOS_MACHINE}/module.nix"
          ]
        )
        init_fixture_repo!(conf_dir)
      end

      def update_nixos_generation!(marker:)
        write_nixos_config!(@conf_dir, marker:)
      end

      def update_vpsadminos_generation!(marker:)
        write_vpsadminos_config!(@conf_dir, marker:)
      end

      def carrier_success?(cmd, timeout: 30)
        status, = carrier.execute(cmd, timeout:)
        status == 0
      rescue OsVm::Error
        false
      end

      def carrier_realpath(path)
        _, out = carrier.succeeds("realpath #{Shellwords.escape(path)}")
        store_path = extract_store_path(out)
        return store_path unless store_path.nil?

        out.lines.map(&:strip).reject(&:empty?).last.to_s
      end

      def carrier_file(path)
        _, encoded = carrier.succeeds("base64 -w0 < #{Shellwords.escape(path)}")
        Base64.decode64(encoded)
      end

      def carrier_json(path)
        JSON.parse(carrier_file(path))
      end

      def carrier_profile_path(alias_name)
        "/nix/var/nix/profiles/confctl-#{alias_name}"
      end

      def netboot_machine_path(fqdn)
        "/var/lib/confctl/carrier/netboot/http/#{fqdn}/machine.json"
      end

      def netboot_current_path(fqdn)
        "/var/lib/confctl/carrier/netboot/http/#{fqdn}/current"
      end

      def netboot_generation_path(fqdn, generation)
        "/var/lib/confctl/carrier/netboot/http/#{fqdn}/#{generation}/generation.json"
      end

      def netboot_kernel_params_path(fqdn, generation)
        "/var/lib/confctl/carrier/netboot/http/#{fqdn}/#{generation}/kernel-params"
      end

      def carrier_netboot_machine(fqdn)
        carrier_json(netboot_machine_path(fqdn))
      end

      def build_generation!(host)
        confctl!('build', '--yes', host)
        confctl_generation_info(host)
      end

      def wait_for_carrier_netboot_services!
        wait_for_block(name: 'carrier netboot services', timeout: 300) do
          carrier_success?('systemctl is-active --quiet dnsmasq.service') &&
            carrier_success?('systemctl is-active --quiet netboot-atftpd.service') &&
            carrier_success?('systemctl is-active --quiet nginx.service')
        end
      end

      def wait_for_nixos_netboot_artifacts!(generation:, previous_generations: [])
        wait_for_block(name: 'nixos carried netboot artifacts', timeout: 300) do
          next(false) unless carrier_success?("test -e #{Shellwords.escape(carrier_profile_path(NIXOS_MACHINE))}")
          next(false) unless carrier_success?('test -e /var/lib/confctl/carrier/netboot/tftp/pxelinux.cfg/01-52-54-00-10-00-11')
          next(false) unless carrier_success?("test -e #{Shellwords.escape("/var/lib/confctl/carrier/netboot/tftp/pxeserver/machines/#{NIXOS_FQDN}/auto.cfg")}")

          begin
            next(false) unless carrier_realpath(carrier_profile_path(NIXOS_MACHINE)) == generation['toplevel']

            machines_json = carrier_json('/var/lib/confctl/carrier/netboot/http/machines.json')
            next(false) unless machines_json.fetch('machines').any? { |m| m.fetch('name') == NIXOS_MACHINE }

            machine_json = carrier_netboot_machine(NIXOS_FQDN)
            current_generation = machine_json.fetch('generations').detect { |g| g.fetch('current') }
            next(false) if current_generation.nil?
            next(false) unless current_generation.fetch('store_path') == generation['toplevel']
            next(false) unless carrier_success?("test -e #{Shellwords.escape(netboot_generation_path(NIXOS_FQDN, current_generation.fetch('generation')))}")
            next(false) unless carrier_success?("test \"$(basename $(readlink -f #{Shellwords.escape(netboot_current_path(NIXOS_FQDN))}))\" = #{Shellwords.escape(current_generation.fetch('generation').to_s)}")

            next(false) unless previous_generations.all? do |prev_generation|
              prev = machine_json.fetch('generations').detect { |g| g.fetch('store_path') == prev_generation.fetch('toplevel') }
              !prev.nil? && carrier_success?("test -e #{Shellwords.escape(netboot_generation_path(NIXOS_FQDN, prev.fetch('generation')))}")
            end

            nixos_auto = carrier_file("/var/lib/confctl/carrier/netboot/tftp/pxeserver/machines/#{NIXOS_FQDN}/auto.cfg")
            next(false) unless nixos_auto.include?("DEFAULT #{NIXOS_FQDN}")

            nixos_kernel_params = carrier_file(netboot_kernel_params_path(NIXOS_FQDN, current_generation.fetch('generation')))
            next(false) unless nixos_kernel_params.include?('httproot=http://192.168.100.1/')
          rescue JSON::ParserError, OsVm::Error
            next(false)
          end

          true
        end
      end

      def wait_for_vpsadminos_netboot_artifacts!(generation:, previous_generations: [])
        wait_for_block(name: 'vpsadminos carried netboot artifacts', timeout: 300) do
          next(false) unless carrier_success?("test -e #{Shellwords.escape(carrier_profile_path(VPSADMINOS_MACHINE))}")
          next(false) unless carrier_success?('test -e /var/lib/confctl/carrier/netboot/tftp/pxelinux.cfg/01-52-54-00-10-00-12')
          next(false) unless carrier_success?("test -e #{Shellwords.escape("/var/lib/confctl/carrier/netboot/tftp/pxeserver/machines/#{VPSADMINOS_FQDN}/auto.cfg")}")

          begin
            next(false) unless carrier_realpath(carrier_profile_path(VPSADMINOS_MACHINE)) == generation['toplevel']

            machines_json = carrier_json('/var/lib/confctl/carrier/netboot/http/machines.json')
            next(false) unless machines_json.fetch('machines').any? { |m| m.fetch('name') == VPSADMINOS_MACHINE }

            machine_json = carrier_netboot_machine(VPSADMINOS_FQDN)
            current_generation = machine_json.fetch('generations').detect { |g| g.fetch('current') }
            next(false) if current_generation.nil?
            next(false) unless current_generation.fetch('store_path') == generation['toplevel']
            next(false) unless carrier_success?("test -e #{Shellwords.escape(netboot_generation_path(VPSADMINOS_FQDN, current_generation.fetch('generation')))}")
            next(false) unless carrier_success?("test \"$(basename $(readlink -f #{Shellwords.escape(netboot_current_path(VPSADMINOS_FQDN))}))\" = #{Shellwords.escape(current_generation.fetch('generation').to_s)}")

            next(false) unless previous_generations.all? do |prev_generation|
              prev = machine_json.fetch('generations').detect { |g| g.fetch('store_path') == prev_generation.fetch('toplevel') }
              !prev.nil? && carrier_success?("test -e #{Shellwords.escape(netboot_generation_path(VPSADMINOS_FQDN, prev.fetch('generation')))}")
            end

            vpsadminos_kernel_params = carrier_file(netboot_kernel_params_path(VPSADMINOS_FQDN, current_generation.fetch('generation')))
            next(false) unless vpsadminos_kernel_params.include?('httproot=http://192.168.100.1/')
          rescue JSON::ParserError, OsVm::Error
            next(false)
          end

          true
        end
      end

      def boot_pxe_client!(machine, timeout:)
        machine.start(wait_for_boot: false) unless machine.running?
        machine.wait_for_boot(timeout:)
      end

      def wait_for_netboot_http!(machine, timeout:)
        machine.wait_until_succeeds("curl -sf http://#{PXE_ADDRESS}/machines.json >/dev/null", timeout:)
      end

      def boot_id(machine)
        _, out = machine.succeeds('cat /proc/sys/kernel/random/boot_id')
        out.strip
      end

      def wait_for_reboot!(machine, previous_boot_id:, timeout:)
        wait_for_block(name: "#{machine.name} reboot after kexec", timeout:) do
          begin
            _, out = machine.execute('cat /proc/sys/kernel/random/boot_id', timeout: 10)
            next(false) if out.strip == previous_boot_id

            true
          rescue OsVm::MachineShellClosed, OsVm::TimeoutError, OsVm::Error
            next(false)
          end
        end

        machine.wait_for_boot(timeout: 30)
      end

      def kexec_netboot!(machine, load_args: [], boot_timeout:)
        previous_boot_id = boot_id(machine)
        machine.succeeds(Shellwords.join(['kexec-netboot', *load_args]))

        begin
          machine.execute('kexec-netboot --exec', timeout: 120)
        rescue OsVm::MachineShellClosed
          # The old shell can disappear either during the exec call itself or
          # slightly later as the new kernel takes over.
        end

        wait_for_reboot!(machine, previous_boot_id:, timeout: boot_timeout)
      end

      def assert_marker!(machine, marker)
        _, out = machine.succeeds('cat /etc/confctl-marker')
        expect(out).to eq("#{marker}\n")
      end

      before(:suite) do
        @carrier_port = ConfctlHostfwdPorts.reserve('net1')

        carrier.start

        @state_dir = @opts[:state_dir]
        @conf_dir = File.join(@state_dir, 'conf')
        @home_dir = File.join(@state_dir, 'home')
        confctl_setup!(
          bin: "${confctlBin}",
          src: "${confctlSource}",
          conf_dir: @conf_dir,
          home_dir: @home_dir
        )

        @pubkey = setup_ssh_home!(@home_dir)
        install_pubkey!(carrier, @pubkey)

        prepare_fixture!(
          conf_dir: @conf_dir,
          carrier_port: @carrier_port,
          pubkey: @pubkey
        )
      end

      describe 'confctl carrier netboot behavior', order: :defined do
        before(:context) do
          out = wait_for_confctl_connectivity!(expected_successes: 1, timeout: 180)
          expect(out).to include('1 successful')
        end

        it 'deploys the carrier machine for netboot' do
          carrier_generation = build_generation!(CARRIER_MACHINE)
          confctl!('deploy', '--yes', CARRIER_MACHINE)
          wait_for_carrier_netboot_services!
          expect(carrier_realpath('/nix/var/nix/profiles/system')).to eq(carrier_generation['toplevel'])
        end

        it 'deploys the carried NixOS machine to the carrier' do
          @carried_nixos_gen_a = build_generation!("carrier##{NIXOS_MACHINE}")
          confctl!('deploy', '--yes', "carrier##{NIXOS_MACHINE}")
          wait_for_carrier_netboot_services!
          wait_for_nixos_netboot_artifacts!(generation: @carried_nixos_gen_a)
        end

        it 'deploys the carried vpsAdminOS machine to the carrier' do
          @carried_vpsadminos_gen_a = build_generation!("carrier##{VPSADMINOS_MACHINE}")
          confctl!('deploy', '--yes', "carrier##{VPSADMINOS_MACHINE}")
          wait_for_carrier_netboot_services!
          wait_for_vpsadminos_netboot_artifacts!(generation: @carried_vpsadminos_gen_a)
        end

        it 'boots the carried NixOS machine over PXE from the carrier' do
          boot_pxe_client!(nixos, timeout: NIXOS_BOOT_TIMEOUT)
          assert_marker!(nixos, 'A')
          wait_for_netboot_http!(nixos, timeout: NETBOOT_HTTP_TIMEOUT)

          _, cmdline = nixos.succeeds('cat /proc/cmdline')
          expect(cmdline).to include('httproot=http://192.168.100.1/')
        end

        it 'boots the carried vpsAdminOS machine over PXE from the carrier' do
          boot_pxe_client!(vpsadminos, timeout: VPSADMINOS_BOOT_TIMEOUT)
          assert_marker!(vpsadminos, 'A')
          wait_for_netboot_http!(vpsadminos, timeout: NETBOOT_HTTP_TIMEOUT)

          _, cmdline = vpsadminos.succeeds('cat /proc/cmdline')
          expect(cmdline).to include('httproot=http://192.168.100.1/')
        end

        it 'kexec-netboots the carried NixOS machine to the latest and selected generation' do
          expect(@carried_nixos_gen_a).not_to be_nil

          update_nixos_generation!(marker: 'B')
          @carried_nixos_gen_b = build_generation!("carrier##{NIXOS_MACHINE}")
          confctl!('deploy', '--yes', "carrier##{NIXOS_MACHINE}")
          wait_for_carrier_netboot_services!
          wait_for_nixos_netboot_artifacts!(
            generation: @carried_nixos_gen_b,
            previous_generations: [@carried_nixos_gen_a]
          )

          machine_json = carrier_netboot_machine(NIXOS_FQDN)
          gen_a = machine_json.fetch('generations').detect { |g| g.fetch('store_path') == @carried_nixos_gen_a['toplevel'] }
          gen_b = machine_json.fetch('generations').detect { |g| g.fetch('store_path') == @carried_nixos_gen_b['toplevel'] }

          expect(gen_a).not_to be_nil
          expect(gen_b).not_to be_nil
          expect(gen_b.fetch('current')).to be(true)

          kexec_netboot!(nixos, boot_timeout: NIXOS_BOOT_TIMEOUT)
          assert_marker!(nixos, 'B')
          wait_for_netboot_http!(nixos, timeout: NETBOOT_HTTP_TIMEOUT)

          kexec_netboot!(
            nixos,
            load_args: ['--generation', gen_a.fetch('generation').to_s],
            boot_timeout: NIXOS_BOOT_TIMEOUT
          )
          assert_marker!(nixos, 'A')
          wait_for_netboot_http!(nixos, timeout: NETBOOT_HTTP_TIMEOUT)
        end

        it 'kexec-netboots the carried vpsAdminOS machine to the latest and selected generation' do
          expect(@carried_vpsadminos_gen_a).not_to be_nil

          update_vpsadminos_generation!(marker: 'B')
          @carried_vpsadminos_gen_b = build_generation!("carrier##{VPSADMINOS_MACHINE}")
          confctl!('deploy', '--yes', "carrier##{VPSADMINOS_MACHINE}")
          wait_for_carrier_netboot_services!
          wait_for_vpsadminos_netboot_artifacts!(
            generation: @carried_vpsadminos_gen_b,
            previous_generations: [@carried_vpsadminos_gen_a]
          )

          machine_json = carrier_netboot_machine(VPSADMINOS_FQDN)
          gen_a = machine_json.fetch('generations').detect { |g| g.fetch('store_path') == @carried_vpsadminos_gen_a['toplevel'] }
          gen_b = machine_json.fetch('generations').detect { |g| g.fetch('store_path') == @carried_vpsadminos_gen_b['toplevel'] }

          expect(gen_a).not_to be_nil
          expect(gen_b).not_to be_nil
          expect(gen_b.fetch('current')).to be(true)

          kexec_netboot!(vpsadminos, boot_timeout: VPSADMINOS_BOOT_TIMEOUT)
          assert_marker!(vpsadminos, 'B')
          wait_for_netboot_http!(vpsadminos, timeout: NETBOOT_HTTP_TIMEOUT)

          kexec_netboot!(
            vpsadminos,
            load_args: ['--generation', gen_a.fetch('generation').to_s],
            boot_timeout: VPSADMINOS_BOOT_TIMEOUT
          )
          assert_marker!(vpsadminos, 'A')
          wait_for_netboot_http!(vpsadminos, timeout: NETBOOT_HTTP_TIMEOUT)
        end
      end
    '';
  }
)
