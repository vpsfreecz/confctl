import ../make-test.nix (
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
    name = "auto_rollback";

    description = ''
      Verify deterministic auto-rollback for multiple failure modes.
    '';

    tags = [
      "ci"
    ];

    machines = {
      nixos = {
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
    };

    testScript = ''
      require 'fileutils'

      NIXOS_MACHINE = 'nixos-machine'

      FAILURE_NETWORK_CONFIG = {
        'flush_addresses' => <<~NIX,
          networking.useDHCP = false;
          networking.dhcpcd.enable = false;
          networking.interfaces.eth0.useDHCP = false;
          networking.interfaces.eth0.ipv4.addresses = [ ];
          networking.interfaces.eth0.ipv6.addresses = [ ];
        NIX
        'flush_routes' => <<~NIX,
          networking.useDHCP = false;
          networking.dhcpcd.enable = false;
          networking.interfaces.eth0.useDHCP = false;
          networking.interfaces.eth0.ipv4.addresses = [
            {
              address = "10.0.2.15";
              prefixLength = 32;
            }
          ];
          networking.interfaces.eth0.ipv6.addresses = [ ];
        NIX
        'link_down' => <<~NIX
          systemd.services.confctl-break-link-down = {
            description = "Break network by forcing eth0 down";
            wantedBy = [ "multi-user.target" ];
            after = [ "network-online.target" "sshd.service" ];
            serviceConfig = {
              Type = "oneshot";
              RemainAfterExit = true;
              ExecStart = "${pkgs.iproute2}/bin/ip link set dev eth0 down";
              ExecStop = "${pkgs.iproute2}/bin/ip link set dev eth0 up";
            };
          };
        NIX
      }

      def network_breaker_nix(mode)
        return "" if mode.nil?

        FAILURE_NETWORK_CONFIG.fetch(mode)
      end

      def write_nixos_config!(conf_dir, marker:, failure_mode: nil)
        breaker = network_breaker_nix(failure_mode)

        File.write(File.join(conf_dir, 'cluster/nixos-machine/config.nix'), <<~NIX)
          {
            config,
            pkgs,
            lib,
            inputs,
            ...
          }:
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

            networking.hostName = "nixos-machine";
            time.timeZone = lib.mkForce "UTC";
            boot.loader.grub.enable = false;

            fileSystems."/" = {
              device = "/dev/disk/by-label/nixos";
              fsType = "ext4";
            };

            environment.etc."confctl-marker".text = "#{marker}\n";
            #{breaker}
          }
        NIX
      end

      def prepare_fixture!(conf_dir:, port:, pubkey:)
        prepare_fixture_dir!(conf_dir)

        File.write(File.join(conf_dir, 'flake.nix'), <<~NIX)
          {
            description = "confctl auto-rollback test fixture";

            inputs = {
              confctl.url = "path:#{confctl_src}";
              nixpkgs.follows = "confctl/nixpkgs";
              vpsadminos.follows = "confctl/vpsadminos";
            };

            outputs = inputs@{ self, confctl, ... }:
              let
                channels = {
                  nixos = {
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

                devShells.x86_64-linux.default = confctl.lib.mkDevShell { system = "x86_64-linux"; };
              };
          }
        NIX

        File.write(File.join(conf_dir, 'cluster/nixos-machine/module.nix'), <<~NIX)
          { config, ... }:
          {
            cluster."nixos-machine" = {
              spin = "nixos";
              inputs.channels = [ "nixos" ];
              host.target = "127.0.0.1";
              host.port = #{port};
              autoRollback.timeout = 15;
            };
          }
        NIX

        write_nixos_config!(conf_dir, marker: 'baseline', failure_mode: nil)

        write_cluster_modules!(conf_dir, [ 'nixos-machine/module.nix' ])
        write_admin_ssh_keys!(conf_dir, pubkey)
        init_fixture_repo!(conf_dir)
      end

      def machine_state(machine_name)
        confctl_machine_state(machine_name)
      end

      def assert_machine_state(machine_name, profile:, current:)
        expect(confctl_remote_realpath(machine_name, '/nix/var/nix/profiles/system')).to eq(profile)
        expect(confctl_remote_realpath(machine_name, '/run/current-system')).to eq(current)
      end

      def assert_store_path_exists(machine_name, store_path)
        confctl_store_path_exists!(machine_name, store_path)
      end

      def deploy_with_retry!(*deploy_args, attempts: 3)
        last_error = nil

        attempts.times do |i|
          begin
            return confctl!('deploy', '--yes', *deploy_args)
          rescue StandardError => e
            last_error = e
            raise if i + 1 >= attempts

            sleep(2)
            wait_for_confctl_connectivity!(expected_successes: 1, timeout: 120)
          end
        end

        raise(last_error || 'deploy_with_retry! failed')
      end

      def exercise_failure_mode!(mode, idx, baseline_state:, baseline_gen:)
        deploy_with_retry!('--generation', baseline_gen['name'], NIXOS_MACHINE, 'switch')
        assert_machine_state(
          NIXOS_MACHINE,
          profile: baseline_state[:profile],
          current: baseline_state[:current]
        )

        write_nixos_config!(@conf_dir, marker: "#{mode}-#{idx}", failure_mode: mode)
        confctl!('build', '--yes', NIXOS_MACHINE)
        broken_gen = confctl_generation_info(NIXOS_MACHINE)

        expect(broken_gen['toplevel']).not_to eq(baseline_gen['toplevel'])

        _out, _err, status = confctl('deploy', '--yes', '--generation', 'current', NIXOS_MACHINE)
        expect(status.success?).to be(false)

        wait_for_block(name: "rollback after #{mode}", timeout: 240) do
          state = machine_state(NIXOS_MACHINE)
          state[:current] == baseline_state[:current] && state[:profile] == baseline_state[:profile]
        end

        assert_machine_state(
          NIXOS_MACHINE,
          profile: baseline_state[:profile],
          current: baseline_state[:current]
        )
        assert_store_path_exists(NIXOS_MACHINE, broken_gen['toplevel'])

        out = wait_for_confctl_connectivity!(expected_successes: 1, timeout: 180)
        expect(out).to include('1 successful')

        confctl_ssh!(NIXOS_MACHINE, 'sh', '-c', 'umount /run/confctl || true')
      end

      before(:suite) do
        @port = ConfctlHostfwdPorts.reserve('net1')
        nixos.start

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
        install_pubkey!(nixos, @pubkey)
        prepare_fixture!(conf_dir: @conf_dir, port: @port, pubkey: @pubkey)
      end

      describe 'auto rollback', order: :defined do
        before(:context) do
          out = wait_for_confctl_connectivity!(expected_successes: 1, timeout: 180)
          expect(out).to include('1 successful')

          deploy_with_retry!(NIXOS_MACHINE)
          @baseline_gen = confctl_generation_info(NIXOS_MACHINE)
          @baseline_state = machine_state(NIXOS_MACHINE)

          expect(@baseline_gen['toplevel']).to eq(@baseline_state[:current])
          expect(@baseline_gen['toplevel']).to eq(@baseline_state[:profile])
        end

        it 'rolls back when IPv4 and IPv6 addresses are removed' do
          exercise_failure_mode!(
            'flush_addresses',
            0,
            baseline_state: @baseline_state,
            baseline_gen: @baseline_gen
          )
        end

        it 'rolls back when routes are removed' do
          exercise_failure_mode!(
            'flush_routes',
            1,
            baseline_state: @baseline_state,
            baseline_gen: @baseline_gen
          )
        end

        it 'rolls back when interface link is brought down' do
          exercise_failure_mode!(
            'link_down',
            2,
            baseline_state: @baseline_state,
            baseline_gen: @baseline_gen
          )
        end
      end
    '';
  }
)
