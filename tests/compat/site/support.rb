# frozen_string_literal: true

require 'digest'
require 'fileutils'
require 'json'
require 'open3'
require 'rbconfig'
require 'tmpdir'

module CompatibilityOracle
  ROOT = File.expand_path('..', __dir__)

  def self.verify_sources(directory)
    manifest = JSON.parse(File.read(File.join(directory, 'sources.json')))
    manifest.fetch('sources').each do |source|
      file = File.join(directory, source.fetch('snapshot'))
      raise "Changed oracle source: #{file}" unless Digest::SHA256.file(file).hexdigest == source.fetch('sha256')
    end
    manifest
  end

  def self.run(group, scenario, input: '')
    directory = File.join(ROOT, group)
    verify_sources(directory)

    Dir.mktmpdir("confctl-compat-#{group}-") do |root|
      File.write(File.join(root, 'scenario.json'), JSON.generate(scenario))
      environment = {
        'CONFCTL_COMPAT_ROOT' => root,
        'CONFCTL_RUBY_ORACLE_ROOT' => ENV.fetch('CONFCTL_RUBY_ORACLE_ROOT', File.expand_path('../../..', __dir__)),
        'CONFCTL_TTY' => '0', 'NO_COLOR' => '1', 'PAGER' => '', 'TZ' => 'UTC',
        'CONFCTL_SSH_CONFIG' => scenario['ssh_config']
      }
      startup = startup_stderr(environment)
      out, err, status = Open3.capture3(environment, RbConfig.ruby, File.join(directory, 'driver.rb'), stdin_data: input)
      result = {
        'stdout' => out, 'stderr' => err, 'exit_status' => status.exitstatus,
        'startup_stderr' => startup,
        'application_stderr' => err.start_with?(startup) ? err.delete_prefix(startup) : err,
        'signal' => status.termsig, 'root' => root,
        'runtime' => { 'ruby' => RUBY_DESCRIPTION, 'gems' => Gem.loaded_specs.transform_values { |spec| spec.version.to_s } },
        'observation' => JSON.parse(File.read(File.join(root, 'observation.json')))
      }

      if ENV['CONFCTL_COMPAT_CAPTURE_DIR']
        FileUtils.mkdir_p(ENV.fetch('CONFCTL_COMPAT_CAPTURE_DIR'))
        name = scenario.fetch('id')
        raise 'Invalid capture name' unless /\A[a-z0-9_-]+\z/.match?(name)

        File.write(File.join(ENV.fetch('CONFCTL_COMPAT_CAPTURE_DIR'), "#{name}.json"), JSON.pretty_generate(result))
      end

      result
    end
  end

  def self.startup_stderr(environment)
    return @startup_stderr if defined?(@startup_stderr)

    out, err, status = Open3.capture3(environment, RbConfig.ruby, '-e', '')
    raise 'Ruby bootstrap failed' unless status.success? && out.empty?

    # This exact block is emitted even for empty Ruby, before any oracle source.
    # Retain it and raw application stderr separately; accept no other noise.
    constants = %w[JAVA MSWIN MSWIN64 MINGW X64_MINGW WINDOWS X64_LINUX X64_LINUX_MUSL]
    lines = err.lines
    known = lines.length == 16 && constants.each_with_index.all? do |constant, index|
      lines[index * 2].match?(%r{\A.+/bundler-2\.5\.7/lib/bundler/rubygems_ext\.rb:\d+: warning: already initialized constant Gem::Platform::#{constant}\n\z}) &&
        lines[(index * 2) + 1].match?(%r{\A/nix/store/[^/]+-ruby-3\.4\.9/lib/ruby/3\.4\.0/rubygems/platform\.rb:\d+: warning: previous definition of #{constant} was here\n\z})
    end
    raise "Unexpected Ruby startup diagnostics: #{err}" unless err.empty? || known

    @startup_stderr = err
  end
end
