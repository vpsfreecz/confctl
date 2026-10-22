{ testFramework, ... }@args:
let
  vpsadminosSource = testFramework.sourcePath;
in
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
    name = "deploy-flakes";

    description = ''
      Exercise confctl build/deploy lifecycle on NixOS and vpsAdminOS machines (flakes mode).
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
      vpsadminos = {
        spin = "vpsadminos";
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
      require 'digest'
      require 'fileutils'
      require 'rbconfig'

      NIXPKGS_PATH = '${pkgs.path}'
      VPSADMINOS_PATH = '${vpsadminosSource}'

      NIXOS_MACHINE = 'nixos-machine'
      VPSADMINOS_MACHINE = 'vpsadminos-machine'

      def git_commit!(repo, file, content, message)
        File.write(File.join(repo, file), content)
        run_local!(%w[git add .], chdir: repo)
        run_local!(['git', 'commit', '-m', message], chdir: repo)
        out, = run_local!(%w[git rev-parse HEAD], chdir: repo)
        out.strip
      end

      def setup_dummy_repo!(dummy_repo)
        FileUtils.rm_rf(dummy_repo)
        FileUtils.mkdir_p(dummy_repo)
        run_local!(%w[git init], chdir: dummy_repo)
        run_local!(%w[git config user.email confctl-tests@example.invalid], chdir: dummy_repo)
        run_local!(%w[git config user.name confctl-tests], chdir: dummy_repo)
        File.write(File.join(dummy_repo, 'flake.nix'), <<~NIX)
          {
            description = "dummy input for confctl tests";
            outputs = { self }: { };
          }
        NIX
        File.write(File.join(dummy_repo, 'dummy.txt'), "A\n")
        run_local!(%w[git add .], chdir: dummy_repo)
        run_local!(['git', 'commit', '-m', 'dummy: A'], chdir: dummy_repo)
        out, = run_local!(%w[git rev-parse HEAD], chdir: dummy_repo)
        out.strip
      end

      def bump_dummy_repo!(dummy_repo, label)
        out, = run_local!(%w[git rev-list --count HEAD], chdir: dummy_repo)
        n = out.strip.to_i + 1
        git_commit!(dummy_repo, 'dummy.txt', "#{label} #{n}\n", "dummy: #{label} #{n}")
      end

      def write_nixos_config!(conf_dir, marker:)
        vpsadminos_import =
          '(inputs.vpsadminos + "/tests/configs/nixos/base.nix")'

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
              #{vpsadminos_import}
            ];

            networking.hostName = "nixos-machine";
            time.timeZone = lib.mkForce "UTC";
            boot.loader.grub.enable = false;

            fileSystems."/" = {
              device = "/dev/disk/by-label/nixos";
              fsType = "ext4";
            };

            environment.etc."confctl-test-rust-package".text = toString config._module.args.confctlRustPackage;

            environment.etc."confctl-marker-nixos".text = "#{marker}\n";
          }
        NIX
      end

      def write_vpsadminos_config!(conf_dir, marker:)
        vpsadminos_import =
          '(inputs.vpsadminos + "/tests/configs/vpsadminos/base.nix")'

        File.write(File.join(conf_dir, 'cluster/vpsadminos-machine/config.nix'), <<~NIX)
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
              #{vpsadminos_import}
            ];

            networking.hostName = "vpsadminos-machine";

            boot.loader.grub.enable = false;

            boot.supportedFilesystems = [ "zfs" ];
            boot.kernelParams = [ "nolive" ];
            boot.zfs.pools = { };

            environment.etc."confctl-test-rust-package".text = toString config._module.args.confctlRustPackage;

            environment.etc."confctl-marker-vpsadminos".text = "#{marker}\n";

            system.stateVersion = "20.09";
          }
        NIX
      end

      def write_flake_root!(conf_dir, dummy_repo)
        File.write(File.join(conf_dir, 'flake.nix'), <<~NIX)
          {
            description = "confctl test fixture (flake)";

            inputs = {
              confctl.url = "path:#{confctl_src}";
              nixpkgs.follows = "confctl/nixpkgs";
              vpsadminos.follows = "confctl/vpsadminos";
              dummy-input.url = "git+file://#{dummy_repo}";
            };

            outputs = inputs@{ self, confctl, ... }:
              let
                channels = {
                  deploy = {
                    nixpkgs = "nixpkgs";
                    vpsadminos = "vpsadminos";
                    dummy = "dummy-input";
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
      end

      def write_machine_modules!(conf_dir, nixos_port:, vpsadminos_port:)
        channel_line =
          'inputs.channels = [ "deploy" ];'

        File.write(File.join(conf_dir, 'cluster/nixos-machine/module.nix'), <<~NIX)
          { config, ... }:
          {
            cluster."nixos-machine" = {
              spin = "nixos";
              #{channel_line}
              host.target = "127.0.0.1";
              host.port = #{nixos_port};
              healthChecks.machineCommands = [
                {
                  description = "true";
                  command = [ "true" ];
                }
                {
                  description = "argv with space";
                  command = [
                    "printf"
                    "%s"
                    "space ok"
                  ];
                  standardOutput.match = "space ok";
                }
              ];
            };
          }
        NIX

        File.write(File.join(conf_dir, 'cluster/vpsadminos-machine/module.nix'), <<~NIX)
          { config, ... }:
          {
            cluster."vpsadminos-machine" = {
              spin = "vpsadminos";
              #{channel_line}
              host.target = "127.0.0.1";
              host.port = #{vpsadminos_port};
              healthChecks.machineCommands = [
                {
                  description = "true";
                  command = [ "true" ];
                }
                {
                  description = "argv with space";
                  command = [
                    "printf"
                    "%s"
                    "space ok"
                  ];
                  standardOutput.match = "space ok";
                }
              ];
            };
          }
        NIX
      end

      def prepare_fixture!(conf_dir:, dummy_repo:, nixos_port:, vpsadminos_port:, pubkey:)
        prepare_fixture_dir!(conf_dir, template: 'example')

        write_flake_root!(conf_dir, dummy_repo)

        write_machine_modules!(
          conf_dir,
          nixos_port:,
          vpsadminos_port:
        )

        write_nixos_config!(conf_dir, marker: 'A')
        write_vpsadminos_config!(conf_dir, marker: 'A')

        write_admin_ssh_keys!(conf_dir, pubkey)
        cluster_modules = [ 'nixos-machine/module.nix' ]
        cluster_modules << 'vpsadminos-machine/module.nix'
        write_cluster_modules!(conf_dir, cluster_modules)
        run_local!(['nix', 'flake', 'lock', "path:#{conf_dir}"], chdir: conf_dir)
        init_fixture_repo!(conf_dir)
      end

      def update_dummy_without_commit!
        confctl!('inputs', 'update', 'dummy-input')
      end

      def commit_dummy_update_and_set!(target_rev)
        confctl!('inputs', 'update', '--commit', '--no-changelog', '--no-editor', 'dummy-input')
        confctl!('inputs', 'set', '--commit', '--changelog', '--no-editor', 'dummy-input', target_rev)
      end

      def machine_state(machine_name)
        confctl_machine_state(machine_name)
      end

      def assert_machine_state(machine_name, profile: nil, current: nil)
        expect(confctl_remote_realpath(machine_name, '/nix/var/nix/profiles/system')).to eq(profile) unless profile.nil?
        expect(confctl_remote_realpath(machine_name, '/run/current-system')).to eq(current) unless current.nil?
      end

      def assert_store_path_exists(machine_name, store_path)
        confctl_store_path_exists!(machine_name, store_path)
      end

      def configuration_info(machine_name)
        out, = confctl_ssh!(machine_name, 'cat', '/etc/confctl/configuration-info.json')
        json = out.lines.find { |line| line.lstrip.start_with?('{') }
        raise "unable to parse configuration info from #{machine_name}: #{out.inspect}" if json.nil?

        JSON.parse(json)
      end

      def build_generation!(host)
        confctl!('build', '--yes', host)
        confctl_generation_info(host)
      end

      def assert_generation_b_valid!(phase)
        generation = @nixos_gen_b
        root = File.realpath(conf_dir)
        # Use the qualified Ruby login contract in the same child environment as
        # confctl, and hash its canonical configuration root, not the runner cwd.
        login, = run_local!(
          [RbConfig.ruby, '-retc', '-e', 'print(Etc.getlogin || ENV["USER"] || abort("no login identity"))'],
          chdir: conf_dir, env: confctl_env
        )
        host = NIXOS_MACHINE.tr('/', ':')
        local_link = File.join(root, '.confctl/generations', host, generation.fetch('name'), 'toplevel')
        root_link = File.join(
          '/nix/var/nix/gcroots/per-user', login, "confctl-#{Digest::SHA256.hexdigest(root)[0, 8]}",
          "#{host}-generation-#{generation.fetch('name')}-toplevel"
        )
        toplevel = generation.fetch('toplevel')
        puts "generation B validity: phase=#{phase.inspect} name=#{generation.fetch('name').inspect} toplevel=#{toplevel.inspect}"
        failures = []
        { root_link => local_link, local_link => toplevel }.each do |path, expected|
          begin
            target = File.readlink(path)
            puts "generation B link: phase=#{phase.inspect} path=#{path.inspect} target=#{target.inspect} expected=#{expected.inspect}"
            failures << "link #{path.inspect} has unexpected target" unless target == expected
          rescue SystemCallError => error
            puts "generation B link: phase=#{phase.inspect} path=#{path.inspect} errno=#{error.errno} error=#{error.message.inspect} expected=#{expected.inspect}"
            failures << "link #{path.inspect} errno=#{error.errno}"
          end
        end
        out, err, status = run_local(['nix', 'path-info', toplevel], chdir: conf_dir, env: confctl_env)
        puts "generation B path-info: phase=#{phase.inspect} path=#{toplevel.inspect} exit=#{status.exitstatus.inspect} signal=#{status.termsig.inspect} stdout=#{out.inspect} stderr=#{err.inspect}"
        failures << "path-info failed for #{toplevel.inspect}" unless status.success?
        unless failures.empty?
          raise "generation B validity precondition failed: phase=#{phase.inspect} #{failures.join('; ')}; path-info exit=#{status.exitstatus.inspect} signal=#{status.termsig.inspect}"
        end
      end

      before(:suite) do
        @nixos_port = ConfctlHostfwdPorts.reserve('net1')
        @vpsadminos_port = ConfctlHostfwdPorts.reserve('net2')

        nixos.start
        vpsadminos.start

        @state_dir = @opts[:state_dir]
        @conf_dir = File.join(@state_dir, 'conf')
        @dummy_repo = File.join(@state_dir, 'dummy-input')
        @home_dir = File.join(@state_dir, 'home')
        confctl_setup!(
          bin: "${confctlBin}",
          src: "${confctlSource}",
          conf_dir: @conf_dir,
          home_dir: @home_dir,
          nix_path: "nixpkgs=#{NIXPKGS_PATH}"
        )

        @dummy_rev_a = setup_dummy_repo!(@dummy_repo)
        @pubkey = setup_ssh_home!(@home_dir)

        install_pubkey!(nixos, @pubkey)
        install_pubkey!(vpsadminos, @pubkey)

        prepare_fixture!(
          conf_dir: @conf_dir,
          dummy_repo: @dummy_repo,
          nixos_port: @nixos_port,
          vpsadminos_port: @vpsadminos_port,
          pubkey: @pubkey
        )
      end

      describe 'confctl deploy behavior', order: :defined do
        before(:context) do
          out = wait_for_confctl_connectivity!(expected_successes: 2, timeout: 180)
          expect(out).to include("#{2} successful")

          out, = run_local!(%w[git rev-parse HEAD], chdir: @conf_dir)
          @configuration_revision = out.strip

          @nixos_gen_a = build_generation!(NIXOS_MACHINE)
          @vps_gen_a = build_generation!(VPSADMINOS_MACHINE)

          out, = confctl!('deploy', '--yes')
          expect(out).not_to match(/\e\[/)

          @nixos_state_a = machine_state(NIXOS_MACHINE)
          assert_rust_helper!(nixos, 'confctl-auto-rollback', wrapper: @nixos_gen_a.fetch('auto_rollback'))
          assert_rust_helper!(vpsadminos, 'confctl-auto-rollback', wrapper: @vps_gen_a.fetch('auto_rollback'))
        end

        it 'deploys baseline generation on both machines' do
          [NIXOS_MACHINE, VPSADMINOS_MACHINE].each do |host|
            out, = confctl_ssh!(host, 'cat', '/etc/confctl/inputs-info.json')
            expect(out).to include('nixpkgs')
            confctl_ssh!(host, 'sh', '-c', 'test ! -e /etc/confctl/swpins-info.json')
          end

          assert_machine_state(
            NIXOS_MACHINE,
            profile: @nixos_gen_a['toplevel'],
            current: @nixos_gen_a['toplevel']
          )
          assert_machine_state(
            VPSADMINOS_MACHINE,
            profile: @vps_gen_a['toplevel'],
            current: @vps_gen_a['toplevel']
          )

          expect(configuration_info(NIXOS_MACHINE)).to eq(
            'schemaVersion' => 1,
            'revision' => @configuration_revision,
            'revisionDirty' => false
          )
          expect(configuration_info(VPSADMINOS_MACHINE)).to eq(
            'schemaVersion' => 1,
            'revision' => @configuration_revision,
            'revisionDirty' => false
          )
        end

        it 'builds second generation for activation-target tests' do
          write_nixos_config!(@conf_dir, marker: 'B')
          @nixos_gen_b = build_generation!(NIXOS_MACHINE)
          assert_generation_b_valid!('B publication')
          expect(@nixos_gen_b['toplevel']).not_to eq(@nixos_gen_a['toplevel'])
        end

        it 'copy-only copies selected generation without changing profile or current system' do
          expect(@nixos_gen_b).not_to be_nil

          confctl!('deploy', '--yes', '--copy-only', '--generation', @nixos_gen_b['name'], NIXOS_MACHINE)
          assert_store_path_exists(NIXOS_MACHINE, @nixos_gen_b['toplevel'])
          assert_machine_state(
            NIXOS_MACHINE,
            profile: @nixos_state_a[:profile],
            current: @nixos_state_a[:current]
          )
        end

        it 'test action keeps system profile unchanged' do
          expect(@nixos_gen_b).not_to be_nil

          confctl!('deploy', '--yes', '--generation', @nixos_gen_b['name'], NIXOS_MACHINE, 'test')
          assert_store_path_exists(NIXOS_MACHINE, @nixos_gen_b['toplevel'])
          assert_machine_state(NIXOS_MACHINE, profile: @nixos_state_a[:profile])
        end

        it 'dry-activate does not change current or profile symlinks' do
          expect(@nixos_gen_b).not_to be_nil

          confctl!('deploy', '--yes', '--generation', @nixos_gen_a['name'], NIXOS_MACHINE, 'switch')
          assert_machine_state(
            NIXOS_MACHINE,
            profile: @nixos_gen_a['toplevel'],
            current: @nixos_gen_a['toplevel']
          )

          confctl!('deploy', '--yes', '--generation', @nixos_gen_b['name'], NIXOS_MACHINE, 'dry-activate')
          assert_store_path_exists(NIXOS_MACHINE, @nixos_gen_b['toplevel'])
          assert_machine_state(
            NIXOS_MACHINE,
            profile: @nixos_gen_a['toplevel'],
            current: @nixos_gen_a['toplevel']
          )
        end

        it 'boot action updates profile without changing current system' do
          expect(@nixos_gen_b).not_to be_nil

          confctl!('deploy', '--yes', '--generation', @nixos_gen_a['name'], NIXOS_MACHINE, 'switch')

          confctl!('deploy', '--yes', '--generation', @nixos_gen_b['name'], NIXOS_MACHINE, 'boot')
          assert_machine_state(
            NIXOS_MACHINE,
            profile: @nixos_gen_b['toplevel'],
            current: @nixos_gen_a['toplevel']
          )
        end

        it 'switch action updates both profile and current system' do
          expect(@nixos_gen_b).not_to be_nil

          confctl!('deploy', '--yes', '--generation', @nixos_gen_b['name'], NIXOS_MACHINE, 'switch')
          assert_machine_state(
            NIXOS_MACHINE,
            profile: @nixos_gen_b['toplevel'],
            current: @nixos_gen_b['toplevel']
          )
          expect(configuration_info(NIXOS_MACHINE)).to eq(
            'schemaVersion' => 1,
            'revision' => @configuration_revision,
            'revisionDirty' => true
          )
        end

        it 'skips repeated boot deploy with reboot when generation is already current' do
          expect(@nixos_gen_b).not_to be_nil

          out, = confctl!('deploy', '--yes', '--reboot', '--generation', @nixos_gen_b['name'], NIXOS_MACHINE, 'boot')
          expect(out).to include("Skipping #{NIXOS_MACHINE}: already using target generation")
          expect(out).not_to include("Rebooting #{NIXOS_MACHINE}")
          assert_machine_state(
            NIXOS_MACHINE,
            profile: @nixos_gen_b['toplevel'],
            current: @nixos_gen_b['toplevel']
          )
        end

        it 'builds third generation for generation-selection tests' do
          expect(@nixos_gen_b).not_to be_nil

          write_nixos_config!(@conf_dir, marker: 'C')
          @nixos_gen_c = build_generation!(NIXOS_MACHINE)
          assert_generation_b_valid!('after C publication')
          expect(@nixos_gen_c['toplevel']).not_to eq(@nixos_gen_b['toplevel'])
        end

        it 'deploys selected generation by explicit name' do
          expect(@nixos_gen_b).not_to be_nil

          confctl!('deploy', '--yes', '--generation', @nixos_gen_b['name'], NIXOS_MACHINE, 'switch')
          assert_machine_state(
            NIXOS_MACHINE,
            profile: @nixos_gen_b['toplevel'],
            current: @nixos_gen_b['toplevel']
          )
        end

        it 'deploys selected generation by current selector' do
          expect(@nixos_gen_c).not_to be_nil

          confctl!('deploy', '--yes', '--generation', 'current', NIXOS_MACHINE, 'switch')
          assert_machine_state(
            NIXOS_MACHINE,
            profile: @nixos_gen_c['toplevel'],
            current: @nixos_gen_c['toplevel']
          )
        end

        it 'deploys selected generation by numeric offset' do
          expect(@nixos_gen_b).not_to be_nil

          assert_generation_b_valid!('before numeric offset deploy')
          confctl!('deploy', '--yes', '--generation', '-1', NIXOS_MACHINE, 'switch')
          assert_machine_state(
            NIXOS_MACHINE,
            profile: @nixos_gen_b['toplevel'],
            current: @nixos_gen_b['toplevel']
          )
        end

        it 'deploys selected current generation with one-by-one and dry-activate-first' do
          selected_nixos = confctl_generation_info(NIXOS_MACHINE)
          selected_vpsadminos = confctl_generation_info(VPSADMINOS_MACHINE)

          confctl!('deploy', '--yes', '--one-by-one', '--dry-activate-first', '--generation', 'current')

          assert_machine_state(
            NIXOS_MACHINE,
            profile: selected_nixos['toplevel'],
            current: selected_nixos['toplevel']
          )
          assert_machine_state(
            VPSADMINOS_MACHINE,
            profile: selected_vpsadminos['toplevel'],
            current: selected_vpsadminos['toplevel']
          )
          assert_store_path_exists(VPSADMINOS_MACHINE, selected_vpsadminos['toplevel'])
        end

        it 'runs status command for current generation' do
          out, = confctl!('status', '--yes', '--generation', 'current')
          expect(out).to include('nixos-machine')
          expect(out).to include('vpsadminos-machine')
        end

        it 'runs diff command for current generation' do
          out, = confctl!('diff', '--yes', '--generation', 'current')
          expect(out).to be_a(String)
        end

        it 'runs changelog command for dummy source' do
          out, = confctl!('changelog', '--yes', 'dummy')
          expect(out).to be_a(String)
        end

        it 'updates dummy source without creating commit' do
          @dummy_rev_b = bump_dummy_repo!(@dummy_repo, 'B')
          update_dummy_without_commit!
        end

        it 'commits dummy source update and sets revision back' do
          expect(@dummy_rev_b).not_to be_nil

          bump_dummy_repo!(@dummy_repo, 'C')
          commit_dummy_update_and_set!(@dummy_rev_b)
        end

        it 'reports dummy source changes in status, diff and changelog output' do
          status_out, = confctl!('status', '--yes', '--generation', 'current')
          diff_out, = confctl!('diff', '--yes', '--generation', 'current')
          changelog_out, = confctl!('changelog', '--yes', 'dummy')
          expect(status_out).to include('DUMMY')
          expect([status_out, diff_out, changelog_out].join("\n")).to match(/dummy/i)
        end

        it 'passes health-checks' do
          out, = confctl!('health-check', '--yes')
          expect(out).to include('checks passed')
        end

        it 'lists local generations' do
          out, = confctl!('generation', 'ls', '--local')
          expect(out).to include('nixos-machine')
          expect(out).to include('vpsadminos-machine')
        end

        it 'prints expected output from confctl ssh' do
          ssh_out, = confctl!('ssh', '--yes', NIXOS_MACHINE, 'printf', '%s', 'ssh ok')
          expect(ssh_out).to include('nixos-machine')
          expect(ssh_out).to include('ssh ok')
        end

        it 'keeps both machines reachable at the end' do
          out = wait_for_confctl_connectivity!(expected_successes: 2, timeout: 180)
          expect(out).to include("#{2} successful")
        end
      end
    '';
  }
) args
