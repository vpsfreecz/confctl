# frozen_string_literal: true

# Original helpers run with fake OS/process/HTTP boundaries. No shell command,
# SSH, Nix profile mutation, live HTTP request or privileged path is executed.
require 'fileutils'
require 'json'
require 'net/http'
require 'securerandom'
require 'shellwords'
require 'tempfile'

module HelperOracle
  ROOT = File.realpath(ENV.fetch('CONFCTL_COMPAT_ROOT'))
  SCENARIO = JSON.parse(File.read(File.join(ROOT, 'scenario.json')))
  EVENTS = [] # rubocop:disable Style/MutableConstant -- per-child append-only event trace
  WAIT = Process.method(:wait)
  FORK = Process.method(:fork)
  PATHS = {
    '/nix/var/nix/profiles' => File.join(ROOT, 'profiles'),
    '/run/confctl' => File.join(ROOT, 'run/confctl')
  }.freeze

  def self.record(event, data = {})
    EVENTS << { 'event' => event }.merge(data)
  end

  def self.path(path)
    value = path.respond_to?(:to_path) ? path.to_path : path
    return value unless value.is_a?(String)

    PATHS.each do |prefix, destination|
      return destination + value[prefix.length..] if value == prefix || value.start_with?("#{prefix}/")
    end
    value
  end

  def self.check_write(path)
    destination = File.expand_path(self.path(path))
    raise "Write outside fixture: #{destination}" unless destination.start_with?("#{ROOT}/")

    destination
  end

  def self.system(*argv)
    record('system', 'argv' => argv)
    if argv[0] == File.join(ROOT, 'tools/bin/ln')
      return false if SCENARIO['publish_failure']

      _program, flag, target, destination = argv
      raise 'Unexpected ln contract' unless flag == '-sfn'

      check_write(destination)
      temporary = "#{destination}.new"
      File.symlink(target, temporary)
      File.rename(temporary, destination)
      return true
    end

    valid = argv[0] == 'nix-env' || argv[0] == File.join(ROOT, 'on-change') ||
            (argv.length == 1 && argv[0].start_with?(File.join(ROOT, 'tools/bin/kexec')))
    raise "Unexpected process: #{argv.inspect}" unless valid

    status =
      if argv[0] == 'nix-env'
        SCENARIO.fetch('nix_status', 0)
      elsif argv[0] == File.join(ROOT, 'on-change')
        SCENARIO.fetch('callback_status', 0)
      else
        SCENARIO.fetch('kexec_status', 0)
      end
    # A tiny owned child sets Ruby's real $? without executing any command.
    pid = FORK.call { exit!(status) }
    WAIT.call(pid)
    status == 0
  end

  def self.http_response(uri)
    record('http', 'url' => uri.to_s)
    code = SCENARIO.fetch('http_status', 200)
    klass = code == 200 ? Net::HTTPOK : Net::HTTPNotFound
    response = klass.new('1.1', code.to_s, 'fixture')
    body = if SCENARIO['bad_json']
             '{bad'
           else
             JSON.generate('machines' => SCENARIO.fetch('machines', []))
           end
    response.define_singleton_method(:body) { body }
    response.define_singleton_method(:read_body) { |&block| block.call('fixture-download') }
    response
  end

  def self.prepare
    FileUtils.mkdir_p(File.join(ROOT, 'profiles'))
    FileUtils.mkdir_p(File.join(ROOT, 'run/confctl'))
    FileUtils.mkdir_p(File.join(ROOT, 'tools/share/syslinux'))
    %w[pxelinux.0 ldlinux.c32 libcom32.c32 libutil.c32 memdisk menu.c32 reboot.c32].each do |program|
      File.write(File.join(ROOT, 'tools/share/syslinux', program), program)
    end
    File.write(File.join(ROOT, 'memtest.bin'), 'memtest')
    File.write(File.join(ROOT, 'installer.iso'), 'iso')

    SCENARIO.fetch('profiles', []).each do |profile|
      name = profile.fetch('name')
      generation = profile.fetch('generation')
      dist = File.join(ROOT, 'store', "#{name}-#{generation}")
      toplevel = File.join(ROOT, 'systems', "#{name}-#{generation}")
      FileUtils.mkdir_p(dist)
      FileUtils.mkdir_p(toplevel)
      File.symlink('/nix/store/fixture-linux-6.12.3/bzImage', File.join(toplevel, 'kernel'))
      metadata = {
        'toplevel' => toplevel, 'spin' => profile.fetch('spin', 'vpsadminos'),
        'fqdn' => "#{name}.invalid", 'label' => "#{name}.fixture.invalid", 'version' => '25.11.test',
        'revision' => '0123456789abcdef', 'kernelParams' => ['console=ttyS0', 'httproot=old'],
        'macs' => ['02:00:00:00:00:01'], 'inputs-info' => { 'vpsadminos' => { 'rev' => 'abcdef' } }
      }.merge(profile.fetch('metadata', {}))
      File.write(File.join(dist, 'machine.json'), profile['corrupt'] ? '{bad' : JSON.generate(metadata))
      File.write(File.join(dist, 'kernel-params'), profile['params_file']) if profile['params_file']
      profile.fetch('boot_files', %w[bzImage initrd root.squashfs]).each do |file|
        File.write(File.join(dist, file), file)
      end
      File.symlink(dist, File.join(ROOT, 'profiles', "confctl-#{name}-#{generation}-link"))
      File.symlink(dist, File.join(ROOT, 'profiles', "confctl-#{name}")) if profile['current']
    end

    %w[tftp http].each do |name|
      next unless SCENARIO['old_roots']

      FileUtils.mkdir_p(File.join(ROOT, "#{name}.old"))
      File.write(File.join(ROOT, "#{name}.old/sentinel"), 'old')
      File.symlink("#{name}.old", File.join(ROOT, name))
    end

    config = {
      'ruby' => '/unused/ruby', 'coreutils' => File.join(ROOT, 'tools'), 'syslinux' => File.join(ROOT, 'tools'),
      'tftpRoot' => File.join(ROOT, 'tftp'), 'httpRoot' => File.join(ROOT, 'http'),
      'hostName' => 'boot.fixture.invalid', 'httpUrl' => 'http://boot.fixture.invalid',
      'memtest' => SCENARIO['memtest'] ? { 'package' => ROOT, 'params' => ['console=ttyS0'] } : nil,
      'isoImages' => SCENARIO['iso'] ? [{ 'file' => File.join(ROOT, 'installer.iso'), 'label' => '' }] : []
    }
    File.write(File.join(ROOT, 'config.json'), JSON.generate(config))
  end

  def self.observation
    files = {}
    Dir.glob(File.join(ROOT, '{http,tftp}*/**/*'), File::FNM_EXTGLOB | File::FNM_DOTMATCH).sort.each do |path|
      relative = path.delete_prefix("#{ROOT}/")
      next if %w[. ..].include?(File.basename(path))

      if File.symlink?(path)
        files[relative] = { 'symlink' => File.readlink(path) }
      elsif File.file?(path)
        files[relative] = { 'text' => File.read(path) }
      end
    end
    %w[http tftp].each do |name|
      path = File.join(ROOT, name)
      files[name] = { 'symlink' => File.readlink(path) } if File.symlink?(path)
    end
    check = File.join(ROOT, 'check')
    { 'events' => EVENTS, 'files' => files, 'check' => File.file?(check) ? File.read(check) : nil,
      'temporary_files' => Dir.glob(File.join(ROOT, 'downloads/*')).map { |path| File.basename(path) } }
  end
end

HelperOracle.prepare
Dir.chdir(HelperOracle::ROOT)
FileUtils.mkdir_p(File.join(HelperOracle::ROOT, 'downloads'))
ENV['TMPDIR'] = File.join(HelperOracle::ROOT, 'downloads')

module OracleFiles
  def read(path, *, **)
    return HelperOracle::SCENARIO.fetch('cmdline', 'httproot=http://boot.fixture.invalid/node/9/root.squashfs') if path == '/proc/cmdline'

    super(HelperOracle.path(path), *, **)
  end

  def readlink(path, *)
    return File.join(HelperOracle::ROOT, 'old-system') if path == '/run/current-system'

    super(HelperOracle.path(path), *)
  end

  def open(path, *args, **, &)
    mapped = HelperOracle.path(path)
    HelperOracle.check_write(mapped) if args.first.to_s.match?(/[wa+]/)
    HelperOracle.record('lock.open') if path == '/run/confctl/build-netboot-server.lock'
    super(mapped, *args, **, &)
  end

  def write(path, text, *)
    destination = HelperOracle.check_write(path)
    HelperOracle.record('check.write', 'value' => text) if destination == File.join(HelperOracle::ROOT, 'check')
    super(destination, text, *)
  end

  def realpath(path, *)
    super(HelperOracle.path(path), *)
  end

  def lstat(path)
    result = super(HelperOracle.path(path))
    return result unless path.to_s.start_with?('/nix/var/nix/profiles/')

    Struct.new(:mtime).new(Time.at(1_700_000_000))
  end

  def exist?(path)
    super(HelperOracle.path(path))
  end
end
File.singleton_class.prepend(OracleFiles)

module OracleDirectories
  def entries(path, *)
    super(HelperOracle.path(path), *)
  end
end
Dir.singleton_class.prepend(OracleDirectories)

module OracleFileUtils
  def mkdir_p(path, **)
    super(HelperOracle.check_write(path), **)
  end
end
FileUtils.singleton_class.prepend(OracleFileUtils)

module OracleKernel
  def system(*)
    HelperOracle.system(*)
  end

  def sleep(_duration)
    HelperOracle.record('sleep')
    if HelperOracle::SCENARIO['confirm']
      File.write(File.join(HelperOracle::ROOT, 'check'), 'confirmed')
    end
    0
  end
end
Kernel.prepend(OracleKernel)
Kernel.singleton_class.prepend(OracleKernel)

module OracleProcess
  def spawn(*argv)
    HelperOracle.record('spawn', 'argv' => argv)
    100
  end

  def wait(pid)
    HelperOracle.record('wait', 'pid' => pid)
    pid
  end
end
Process.singleton_class.prepend(OracleProcess)

module OracleClock
  def now
    return super unless HelperOracle::SCENARIO['helper'] == 'auto-rollback'

    @oracle_tick = (@oracle_tick || 0) + 1
    Time.at(@oracle_tick)
  end
end
Time.singleton_class.prepend(OracleClock)

module OracleRandom
  def hex(_size)
    'abcdef'
  end
end
SecureRandom.singleton_class.prepend(OracleRandom)

module OracleHTTP
  def get_response(uri)
    HelperOracle.http_response(uri)
  end

  def start(host, port)
    client = Object.new
    client.define_singleton_method(:request_get) do |path, &block|
      block.call(HelperOracle.http_response(URI("http://#{host}:#{port}#{path}")))
    end
    yield(client)
  end
end
Net::HTTP.singleton_class.prepend(OracleHTTP)

at_exit do
  observation = HelperOracle.observation
  error = $!
  observation['exception'] = error.class.name if error && !error.is_a?(SystemExit)
  File.write(File.join(HelperOracle::ROOT, 'observation.json'), JSON.generate(observation))
end

source = File.read(File.join(__dir__, 'baseline', "#{HelperOracle::SCENARIO.fetch('helper')}.rb.txt"))
substitutions = {
  '@ruby@' => '/unused/ruby', '@onChangeCommands@' => File.join(HelperOracle::ROOT, 'on-change'),
  '@jsonConfig@' => File.join(HelperOracle::ROOT, 'config.json'), '@machineFqdn@' => 'node.invalid',
  '@kexecTools@' => File.join(HelperOracle::ROOT, 'tools')
}
substitutions.each { |key, value| source = source.gsub(key, value) }
ARGV.replace(HelperOracle::SCENARIO.fetch('argv', []).map { |arg| arg.gsub('@ROOT@', HelperOracle::ROOT) })
$0 = HelperOracle::SCENARIO.fetch('helper')
script_path = File.join(HelperOracle::ROOT, "#{$0}.rb")
File.write(script_path, source)
load(script_path)
