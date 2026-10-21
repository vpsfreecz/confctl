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
      require 'shellwords'

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

            environment.etc."confctl-test-rust-package".text = toString config._module.args.confctlRustPackage;

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
                  rustHelpers = true;
                };
              in
              {
                confctl = confctlOutputs;

                devShells.x86_64-linux.default = confctl.lib.mkConfigDevShell {
                  system = "x86_64-linux";
                  mode = "minimal";
                  implementation = "rust";
                };
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
          assert_rust_helper!(nixos, 'confctl-auto-rollback', wrapper: @baseline_gen.fetch('auto_rollback'))

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
        it 'confirms a new CLI deployment using the immutable old rollback helper' do
          original = File.binread(File.join(@conf_dir, 'flake.nix'))
          override = <<~NIX.strip
            packages.x86_64-linux.m5-legacy-rollback =
              let pkgs = inputs.nixpkgs.legacyPackages.x86_64-linux;
              in pkgs.replaceVarsWith {
                src = confctl + "/tests/compat/helpers/baseline/auto-rollback.rb.txt";
                isExecutable = true;
                replacements.ruby = pkgs.ruby;
              };
            confctl = confctlOutputs // {
              build = builtins.mapAttrs (key: generation:
                if key == confctlOutputs.machineKeys."nixos-machine" then
                  generation // { autoRollback = self.packages.x86_64-linux.m5-legacy-rollback; }
                else generation) confctlOutputs.build;
            };
          NIX
          expect(original.scan('confctl = confctlOutputs;').size).to eq(1)
          begin
            File.write(File.join(@conf_dir, 'flake.nix'), original.sub('confctl = confctlOutputs;', override))
            write_nixos_config!(@conf_dir, marker: 'legacy-confirm')
            confctl!('build', '--yes', NIXOS_MACHINE)
            saved = confctl_generation_info(NIXOS_MACHINE)
            legacy_helper, = run_local!(
              ['nix', 'eval', '--raw', '--no-write-lock-file', '.#packages.x86_64-linux.m5-legacy-rollback.outPath'],
              chdir: @conf_dir, env: confctl_env
            )
            expect(saved.fetch('auto_rollback')).to eq(legacy_helper)
            out, = confctl!('deploy', '--yes', '--generation', saved.fetch('name'), NIXOS_MACHINE)
            assert_machine_state(NIXOS_MACHINE, profile: saved.fetch('toplevel'), current: saved.fetch('toplevel'))
            _, marker = nixos.succeeds('cat /etc/confctl-marker')
            expect(marker).to eq("legacy-confirm\n")
            checks = out.scan(%r{/run/confctl-confirm-[0-9a-f]+}).uniq
            expect(checks).not_to be_empty
            checks.each { |path| nixos.succeeds("test ! -e #{Shellwords.escape(path)}") }
          ensure
            File.binwrite(File.join(@conf_dir, 'flake.nix'), original)
          end
        end

        it 'rolls back after the owned SSH transport closes beyond the switched boundary' do
          deploy_with_retry!('--generation', @baseline_gen.fetch('name'), NIXOS_MACHINE, 'switch')
          write_nixos_config!(@conf_dir, marker: 'ssh-disconnect')
          confctl!('build', '--yes', NIXOS_MACHINE)
          changed = confctl_generation_info(NIXOS_MACHINE)
          confctl!('deploy', '--yes', '--copy-only', '--generation', changed.fetch('name'), NIXOS_MACHINE)
          backend = assert_rust_helper!(nixos, 'confctl-auto-rollback', wrapper: changed.fetch('auto_rollback'))
          check = '/run/confctl-m5-disconnect'
          nixos.succeeds("test ! -e #{check}")
          remote = Shellwords.join([changed.fetch('auto_rollback'), '--timeout', '90', changed.fetch('toplevel'), 'test', check])
          ssh = ['ssh', '-F', File.join(home_dir, '.ssh/config'), '-p', @port.to_s, 'root@127.0.0.1', remote]
          worker = nil
          transcript = []
          Open3.popen3(confctl_env, *ssh) do |input, output, error, wait|
            input.close
            readers = [output, error].map { |stream| Thread.new { stream.read } }
            begin
              wait_for_block(name: 'detached worker switched while SSH is live', timeout: 60) do
                raise "SSH exited before switched: #{wait.value.inspect}" unless wait.alive?
                _, token = nixos.execute("cat #{check} 2>/dev/null")
                _, current = nixos.succeeds('readlink -e /run/current-system')
                next false unless token == 'switched' && current.strip == changed.fetch('toplevel')

                _, candidates = nixos.succeeds(<<~SH)
                  for proc in /proc/[0-9]*; do
                    [ "$(readlink "$proc/exe" 2>/dev/null)" = #{Shellwords.escape(backend)} ] || continue
                    tr '\\000' '\\n' < "$proc/cmdline" | grep -Fx -- #{check} >/dev/null || continue
                    tr '\\000' '\\n' < "$proc/cmdline" | grep -Fx -- --__confctl-auto-rollback-worker >/dev/null || continue
                    cat "$proc/stat"
                  done
                SH
                rows = candidates.lines
                raise "expected one detached worker: #{candidates.inspect}" unless rows.size == 1
                row = rows.fetch(0)
                worker = [Integer(row.split.first), row.rpartition(')').last.split.fetch(19)]
                true
              end
              Process.kill('TERM', wait.pid)
              raise 'owned SSH did not terminate after TERM' unless wait.join(10)
              puts "disconnected SSH pid=#{wait.pid} status=#{wait.value.inspect}; worker=#{worker.inspect}"
            ensure
              if wait.alive?
                begin
                  Process.kill('TERM', wait.pid)
                rescue Errno::ESRCH
                  # The owned SSH child exited before this cleanup signal.
                end
                unless wait.join(10)
                  begin
                    Process.kill('KILL', wait.pid)
                  rescue Errno::ESRCH
                    # Its wait thread still owns the direct-child reap.
                  end
                  raise 'owned SSH cleanup did not reap' unless wait.join(10)
                end
              end
              readers.each do |reader|
                raise 'SSH stream did not finish after child reap' unless reader.join(10)
                transcript << reader.value
              end
              puts "SSH stdout/stderr: #{transcript.inspect}; worker=#{worker.inspect}"
            end
          end
          expect(worker).not_to be_nil
          wait_for_block(name: 'post-disconnect rollback and worker termination', timeout: 150) do
            _, current = nixos.succeeds('readlink -e /run/current-system')
            _, stat = nixos.execute("cat /proc/#{worker.fetch(0)}/stat 2>/dev/null")
            gone = stat.empty? || stat.rpartition(')').last.split.fetch(19) != worker.fetch(1)
            current.strip == @baseline_state.fetch(:current) && gone
          end
          assert_machine_state(NIXOS_MACHINE, profile: @baseline_state.fetch(:profile), current: @baseline_state.fetch(:current))
          _, marker = nixos.succeeds('cat /etc/confctl-marker')
          expect(marker).to eq("baseline\n")
          expect(wait_for_confctl_connectivity!(expected_successes: 1, timeout: 120)).to include('1 successful')
        end
      end
    '';
  }
)
