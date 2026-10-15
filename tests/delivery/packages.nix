# Fixture-bound installed packages. These outputs are not the ordinary fleet
# package observations: bindings point only into the disposable guest.
{
  pkgs,
  src ? ../..,
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
  netbootConfig = pkgs.writeText "delivery-netboot.json" (
    builtins.toJSON {
      coreutils = toString pkgs.coreutils;
      syslinux = toString pkgs.syslinux;
      tftpRoot = root + "/tftp";
      httpRoot = root + "/http";
      hostName = "delivery-fixture";
      httpUrl = "http://127.0.0.1:18080";
      memtest = null;
      isoImages = [ ];
    }
  );
  manPage = import (src + "/nix/kexec-netboot-man.nix") { inherit pkgs; };
in
{
  inherit root tools fakeKexec;
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
