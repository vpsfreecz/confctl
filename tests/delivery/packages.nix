# Fixture-bound installed packages. These outputs are not the ordinary fleet
# package observations: bindings point only into the disposable guest.
{
  pkgs,
  src ? ../..,
  appSrc ? null,
  appDocs ? null,
  oracleSrc ? null,
  sitePairs ? [ ],
}:
let
  root = "/var/lib/confctl-delivery";
  tools = import (src + "/tests/compat/tools.nix") { inherit pkgs; };
  callback = pkgs.writeScript "delivery-carrier-callback" ''
    #!${pkgs.ruby}/bin/ruby
    require 'json'
    root = ${builtins.toJSON root}
    profile = File.join(root, 'carrier/profile')
    abort 'profile effect missing before callback' unless File.file?(profile)
    File.open(File.join(root, 'carrier/callback.jsonl'), 'a') do |f|
      f.puts(JSON.generate('profile' => File.read(profile)))
    end
    exit(File.exist?(File.join(root, 'carrier/callback-fail')) ? 9 : 0)
  '';
  fakeKexec = pkgs.writeScriptBin "kexec" ''
    #!${pkgs.ruby}/bin/ruby
    require 'json'
    root = ${builtins.toJSON root}
    record = { 'argv' => ARGV }
    if ARGV.length == 5 && ARGV[0] == '--load' && ARGV[2].start_with?('--initrd=') &&
       ARGV[3].start_with?('--append=') && ARGV[4] == '--no-sync'
      kernel, initrd = ARGV[1], ARGV[2].delete_prefix('--initrd=')
      [kernel, initrd].each do |path|
        abort 'load outside guest temp root' unless path.start_with?(root + '/tmp/') && File.file?(path)
      end
      record['kernel'] = File.binread(kernel)
      record['initrd'] = File.binread(initrd)
    elsif ![['--exec', '--no-sync'], ['--unload', '--no-sync']].include?(ARGV)
      abort "unselected kexec fixture request: #{ARGV.inspect}"
    end
    File.open(root + '/kexec.jsonl', 'a') { |f| f.puts(JSON.generate(record)) }
  '';
  # Known copy-test bytes, not executable PXE payloads or bootability evidence.
  fixtureBootData = pkgs.runCommand "delivery-fixture-boot-data" { } ''
    mkdir -p "$out/share/syslinux"
    for name in pxelinux.0 ldlinux.c32 libcom32.c32 libutil.c32 memdisk menu.c32 reboot.c32; do
      printf 'asset:%s' "$name" > "$out/share/syslinux/$name"
    done
  '';
  netbootConfig = pkgs.writeText "delivery-netboot.json" (
    builtins.toJSON {
      coreutils = toString pkgs.coreutils;
      syslinux = toString fixtureBootData;
      tftpRoot = root + "/tftp";
      httpRoot = root + "/http";
      hostName = "delivery-fixture";
      httpUrl = "http://127.0.0.1:18080";
      memtest = null;
      isoImages = [ ];
    }
  );
  manPage = import (src + "/nix/kexec-netboot-man.nix") { inherit pkgs; };
  fixtureNix = pkgs.writeScriptBin "nix" ''
    #!${pkgs.ruby}/bin/ruby
    # Only the existing input-set fixture needs a dynamically named output.
    # All requests still pass through the existing finite plan/trace dispatcher.
    if ARGV[0, 2] == ['flake', 'update'] || ARGV[0, 2] == ['flake', 'lock']
      root = File.realpath(ENV.fetch('COMPAT_ROOT'))
      abort 'not the fixture guest' unless File.read('/etc/hostname').strip == 'delivery-fixture' && root.start_with?('${root}/cli/')
      abort 'input mutation outside fixture config' unless Dir.pwd == root
      if ARGV == ['flake', 'update', 'a']
        target = File.join(root, 'flake.lock')
      elsif ARGV.length == 9 && ARGV[0, 4] == ['flake', 'lock', '--update-input', 'a'] &&
            ARGV[4, 2] == ['--override-input', 'a'] && ARGV[7] == '--output-lock-file' &&
            File.dirname(ARGV[8]) == root && File.basename(ARGV[8]).start_with?('flake.lock.tmp-')
        target = ARGV[8]
      else
        abort 'unselected input fixture request'
      end
      system('${tools}/bin/nix', *ARGV)
      exit($?.exitstatus || 1) unless $?.success?
      File.write(target, File.read(File.join(root, 'next.lock')))
    else
      exec('${tools}/bin/nix', *ARGV)
    end
  '';
  operationPkgs = pkgs // {
    # Expose only SSH here: putting tools/bin earlier would shadow fixtureNix.
    openssh = pkgs.runCommand "delivery-ssh" { } ''
      mkdir -p "$out/bin"
      ln -s ${tools}/bin/ssh "$out/bin/ssh"
    '';
    nix = pkgs.runCommand "delivery-nix-tools" { } ''
      mkdir -p "$out/bin"
      ln -s ${fixtureNix}/bin/nix "$out/bin/nix"
      for name in nix-copy-closure nix-env nix-collect-garbage nix-shell; do
        ln -s "${tools}/bin/$name" "$out/bin/$name"
      done
    '';
  };
  # Keep the normal producer, cgo, checks, flags and wrapper construction. Only
  # the operation package's external effect dependencies are fixture-bound.
  cli =
    if appSrc == null then
      null
    else
      assert appDocs != null;
      import (appSrc + "/nix/package.nix") {
        inherit pkgs;
        src = appSrc;
        docs = appDocs;
      };
  operationCLI =
    if appSrc == null then
      null
    else
      assert appDocs != null;
      (import (appSrc + "/nix/package.nix") {
        pkgs = operationPkgs;
        src = appSrc;
        docs = appDocs;
      }).overrideAttrs
        (_old: {
          # Package checks still receive the declared native tools; the installed
          # wrapper alone binds effectful Nix/SSH to the existing fixture tool.
          nativeCheckInputs = [
            pkgs.git
            pkgs.bash
            pkgs.coreutils
            pkgs.nix
          ];
        });
  originalCLI =
    if oracleSrc == null then
      null
    else
      import (src + "/nix/legacy-package.nix") {
        pkgs = operationPkgs;
        src = oracleSrc;
      };
  # A finite test peer, not a replacement extension SDK or service framework.
  # It uses the production FD3/4 supervisor to record deploy context and request
  # one/two confirmations. Unknown messages fail rather than inventing replies.
  peer = pkgs.writeScriptBin "delivery-peer" ''
    #!${pkgs.ruby}/bin/ruby
    require 'json'
    input, output = IO.new(3, 'r'), IO.new(4, 'w')
    output.sync = true
    init = JSON.parse(input.readline)
    abort 'expected initialize' unless init.fetch('method') == 'initialize'
    output.puts(JSON.generate(jsonrpc: '2.0', id: init.fetch('id'), result: {protocol: {major: 1, minor: 0}}))
    run = JSON.parse(input.readline)
    abort 'expected run' unless run.fetch('method') == 'run'
    context = run.fetch('params').fetch('context')
    root = context.fetch('config_root')
    abort 'peer outside disposable guest config' unless File.read('/etc/hostname').strip == 'delivery-fixture' && root.start_with?('${root}/cli/')
    File.write(File.join(root, 'peer-context.json'), JSON.generate(context))
    File.open(File.join(root, 'peer-events'), 'a') { |f| f.puts('prepare') }
    after = File.join(root, 'machines.after.json')
    File.write(File.join(root, 'machines.json'), File.read(after)) if File.file?(after)
    if run.fetch('params').fetch('handler') == 'confirm'
      count = ENV.fetch('DELIVERY_CONFIRM_COUNT', '1').to_i
      abort 'finite confirmation count' unless [1, 2].include?(count)
      count.times do |i|
        output.puts(JSON.generate(jsonrpc: '2.0', id: "fixture-#{i}", method: 'ui.confirm', params: {message: "Fixture permission #{i}?"}))
      end
      count.times do
        reply = JSON.parse(input.readline)
        # Cancellation may instead close the channel/terminate this owned peer.
        abort 'confirmation refused' if reply.key?('error')
        result = reply['result']
        abort 'unknown confirmation response' unless result.is_a?(Hash) && result.keys == ['accepted'] && [true, false].include?(result['accepted'])
        abort 'confirmation refused' unless result['accepted']
        File.open(File.join(root, 'peer-events'), 'a') { |f| f.puts('accepted') }
      end
    end
    output.puts(JSON.generate(jsonrpc: '2.0', id: run.fetch('id'), result: {exit_code: 0}))
  '';
in
{
  inherit
    root
    tools
    fakeKexec
    cli
    operationCLI
    originalCLI
    peer
    sitePairs
    oracleSrc
    ;
  carrier = import (src + "/nix/modules/confctl/carrier/package.nix") {
    inherit pkgs;
    onChangeCommands = toString callback;
  };
  rollback = import (src + "/nix/helpers/auto-rollback.nix") { inherit pkgs; };
  builder = import (src + "/nix/helpers/build-netboot-server.nix") {
    inherit pkgs;
    configFile = toString netbootConfig;
  };
  kexec = import (src + "/nix/helpers/kexec-netboot.nix") {
    pkgs = pkgs // {
      kexec-tools = fakeKexec;
    };
    machineFqdn = "node.test";
    inherit manPage;
  };
}
