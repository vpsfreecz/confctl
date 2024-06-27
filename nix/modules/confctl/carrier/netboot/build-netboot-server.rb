#!@ruby@/bin/ruby
require 'erb'
require 'fileutils'
require 'json'
require 'securerandom'

class Config
  class Memtest
    # @return [Boolean]
    attr_reader :enable

    # @return [String]
    attr_reader :package

    # @return [String]
    attr_reader :params

    # @return [String]
    attr_reader :params_line

    def initialize(cfg)
      @enable = !cfg.nil?
      return unless @enable

      @package = cfg.fetch('package')
      @params = cfg.fetch('params')
      @params_line = @params.join(' ')
    end
  end

  # @param file [String]
  def self.parse(file)
    new(JSON.parse(File.read(file)))
  end

  # @return [String]
  attr_reader :ruby

  # @return [String]
  attr_reader :coreutils

  # @return [String]
  attr_reader :syslinux

  # @return [String]
  attr_reader :tftp_root

  # @return [String]
  attr_reader :http_root

  # @return [String]
  attr_reader :host_name

  # @return [String]
  attr_reader :http_url

  # @return [Memtest]
  attr_reader :memtest

  # @param cfg [Hash]
  def initialize(cfg)
    @ruby = cfg.fetch('ruby')
    @coreutils = cfg.fetch('coreutils')
    @syslinux = cfg.fetch('syslinux')
    @tftp_root = cfg.fetch('tftpRoot')
    @http_root = cfg.fetch('httpRoot')
    @host_name = cfg.fetch('hostName')
    @http_url = cfg.fetch('httpUrl')
    @memtest = Memtest.new(cfg.fetch('memtest'))
  end
end

class NetbootBuilder
  CONFIG_FILE = '@jsonConfig@'.freeze

  LINK_DIR = '/nix/var/nix/profiles'.freeze

  CARRIED_PREFIX = 'confctl'.freeze

  LOCK_FILE = '/run/confctl/build-netboot-server.lock'.freeze

  def self.run
    builder = new(Config.parse(CONFIG_FILE))
    builder.run
  end

  def initialize(config)
    @config = config
  end

  def run
    lock { safe_run }
  end

  protected

  def safe_run
    machines = load_machines

    machines.each do |m|
      puts m.fqdn

      m.generations.each do |g|
        puts "  - #{g.time_s} - #{g.version}#{g.current ? ' (current)' : ''}"
      end

      puts
    end

    random = SecureRandom.hex(3)

    builders = [
      TftpBuilder.new(@config, random, machines, @config.tftp_root),
      HttpBuilder.new(@config, random, machines, @config.http_root)
    ]

    builders.each(&:run)
    builders.each(&:install)
    builders.each(&:cleanup)
  end

  def lock
    FileUtils.mkdir_p(File.dirname(LOCK_FILE))

    File.open(LOCK_FILE, 'w') do |f|
      f.flock(File::LOCK_EX)
      yield
    end
  end

  def load_machines
    machines = {}

    Dir.entries(LINK_DIR).each do |v|
      next if /\A#{Regexp.escape(CARRIED_PREFIX)}-(.+)-(\d+)-link\z/ !~ v

      name = ::Regexp.last_match(1)
      generation = ::Regexp.last_match(2).to_i

      link_path = File.join(LINK_DIR, v)

      machines[name] ||= Machine.new(name)
      machines[name].add_generation(link_path, generation)
    end

    machines.each_value do |m|
      current_path = File.realpath(File.join(LINK_DIR, "#{CARRIED_PREFIX}-#{m.name}"))

      m.generations.each do |g|
        next if g.store_path != current_path

        g.current = true
        break
      end
    end

    machines.each_value(&:resolve)
    machines.values
  end
end

class RootBuilder
  # @return [Array<Machine>]
  attr_reader :machines

  # @return [String]
  attr_reader :root

  # @param config [Config]
  # @param random [String]
  # @param machines [Array<Machine>]
  # @param root [String]
  def initialize(config, random, machines, root)
    @config = config
    @machines = machines
    @target_root = root
    @root = @new_root = "#{root}.#{random}"

    begin
      @current_root = File.readlink(root)

      unless @current_root.start_with?('/')
        @current_root = File.join(File.dirname(root), @current_root)
      end
    rescue Errno::ENOENT
      @current_root = nil
    end
  end

  def run
    mkdir_p('')
    build
  end

  def build
    raise NotImplementedError
  end

  def install
    # ln -sfn is called instead of using ruby methods, because ln
    # can replace the symlink atomically.
    return if Kernel.system(File.join(@config.coreutils, 'bin/ln'), '-sfn', File.basename(@new_root), @target_root)

    raise "Failed to install new root to #{@target_root}"
  end

  def cleanup
    FileUtils.rm_r(@current_root) if @current_root
  end

  protected

  # @param src [String] link target
  # @param dst [String] link path relative to root
  def ln_s(src, dst)
    FileUtils.ln_s(src, File.join(root, dst))
  end

  # @param src [String] link target
  # @param dst [String] link path relative to root
  def cp(src, dst)
    FileUtils.cp(src, File.join(root, dst))
  end

  # Create directories within root
  def mkdir_p(*paths)
    paths.each do |v|
      FileUtils.mkdir_p(File.join(root, v))
    end
  end
end

class TftpBuilder < RootBuilder
  PROGRAMS = %w[pxelinux.0 ldlinux.c32 libcom32.c32 libutil.c32 menu.c32 reboot.c32].freeze

  SPIN_LABELS = {
    'nixos' => 'NixOS',
    'vpsadminos' => 'vpsAdminOS'
  }.freeze

  def build
    install_syslinux

    mkdir_p('pxelinux.cfg', 'pxeserver')

    install_boot_files

    @spins =
      machines.each_with_object([]) do |m, acc|
        acc << m.spin unless acc.include?(m.spin)
      end.to_h do |spin|
        [spin, SPIN_LABELS.fetch(spin, spin)]
      end

    render_default_config
    render_spin_configs
    render_machine_configs
  end

  protected

  def install_syslinux
    PROGRAMS.each do |prog|
      cp(File.join(@config.syslinux, 'share/syslinux', prog), prog)
    end
  end

  def install_boot_files
    machines.each do |m|
      m.generations.each do |g|
        path = File.join('boot', m.fqdn, g.generation.to_s)
        mkdir_p(path)

        Dir.entries(g.store_path).each do |v|
          next unless %w[bzImage initrd].include?(v)

          ln_s(File.join(g.store_path, v), File.join(path, v))
        end

        ln_s(g.generation.to_s, File.join('boot', m.fqdn, 'current')) if g.current
      end

      m.current.macs.each do |mac|
        # See https://wiki.syslinux.org/wiki/index.php?title=PXELINUX#Configuration
        ln_s("../pxeserver/machines/#{m.fqdn}/auto.cfg", "pxelinux.cfg/01-#{mac.gsub(':', '-')}")
      end
    end

    return unless @config.memtest.enable

    mkdir_p('boot/memtest86')
    ln_s(File.join(@config.memtest.package, 'memtest.bin'), 'boot/memtest86/memtest.bin')
  end

  def render_default_config
    tpl = <<~ERB
      DEFAULT menu.c32
      PROMPT 0
      TIMEOUT 0
      MENU TITLE <%= hostname %>

      <% spins.each do |spin, label| -%>
      LABEL <%= spin %>
        MENU LABEL <%= label %> >
        KERNEL menu.c32
        APPEND pxeserver/<%= spin %>.cfg

      <% end -%>
      <% if enable_memtest -%>
      LABEL memtest
        MENU LABEL Memtest86
        LINUX boot/memtest86/memtest.bin
        APPEND <%= memtest_params %>

      <% end -%>
      LABEL local_boot
        MENU LABEL Local Boot
        LOCALBOOT 0

      LABEL warm_reboot
        MENU LABEL Warm Reboot
        KERNEL reboot.c32
        APPEND --warm

      LABEL cold_reboot
        MENU LABEL Cold Reboot
        KERNEL reboot.c32
    ERB

    render_to(
      tpl,
      {
        hostname: @config.host_name,
        spins: @spins,
        enable_memtest: @config.memtest.enable,
        memtest_params: @config.memtest.params_line
      },
      'pxelinux.cfg/default'
    )
  end

  def render_spin_configs
    tpl = <<~ERB
      MENU TITLE <%= label %>

      <% spin_machines.each do |m| -%>
      LABEL <%= m.fqdn %>
        MENU LABEL <%= m.label %> >
        KERNEL menu.c32
        APPEND pxeserver/machines/<%= m.fqdn %>/menu.cfg
      <% end -%>

      LABEL mainmenu
        MENU LABEL < Back to Main Menu
        KERNEL menu.c32
        APPEND pxelinux.cfg/default
    ERB

    @spins.each do |spin, label|
      spin_machines = machines.select { |m| m.spin == spin }.sort { |a, b| a.name <=> b.name }

      render_to(tpl, { spin:, label:, spin_machines: }, "pxeserver/#{spin}.cfg")
    end
  end

  def render_machine_configs
    mkdir_p('pxeserver/machines')

    machines.each do |m|
      mkdir_p("pxeserver/machines/#{m.fqdn}")
      send(:"render_machine_#{m.spin}", m)
    end
  end

  def render_machine_nixos(machine)
    tpl = <<~ERB
      MENU TITLE <%= m.label %>

      LABEL <%= m.fqdn %>
        MENU LABEL <%= m.label %>
        LINUX boot/<%= m.fqdn %>/<%= m.current.generation %>/bzImage
        INITRD boot/<%= m.fqdn %>/<%= m.current.generation %>/initrd
        APPEND init=<%= m.current.toplevel %>/init loglevel=7

      <% m.generations[1..].each do |g| -%>
      LABEL <%= m.fqdn %>-<%= g.generation %>
        MENU LABEL Configuration <%= g.generation %> - <%= g.time_s %> - <%= g.shortrev %>
        LINUX boot/<%= m.fqdn %>/<%= g.generation %>/bzImage
        INITRD boot/<%= m.fqdn %>/<%= g.generation %>/initrd
        APPEND init=<%= g.toplevel %>/init loglevel=7

      <% end -%>
      <% if enable_memtest -%>
      LABEL memtest
        MENU LABEL Memtest86
        LINUX boot/memtest86/memtest.bin
        APPEND <%= memtest_params %>

      <% end -%>
      LABEL mainmenu
        MENU LABEL < Back to Main Menu
        KERNEL menu.c32
        APPEND pxelinux.cfg/default
    ERB

    render_to(
      tpl,
      {
        m: machine,
        enable_memtest: @config.memtest.enable,
        memtest_params: @config.memtest.params_line
      },
      "pxeserver/machines/#{machine.fqdn}/menu.cfg"
    )
  end

  def render_machine_vpsadminos(machine)
    render_machine_vpsadminos_config(
      machine,
      generation: machine.current,
      file: 'menu',
      root: true
    )

    render_machine_vpsadminos_config(
      machine,
      generation: machine.current,
      file: 'auto',
      timeout: 5,
      root: true
    )

    render_generations(machine)

    machine.generations.each do |g|
      render_machine_vpsadminos_config(
        machine,
        generation: g,
        file: "generation-#{g.generation}",
        root: false
      )
    end
  end

  # @param machine [Machine]
  # @param generation [Generation]
  # @param file [String] config base name
  # @param root [Boolean] true if this is machine menu page, not generation menu
  # @param timeout [Integer, nil] timeout in seconds until the default action is taken
  def render_machine_vpsadminos_config(machine, generation:, file:, root:, timeout: nil)
    variants = {
      default: {
        label: 'Default runlevel',
        kernel_params: [],
        runlevel: 'default'
      },
      nopools: {
        label: 'Default runlevel without container imports',
        kernel_params: ['osctl.pools=0'],
        runlevel: 'default'
      },
      nostart: {
        label: 'Default runlevel without container autostart',
        kernel_params: ['osctl.autostart=0'],
        runlevel: 'default'
      },
      rescue: {
        label: 'Rescue runlevel (network and sshd)',
        kernel_params: [],
        runlevel: 'rescue'
      },
      single: {
        label: 'Single-user runlevel (console only)',
        kernel_params: [],
        runlevel: 'single'
      }
    }

    tpl = <<~ERB
      <% if timeout -%>
      DEFAULT menu.c32
      TIMEOUT 50
      <% end -%>
      MENU TITLE <%= m.label %> (<%= g.generation %> - <%= g.current ? 'current' : g.time_s %> - <%= g.shortrev %>)

      <% variants.each do |variant, vopts| -%>
      LABEL <%= variant %>
        MENU LABEL <%= vopts[:label] %>
        LINUX boot/<%= m.fqdn %>/<%= g.generation %>/bzImage
        INITRD boot/<%= m.fqdn %>/<%= g.generation %>/initrd
        APPEND httproot=<%= File.join(http_url, m.fqdn, g.generation.to_s, 'root.squashfs') %> <%= g.kernel_params.join(' ') %> runlevel=<%= vopts[:runlevel] %> <%= vopts[:kernel_params].join(' ') %>

      <% end -%>
      LABEL <%= m.fqdn %>-generations
        MENU LABEL <%= root ? 'Generations >' : '< Back to Generations' %>
        KERNEL menu.c32
        APPEND pxeserver/machines/<%= m.fqdn %>/generations.cfg

      <% if enable_memtest -%>
      LABEL memtest
        MENU LABEL Memtest86
        LINUX boot/memtest86/memtest.bin
        APPEND <%= memtest_params %>

      <% end -%>
      LABEL mainmenu
        MENU LABEL < Back to Main Menu
        KERNEL menu.c32
        APPEND pxelinux.cfg/default
    ERB

    render_to(
      tpl,
      {
        m: machine,
        g: generation,
        variants:,
        http_url: @config.http_url,
        root:,
        timeout:,
        enable_memtest: @config.memtest.enable,
        memtest_params: @config.memtest.params_line
      },
      "pxeserver/machines/#{machine.fqdn}/#{file}.cfg"
    )
  end

  def render_generations(machine)
    tpl = <<~ERB
      MENU TITLE <%= m.label %> - generations

      <% m.generations.each do |g| -%>
      LABEL generations
        MENU LABEL Configuration <%= g.generation %> - <%= g.time_s %> - <%= g.shortrev %>
        KERNEL menu.c32
        APPEND pxeserver/machines/<%= m.fqdn %>/generation-<%= g.generation %>.cfg

      <% end -%>

      LABEL machine
        MENU LABEL < Back to <%= m.label %>
        KERNEL menu.c32
        APPEND pxeserver/machines/<%= m.fqdn %>/menu.cfg

      LABEL mainmenu
        MENU LABEL < Back to Main Menu
        KERNEL menu.c32
        APPEND pxelinux.cfg/default
    ERB

    render_to(tpl, { m: machine }, "pxeserver/machines/#{machine.fqdn}/generations.cfg")
  end

  # @param template [String]
  # @param vars [Hash]
  def render(template, vars)
    erb = ERB.new(template, trim_mode: '-')
    erb.result_with_hash(vars)
  end

  # @param template [String]
  # @param vars [Hash]
  # @param path [String]
  def render_to(template, vars, path)
    File.write(File.join(root, path), render(template, vars))
  end
end

class HttpBuilder < RootBuilder
  def build
    machines.each do |m|
      m.generations.each do |g|
        begin
          rootfs = File.realpath(File.join(g.link_path, 'root.squashfs'))
        rescue Errno::ENOENT
          next
        end

        gen_path = File.join(m.fqdn, g.generation.to_s)

        mkdir_p(gen_path)
        ln_s(rootfs, File.join(gen_path, 'root.squashfs'))
        ln_s(g.generation.to_s, File.join(m.fqdn, 'current')) if g.current
      end
    end
  end
end

class Machine
  # @return [String]
  attr_reader :name

  # @return [String]
  attr_reader :spin

  # @return [String]
  attr_reader :fqdn

  # @return [String]
  attr_reader :label

  # @return [Array<Generation>]
  attr_reader :generations

  # @return [Generation]
  attr_reader :current

  # @param name [String] machine name
  def initialize(name)
    @name = name
    @spin = 'nixos'
    @fqdn = name
    @label = name
    @toplevel = nil
    @macs = []
    @generations = []
    @current = nil
  end

  # @param link_path [String] Nix store path
  # @param generation [Integer]
  def add_generation(link_path, generation)
    @generations << Generation.new(link_path, generation)
    nil
  end

  def resolve
    sort_generations
    load_json
  end

  protected

  def sort_generations
    @generations.sort! do |a, b|
      b.generation <=> a.generation
    end
  end

  def load_json
    @current = @generations.detect(&:current)

    if @current.nil?
      raise "Unable to find current generation of machine #{name}"
    end

    @spin = @current.json.fetch('spin', spin)
    @fqdn = @current.json.fetch('fqdn', name)

    # rubocop:disable Naming/MemoizedInstanceVariableName
    @label = @current.json.fetch('label', nil)
    @label ||= @current.json.fetch('fqdn', nil)
    @label ||= name
    # rubocop:enable Naming/MemoizedInstanceVariableName
  end
end

class Generation
  # @return [String]
  attr_reader :link_path

  # @return [String]
  attr_reader :store_path

  # @return [Integer]
  attr_reader :generation

  # @return [Time]
  attr_reader :time

  # @return [String]
  attr_reader :time_s

  # @return [Array<String>]
  attr_reader :kernel_params

  # @return [String] Nix store path to `config.system.build.toplevel`
  attr_reader :toplevel

  # @return [String, nil] system.nixos.version
  attr_reader :version

  # @return [String, nil] system.nixos.revision
  attr_reader :revision

  # @return [String, nil]
  attr_reader :shortrev

  # @return [Array<String>]
  attr_reader :macs

  # @return [Boolean]
  attr_accessor :current

  # @return [Hash] contents of `machine.json`
  attr_reader :json

  # @param link_path [String] Nix store path
  # @param generation [Integer]
  def initialize(link_path, generation)
    @link_path = link_path
    @store_path = File.realpath(link_path)
    @generation = generation
    @time = File.lstat(link_path).mtime
    @time_s = @time.strftime('%Y-%m-%d %H:%M:%S')
    @current = false

    @json = JSON.parse(File.read(File.join(link_path, 'machine.json')))
    @toplevel = json.fetch('toplevel')
    @version = json.fetch('version', nil)
    @revision = json.fetch('revision', nil)

    @shortrev =
      if @revision
        @revision[0..8]
      elsif @version
        @version.split('.').last
      end

    @macs = json.fetch('macs', [])

    kernel_params_file = File.join(store_path, 'kernel-params')

    @kernel_params =
      if File.exist?(kernel_params_file)
        File.read(kernel_params_file).strip.split
      else
        json.fetch('kernelParams', [])
      end
  end
end

NetbootBuilder.run
