import ../../make-test.nix (
  {
    pkgs,
    confctlPackage,
    confctlSrc,
    ...
  }:
  let
    confctlBin =
      if confctlPackage == null then
        throw "suiteArgs.confctlPackage is required"
      else
        "${confctlPackage}/bin/confctl";

    confctlSource = if confctlSrc == null then throw "suiteArgs.confctlSrc is required" else confctlSrc;
  in
  {
    name = "carrier-deploy";

    description = ''
      Verify standalone deploys and carried profile deploys on a NixOS carrier for NixOS and vpsAdminOS machines.
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
            opts = {
              hostForward = "tcp::net1-:22";
              network = "10.0.2.0/24";
              host = "10.0.2.2";
              dns = "10.0.2.3";
            };
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
          networking.firewall.enable = false;
          environment.systemPackages = with pkgs; [
            git
          ];
        };
      };

      nixos = {
        spin = "nixos";
        diskSize = 20480;
        networks = [
          {
            type = "user";
            opts = {
              hostForward = "tcp::net2-:22";
              network = "10.0.2.0/24";
              host = "10.0.2.2";
              dns = "10.0.2.3";
            };
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
          networking.firewall.enable = false;
          environment.systemPackages = with pkgs; [
            git
          ];
        };
      };

      vpsadminos = {
        spin = "vpsadminos";
        networks = [
          {
            type = "user";
            opts = {
              hostForward = "tcp::net3-:22";
              network = "10.0.2.0/24";
              host = "10.0.2.2";
              dns = "10.0.2.3";
            };
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
          boot.qemu = {
            memory = 2048;
            cpus = 4;
          };
          networking.firewall.enable = false;
          environment.systemPackages = with pkgs; [
            git
          ];
        };
      };
    };

    testScript = ''
      require 'fileutils'
      require 'json'

      CARRIER_MACHINE = 'carrier'
      NIXOS_MACHINE = 'nixos-machine'
      VPSADMINOS_MACHINE = 'vpsadminos-machine'

      CARRIED_NIXOS_MACHINE = 'carrier#nixos-machine'
      CARRIED_VPSADMINOS_MACHINE = 'carrier#vpsadminos-machine'

      NIXOS_CARRIED_ENTRIES = %w[machine.json build-mode kernel init]
      VPSADMINOS_CARRIED_ENTRIES = %w[machine.json build-mode bzImage initrd root.squashfs kernel-params]

      def write_flake_root!(conf_dir)
        File.write(File.join(conf_dir, 'flake.nix'), <<~NIX)
          {
            description = "confctl carrier deploy test fixture";

            inputs = {
              confctl.url = "path:#{confctl_src}";
              nixpkgs.follows = "confctl/nixpkgs";
              vpsadminos.follows = "confctl/vpsadminos";
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

      def write_nixos_config!(conf_dir, machine_name, marker:, carrier_enabled:, build_dist:)
        write_hardware_config!(conf_dir, machine_name)

        carrier_config =
          if carrier_enabled
            <<~NIX
              confctl.carrier.onChangeCommands = '''
                {
                  echo "on-change"
                  for profile in /nix/var/nix/profiles/confctl-*; do
                    [ -e "$profile" ] || continue
                    echo "$profile -> $(readlink "$profile")"
                  done
                } >> /var/log/confctl-carrier-events.log
              ''';
            NIX
          else
            ""
          end

        dist_config =
          if build_dist
            <<~NIX
              system.build.dist = pkgs.symlinkJoin {
                name = "#{machine_name}-carried";
                paths = [
                  config.system.build.toplevel
                ];
                postBuild = '''
                  ln -s ''${machineJson} $out/machine.json
                  ln -s ''${buildMode} $out/build-mode
                ''';
              };
            NIX
          else
            ""
          end

        File.write(File.join(conf_dir, "cluster/#{machine_name}/config.nix"), <<~NIX)
          {
            config,
            pkgs,
            lib,
            inputsInfo,
            inputs,
            ...
          }:
          let
            machineJson = pkgs.writeText "machine-#{machine_name}.json" (builtins.toJSON {
              machine = "#{machine_name}";
              marker = "#{marker}";
              spin = "nixos";
              label = "#{machine_name}";
              toplevel = builtins.unsafeDiscardStringContext config.system.build.toplevel;
              inputsInfo = inputsInfo;
            });

            buildMode = pkgs.writeText "build-mode-#{machine_name}" "carried\n";
          in
          {
            imports = [
              ../../environments/base.nix
              ./hardware.nix
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
              (inputs.vpsadminos + "/tests/configs/nixos/base.nix")
            ];

            networking.hostName = "#{machine_name}";
            time.timeZone = lib.mkForce "UTC";
            boot.loader.grub.enable = false;

            fileSystems."/" = {
              device = "/dev/disk/by-label/nixos";
              fsType = "ext4";
            };

            environment.etc."confctl-marker".text = "#{marker}\n";

            #{dist_config}
            #{carrier_config}
          }
        NIX
      end

      def write_vpsadminos_config!(conf_dir, marker:)
        write_hardware_config!(conf_dir, VPSADMINOS_MACHINE)

        File.write(File.join(conf_dir, "cluster/#{VPSADMINOS_MACHINE}/config.nix"), <<~NIX)
          {
            config,
            pkgs,
            lib,
            inputsInfo,
            inputs,
            ...
          }:
          let
            machineJson = pkgs.writeText "machine-#{VPSADMINOS_MACHINE}.json" (builtins.toJSON {
              machine = "#{VPSADMINOS_MACHINE}";
              marker = "#{marker}";
              spin = "vpsadminos";
              label = "#{VPSADMINOS_MACHINE}";
              toplevel = builtins.unsafeDiscardStringContext config.system.build.toplevel;
              inputsInfo = inputsInfo;
            });

            buildMode = pkgs.writeText "build-mode-#{VPSADMINOS_MACHINE}" "carried\n";
          in
          {
            imports = [
              ../../environments/base.nix
              ./hardware.nix
              (inputs.vpsadminos + "/tests/configs/vpsadminos/base.nix")
            ];

            networking.hostName = "#{VPSADMINOS_MACHINE}";

            boot.loader.grub.enable = false;
            boot.supportedFilesystems = [ "zfs" ];
            boot.kernelParams = [ "nolive" ];
            boot.zfs.pools = { };

            environment.etc."confctl-marker".text = "#{marker}\n";

            system.distBuilderCommands = '''
              ln -s ''${machineJson} $out/machine.json
              ln -s ''${buildMode} $out/build-mode
            ''';

            system.stateVersion = "20.09";
          }
        NIX
      end

      def write_machine_modules!(conf_dir, carrier_port:, nixos_port:, vpsadminos_port:)
        FileUtils.mkdir_p(File.join(conf_dir, 'cluster', 'carrier'))
        FileUtils.mkdir_p(File.join(conf_dir, 'cluster', NIXOS_MACHINE))
        FileUtils.mkdir_p(File.join(conf_dir, 'cluster', VPSADMINOS_MACHINE))

        File.write(File.join(conf_dir, 'cluster/carrier/module.nix'), <<~NIX)
          { config, ... }:
          {
            cluster."carrier" = {
              spin = "nixos";
              inputs.channels = [ "carrier" ];
              host.target = "127.0.0.1";
              host.port = #{carrier_port};
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
              host.target = "127.0.0.1";
              host.port = #{nixos_port};
              healthChecks.machineCommands = [
                {
                  description = "true";
                  command = [ "true" ];
                }
              ];
            };
          }
        NIX

        File.write(File.join(conf_dir, "cluster/#{VPSADMINOS_MACHINE}/module.nix"), <<~NIX)
          { config, ... }:
          {
            cluster."#{VPSADMINOS_MACHINE}" = {
              spin = "vpsadminos";
              inputs.channels = [ "carrier" ];
              host.target = "127.0.0.1";
              host.port = #{vpsadminos_port};
              healthChecks.machineCommands = [
                {
                  description = "true";
                  command = [ "true" ];
                }
              ];
            };
          }
        NIX
      end

      def prepare_fixture!(conf_dir:, carrier_port:, nixos_port:, vpsadminos_port:, pubkey:)
        prepare_fixture_dir!(conf_dir)
        write_flake_root!(conf_dir)
        write_machine_modules!(
          conf_dir,
          carrier_port:,
          nixos_port:,
          vpsadminos_port:
        )
        write_nixos_config!(conf_dir, CARRIER_MACHINE, marker: 'A', carrier_enabled: true, build_dist: false)
        write_nixos_config!(conf_dir, NIXOS_MACHINE, marker: 'A', carrier_enabled: false, build_dist: true)
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

      def machine_state(machine_name)
        confctl_machine_state(machine_name)
      end

      def assert_machine_state(machine_name, profile:, current:)
        expect(confctl_remote_realpath(machine_name, '/nix/var/nix/profiles/system')).to eq(profile)
        expect(confctl_remote_realpath(machine_name, '/run/current-system')).to eq(current)
      end

      def carrier_profile_path(alias_name)
        "/nix/var/nix/profiles/confctl-#{alias_name}"
      end

      def assert_carried_generation_matches!(carried_generation, standalone_generation, machine_name:, marker:, spin:, expected_entries:)
        expect(carried_generation['toplevel']).not_to eq(standalone_generation['toplevel'])
        expect(File.read(File.join(carried_generation['toplevel'], 'build-mode'))).to eq("carried\n")

        machine_json = JSON.parse(File.read(File.join(carried_generation['toplevel'], 'machine.json')))
        expect(machine_json['machine']).to eq(machine_name)
        expect(machine_json['marker']).to eq(marker)
        expect(machine_json['spin']).to eq(spin)
        expect(machine_json['toplevel']).to eq(standalone_generation['toplevel'])

        expected_entries.each do |entry|
          expect(File.exist?(File.join(carried_generation['toplevel'], entry))).to be(true)
        end
      end

      def assert_carried_profile!(alias_name, generation:, number:, expected_entries:)
        profile = carrier_profile_path(alias_name)
        generation_link = "#{profile}-#{number}-link"

        expect(confctl_remote_realpath(CARRIER_MACHINE, profile)).to eq(generation['toplevel'])
        expect(confctl_remote_realpath(CARRIER_MACHINE, generation_link)).to eq(generation['toplevel'])

        expected_entries.each do |entry|
          confctl_store_path_exists!(CARRIER_MACHINE, File.join(generation['toplevel'], entry))
        end

        confctl_ssh!(CARRIER_MACHINE, 'test', '-e', generation_link)
      end

      def assert_previous_carried_generation!(alias_name, generation:, number:)
        generation_link = "#{carrier_profile_path(alias_name)}-#{number}-link"
        expect(confctl_remote_realpath(CARRIER_MACHINE, generation_link)).to eq(generation['toplevel'])
      end

      def build_generation!(host)
        confctl!('build', '--yes', host)
        confctl_generation_info(host)
      end

      before(:suite) do
        @carrier_port = ConfctlHostfwdPorts.reserve('net1')
        @nixos_port = ConfctlHostfwdPorts.reserve('net2')
        @vpsadminos_port = ConfctlHostfwdPorts.reserve('net3')

        carrier.start
        nixos.start
        vpsadminos.start

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
        install_pubkey!(nixos, @pubkey)
        install_pubkey!(vpsadminos, @pubkey)

        prepare_fixture!(
          conf_dir: @conf_dir,
          carrier_port: @carrier_port,
          nixos_port: @nixos_port,
          vpsadminos_port: @vpsadminos_port,
          pubkey: @pubkey
        )
      end

      describe 'confctl carrier deploy behavior', order: :defined do
        before(:context) do
          out = wait_for_confctl_connectivity!(expected_successes: 3, timeout: 180)
          expect(out).to include('3 successful')
        end

        it 'builds and deploys standalone generations for carrier, NixOS and vpsAdminOS machines' do
          @carrier_gen_a = build_generation!(CARRIER_MACHINE)
          @nixos_gen_a = build_generation!(NIXOS_MACHINE)
          @vpsadminos_gen_a = build_generation!(VPSADMINOS_MACHINE)

          expect(File.executable?(File.join(@carrier_gen_a['toplevel'], 'bin', 'switch-to-configuration'))).to be(true)
          expect(File.executable?(File.join(@nixos_gen_a['toplevel'], 'bin', 'switch-to-configuration'))).to be(true)

          confctl!('deploy', '--yes', CARRIER_MACHINE)
          confctl!('deploy', '--yes', NIXOS_MACHINE)
          confctl!('deploy', '--yes', VPSADMINOS_MACHINE)

          assert_machine_state(
            CARRIER_MACHINE,
            profile: @carrier_gen_a['toplevel'],
            current: @carrier_gen_a['toplevel']
          )
          assert_machine_state(
            NIXOS_MACHINE,
            profile: @nixos_gen_a['toplevel'],
            current: @nixos_gen_a['toplevel']
          )
          assert_machine_state(
            VPSADMINOS_MACHINE,
            profile: @vpsadminos_gen_a['toplevel'],
            current: @vpsadminos_gen_a['toplevel']
          )
        end

        it 'builds carried NixOS and vpsAdminOS generations from the same source machines' do
          @carried_nixos_gen_a = build_generation!(CARRIED_NIXOS_MACHINE)
          @carried_vpsadminos_gen_a = build_generation!(CARRIED_VPSADMINOS_MACHINE)

          assert_carried_generation_matches!(
            @carried_nixos_gen_a,
            @nixos_gen_a,
            machine_name: NIXOS_MACHINE,
            marker: 'A',
            spin: 'nixos',
            expected_entries: NIXOS_CARRIED_ENTRIES
          )
          assert_carried_generation_matches!(
            @carried_vpsadminos_gen_a,
            @vpsadminos_gen_a,
            machine_name: VPSADMINOS_MACHINE,
            marker: 'A',
            spin: 'vpsadminos',
            expected_entries: VPSADMINOS_CARRIED_ENTRIES
          )
        end

        it 'deploys carried generations to carrier-managed profiles' do
          confctl!('deploy', '--yes', CARRIED_NIXOS_MACHINE)
          confctl!('deploy', '--yes', CARRIED_VPSADMINOS_MACHINE)

          assert_carried_profile!(
            NIXOS_MACHINE,
            generation: @carried_nixos_gen_a,
            number: 1,
            expected_entries: NIXOS_CARRIED_ENTRIES
          )
          assert_carried_profile!(
            VPSADMINOS_MACHINE,
            generation: @carried_vpsadminos_gen_a,
            number: 1,
            expected_entries: VPSADMINOS_CARRIED_ENTRIES
          )

          assert_machine_state(
            CARRIER_MACHINE,
            profile: @carrier_gen_a['toplevel'],
            current: @carrier_gen_a['toplevel']
          )
          assert_machine_state(
            NIXOS_MACHINE,
            profile: @nixos_gen_a['toplevel'],
            current: @nixos_gen_a['toplevel']
          )
          assert_machine_state(
            VPSADMINOS_MACHINE,
            profile: @vpsadminos_gen_a['toplevel'],
            current: @vpsadminos_gen_a['toplevel']
          )

          log_out, = confctl_ssh!(CARRIER_MACHINE, 'cat', '/var/log/confctl-carrier-events.log')
          expect(log_out).to include("/nix/var/nix/profiles/confctl-#{NIXOS_MACHINE}")
          expect(log_out).to include("/nix/var/nix/profiles/confctl-#{VPSADMINOS_MACHINE}")
        end

        it 'builds updated standalone and carried generations for all machines' do
          write_nixos_config!(@conf_dir, CARRIER_MACHINE, marker: 'B', carrier_enabled: true, build_dist: false)
          write_nixos_config!(@conf_dir, NIXOS_MACHINE, marker: 'B', carrier_enabled: false, build_dist: true)
          write_vpsadminos_config!(@conf_dir, marker: 'B')

          @carrier_gen_b = build_generation!(CARRIER_MACHINE)
          @nixos_gen_b = build_generation!(NIXOS_MACHINE)
          @vpsadminos_gen_b = build_generation!(VPSADMINOS_MACHINE)
          @carried_nixos_gen_b = build_generation!(CARRIED_NIXOS_MACHINE)
          @carried_vpsadminos_gen_b = build_generation!(CARRIED_VPSADMINOS_MACHINE)

          expect(@carrier_gen_b['toplevel']).not_to eq(@carrier_gen_a['toplevel'])
          expect(@nixos_gen_b['toplevel']).not_to eq(@nixos_gen_a['toplevel'])
          expect(@vpsadminos_gen_b['toplevel']).not_to eq(@vpsadminos_gen_a['toplevel'])

          assert_carried_generation_matches!(
            @carried_nixos_gen_b,
            @nixos_gen_b,
            machine_name: NIXOS_MACHINE,
            marker: 'B',
            spin: 'nixos',
            expected_entries: NIXOS_CARRIED_ENTRIES
          )
          assert_carried_generation_matches!(
            @carried_vpsadminos_gen_b,
            @vpsadminos_gen_b,
            machine_name: VPSADMINOS_MACHINE,
            marker: 'B',
            spin: 'vpsadminos',
            expected_entries: VPSADMINOS_CARRIED_ENTRIES
          )
        end

        it 'deploys standalone updates without disturbing carried profiles' do
          confctl!('deploy', '--yes', '--generation', @carrier_gen_b['name'], CARRIER_MACHINE, 'switch')
          confctl!('deploy', '--yes', '--generation', @nixos_gen_b['name'], NIXOS_MACHINE, 'switch')
          confctl!('deploy', '--yes', '--generation', @vpsadminos_gen_b['name'], VPSADMINOS_MACHINE, 'switch')

          assert_machine_state(
            CARRIER_MACHINE,
            profile: @carrier_gen_b['toplevel'],
            current: @carrier_gen_b['toplevel']
          )
          assert_machine_state(
            NIXOS_MACHINE,
            profile: @nixos_gen_b['toplevel'],
            current: @nixos_gen_b['toplevel']
          )
          assert_machine_state(
            VPSADMINOS_MACHINE,
            profile: @vpsadminos_gen_b['toplevel'],
            current: @vpsadminos_gen_b['toplevel']
          )

          assert_carried_profile!(
            NIXOS_MACHINE,
            generation: @carried_nixos_gen_a,
            number: 1,
            expected_entries: NIXOS_CARRIED_ENTRIES
          )
          assert_carried_profile!(
            VPSADMINOS_MACHINE,
            generation: @carried_vpsadminos_gen_a,
            number: 1,
            expected_entries: VPSADMINOS_CARRIED_ENTRIES
          )
        end

        it 'deploys updated carried generations and keeps both carrier profile generations' do
          confctl!('deploy', '--yes', '--generation', @carried_nixos_gen_b['name'], CARRIED_NIXOS_MACHINE)
          confctl!('deploy', '--yes', '--generation', @carried_vpsadminos_gen_b['name'], CARRIED_VPSADMINOS_MACHINE)

          assert_carried_profile!(
            NIXOS_MACHINE,
            generation: @carried_nixos_gen_b,
            number: 2,
            expected_entries: NIXOS_CARRIED_ENTRIES
          )
          assert_carried_profile!(
            VPSADMINOS_MACHINE,
            generation: @carried_vpsadminos_gen_b,
            number: 2,
            expected_entries: VPSADMINOS_CARRIED_ENTRIES
          )
          assert_previous_carried_generation!(NIXOS_MACHINE, generation: @carried_nixos_gen_a, number: 1)
          assert_previous_carried_generation!(VPSADMINOS_MACHINE, generation: @carried_vpsadminos_gen_a, number: 1)

          assert_machine_state(
            CARRIER_MACHINE,
            profile: @carrier_gen_b['toplevel'],
            current: @carrier_gen_b['toplevel']
          )
          assert_machine_state(
            NIXOS_MACHINE,
            profile: @nixos_gen_b['toplevel'],
            current: @nixos_gen_b['toplevel']
          )
          assert_machine_state(
            VPSADMINOS_MACHINE,
            profile: @vpsadminos_gen_b['toplevel'],
            current: @vpsadminos_gen_b['toplevel']
          )
        end

        it 'keeps all standalone machines reachable at the end' do
          out = wait_for_confctl_connectivity!(expected_successes: 3, timeout: 180)
          expect(out).to include('3 successful')
        end
      end
    '';
  }
)
