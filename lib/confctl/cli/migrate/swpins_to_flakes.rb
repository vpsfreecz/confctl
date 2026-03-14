require 'confctl/cli/command'
require 'confctl/conf_dir'
require 'confctl/inputs/setter'
require 'confctl/nix_legacy'
require 'confctl/system_command'
require 'fileutils'
require 'json'

module ConfCtl::Cli
  class Migrate::SwpinsToFlakes < Command
    INPUTS_BEGIN = '# confctl migrate swpins-to-flakes: BEGIN inputs'.freeze
    INPUTS_END = '# confctl migrate swpins-to-flakes: END inputs'.freeze
    CHANNELS_BEGIN = '# confctl migrate swpins-to-flakes: BEGIN channels'.freeze
    CHANNELS_END = '# confctl migrate swpins-to-flakes: END channels'.freeze

    def all
      return unless flake
      return unless machines

      imports
      clean

      puts "\nNext steps:"
      puts '  nix develop'
      puts '  confctl ls'
      puts '  confctl build <one-machine>'
    end

    def flake
      step('Flake')

      flake_path = File.join(conf_dir, 'flake.nix')
      swpins_config = File.join(conf_dir, 'configs', 'swpins.nix')
      core_path = File.join(conf_dir, 'swpins', 'core.json')

      raise ConfCtl::Error, 'flake.nix already exists' if File.exist?(flake_path)
      raise ConfCtl::Error, 'configs/swpins.nix missing' unless File.exist?(swpins_config)
      raise ConfCtl::Error, 'swpins/core.json missing; update swpins state first' unless File.exist?(core_path)

      legacy_channels = load_legacy_channels
      channel_names = legacy_channels.keys.sort

      channel_state = {}
      channel_names.each do |channel|
        path = File.join(conf_dir, 'swpins', 'channels', "#{channel}.json")
        unless File.exist?(path)
          raise ConfCtl::Error,
                "swpins/channels/#{channel}.json missing; update swpins state first"
        end

        channel_state[channel] = load_json(path, "swpins/channels/#{channel}.json")
      end

      core_json = load_json(core_path, 'swpins/core.json')
      core_entry = core_json['nixpkgs']
      raise ConfCtl::Error, 'swpins/core.json missing nixpkgs entry; update swpins state first' unless core_entry

      core_spec = build_pin_spec(core_entry, 'core nixpkgs')
      core_url = flake_url_for(core_spec)

      plan = build_channel_plan(legacy_channels, channel_state, core_spec)
      input_defs = plan[:input_defs]
      channels = plan[:channels]
      input_revs = plan[:input_revs]

      flake_content = render_flake(core_url, input_defs, channels)
      @generated_flake_content = flake_content

      ops = []
      ops << [:create, 'flake.nix']
      ops << [:run, 'nix flake lock']
      ops << [:set, "root nixpkgs rev #{core_spec[:rev]}"] if core_spec[:rev]

      channels.keys.sort.each do |channel|
        channels[channel].keys.sort.each do |role|
          input_name = channels[channel][role]
          rev = input_revs[input_name]
          next unless rev

          ops << [:set, "channel #{channel} role #{role} rev #{rev}"]
        end
      end

      return false unless confirm_step(ops, label: 'flake migration', required: true)
      return true if dry_run?

      write_atomic(flake_path, flake_content)
      run_nix_flake_lock!
      apply_input_revisions(input_revs)
      true
    end

    def machines
      step('Machines')

      flake_path = File.join(conf_dir, 'flake.nix')

      cluster_dir = File.join(conf_dir, 'cluster')
      module_paths = Dir.glob(File.join(cluster_dir, '**', 'module.nix'))
      if module_paths.empty?
        puts 'No cluster/**/module.nix files found.'
        return true
      end

      flake_content =
        if File.exist?(flake_path)
          File.read(flake_path)
        elsif dry_run? && @generated_flake_content
          @generated_flake_content
        else
          raise ConfCtl::Error, 'flake.nix missing; run flake migration first'
        end

      edits = {}
      override_inputs = {}
      override_revs = {}
      set_ops = []

      module_paths.each do |path|
        relative = path.sub(%r{\A#{Regexp.escape(cluster_dir)}/}, '')
        machine = File.dirname(relative)
        next if machine == '.'

        content = File.read(path)
        updated = content.gsub(/\bswpins\.channels\b/, 'inputs.channels')

        overrides_json = load_machine_overrides(machine)
        overrides = {}
        overrides_json.keys.map(&:to_s).sort.each do |role|
          spec_entry = overrides_json[role]
          spec = build_pin_spec(spec_entry, "machine #{machine} role #{role}")
          input_name = sanitize_input_name("#{role}__#{machine}")
          overrides[role.to_s] = { input: input_name, spec: spec }
        end

        if overrides.any?
          mapping = overrides.transform_values { |v| v[:input] }
          updated, status = insert_overrides(updated, mapping)

          if status == :missing_anchor
            msg = "Warning: could not find inputs.channels in cluster/#{machine}/module.nix; " \
                  'skipping overrides for this machine'
            puts Rainbow(msg).yellow
          else
            overrides.each do |role, data|
              input_name = data[:input]
              spec = data[:spec]
              identity = pin_identity(spec)

              if override_inputs[input_name] && pin_identity(override_inputs[input_name]) != identity
                raise ConfCtl::Error, "conflicting override input '#{input_name}' for machine #{machine}"
              end

              override_inputs[input_name] ||= spec

              rev = spec[:rev]
              next unless rev

              if override_revs[input_name] && override_revs[input_name] != rev
                raise ConfCtl::Error, "conflicting revisions for override input '#{input_name}'"
              end

              override_revs[input_name] = rev
              set_ops << [:set, "machine #{machine} role #{role} rev #{rev}"]
            end
          end
        end

        edits[path] = updated if updated != content
      end

      new_flake_content, added_inputs = add_inputs_to_flake(flake_content, override_inputs)
      edits[flake_path] = new_flake_content if added_inputs.any?

      ops = edits.keys.sort.map { |path| [:edit, display_path(path)] }
      ops.concat(set_ops)

      return true if ops.empty? && override_inputs.empty?
      return false unless confirm_step(ops, label: 'machine metadata migration', required: true)
      return true if dry_run?

      edits.each do |path, content|
        write_atomic(path, content)
      end

      if override_revs.any? && !File.exist?(File.join(conf_dir, 'flake.lock'))
        raise ConfCtl::Error, 'flake.lock missing; run nix flake lock first'
      end

      apply_input_revisions(override_revs)
      true
    end

    def imports
      step('Imports')

      matches = scan_legacy_imports
      if matches.empty?
        puts 'No legacy NIX_PATH imports detected.'
        return true
      end

      puts Rainbow('Legacy NIX_PATH imports detected:').yellow
      matches.each do |m|
        puts "  #{m[:path]}:#{m[:line]}: #{m[:content]}"
      end

      puts
      puts 'Pure flake evaluation will fail unless these imports are migrated or legacy mode is enabled.'

      change = legacy_confctl_change
      if change.nil?
        puts 'Compatibility options already set in configs/confctl.nix.'
        return true
      end

      op(:edit, 'configs/confctl.nix')
      proceed = ask_confirmation { puts 'Enable compatibility mode in configs/confctl.nix?' }
      return true unless proceed
      return true if dry_run?

      write_confctl_change(change)
      true
    end

    def clean
      step('Clean')

      flake_path = File.join(conf_dir, 'flake.nix')
      unless File.exist?(flake_path) || (dry_run? && @generated_flake_content)
        raise ConfCtl::Error, 'flake.nix missing; refusing to remove swpins artifacts'
      end

      ops = []
      swpins_config = File.join(conf_dir, 'configs', 'swpins.nix')
      swpins_dir = File.join(conf_dir, 'swpins')

      ops << [:delete, 'configs/swpins.nix'] if File.exist?(swpins_config)
      ops << [:delete, 'swpins/'] if Dir.exist?(swpins_dir)

      if ops.any?
        return true unless confirm_step(ops, label: 'swpins cleanup', required: false)
        return true if dry_run?

        FileUtils.rm_f(swpins_config)
        FileUtils.rm_rf(swpins_dir)
      else
        puts 'No swpins artifacts to remove.'
      end

      shell_path = File.join(conf_dir, 'shell.nix')
      return true unless File.exist?(shell_path)

      op(:delete, 'shell.nix')
      proceed = ask_confirmation { puts 'Remove shell.nix?' }
      return true unless proceed
      return true if dry_run?

      FileUtils.rm_f(shell_path)
      true
    end

    protected

    def conf_dir
      ConfCtl::ConfDir.path
    end

    def step(title)
      puts Rainbow("\n== #{title} ==").bright.cyan
    end

    def op(kind, target)
      color =
        case kind
        when :create then :green
        when :edit then :yellow
        when :delete then :red
        when :run, :set then :blue
        else :white
        end
      puts "#{Rainbow(kind.to_s.upcase.ljust(6)).public_send(color)} #{target}"
    end

    def dry_run?
      opts[:'dry-run'] || opts[:dry_run]
    end

    def write_atomic(path, content)
      tmp = "#{path}.new"
      File.write(tmp, content)
      File.rename(tmp, path)
    end

    def display_path(path)
      prefix = "#{conf_dir}/"
      path.start_with?(prefix) ? path.delete_prefix(prefix) : path
    end

    def confirm_step(ops, label:, required:)
      if ops.empty?
        puts 'No changes.'
        return true
      end

      ops.each { |kind, target| op(kind, target) }

      if ask_confirmation
        true
      else
        action = required ? 'Aborting' : 'Skipping'
        puts Rainbow("#{action} #{label}.").yellow
        false
      end
    end

    def load_legacy_channels
      legacy = ConfCtl::NixLegacy.new(conf_dir: conf_dir)
      channels = legacy.list_swpins_channels
      unless channels.is_a?(Hash)
        raise ConfCtl::Error, 'unable to evaluate swpins channels from legacy config'
      end

      channels.transform_values { |v| v.is_a?(Hash) ? v : {} }
    end

    def load_machine_overrides(machine)
      safe_machine = machine.tr('/', ':')
      path = File.join(conf_dir, 'swpins', 'cluster', "#{safe_machine}.json")
      return {} unless File.exist?(path)

      load_json(path, "swpins/cluster/#{safe_machine}.json")
    end

    def load_json(path, label)
      JSON.parse(File.read(path))
    rescue JSON::ParserError => e
      raise ConfCtl::Error, "#{label} is not valid JSON: #{e.message}"
    end

    def build_pin_spec(entry, context)
      raise ConfCtl::Error, "#{context}: missing swpin state" unless entry.is_a?(Hash)

      type = entry['type']
      nix_opts = entry['nix_options'] || {}

      case type
      when 'git', 'git-rev'
        url = nix_opts['url']
        raise ConfCtl::Error, "#{context}: missing nix_options.url" if url.nil? || url == ''

        ref = normalize_ref(nix_opts.dig('update', 'ref'))
        submodules = nix_opts['fetchSubmodules'] ? true : false
        rev = entry.dig('state', 'rev')
        if rev.nil? || rev == ''
          raise ConfCtl::Error, "#{context}: missing state.rev; update swpins state first"
        end

        {
          type: type,
          url: url,
          ref: ref,
          submodules: submodules,
          rev: rev
        }
      when 'directory'
        path = nix_opts['path']
        raise ConfCtl::Error, "#{context}: missing nix_options.path" if path.nil? || path == ''

        {
          type: type,
          path: path,
          rev: entry.dig('state', 'rev')
        }
      else
        raise ConfCtl::Error, "#{context}: unsupported swpin type '#{type}'"
      end
    end

    def build_channel_plan(legacy_channels, channel_state, core_spec)
      used_names = {}
      used_names['nixpkgs'] = pin_identity(core_spec)
      used_names['confctl'] = :reserved

      input_defs = {}
      input_revs = {}
      input_revs['nixpkgs'] = core_spec[:rev] if core_spec[:rev]
      channels = {}

      legacy_channels.keys.sort.each do |channel|
        roles = legacy_channels[channel] || {}
        channels[channel] = {}

        roles.keys.map(&:to_s).sort.each do |role|
          state = channel_state[channel][role]
          unless state
            raise ConfCtl::Error,
                  "swpins/channels/#{channel}.json missing role '#{role}'; update swpins state first"
          end

          spec = build_pin_spec(state, "channel #{channel} role #{role}")
          identity = pin_identity(spec)

          base = sanitize_input_name(role)
          name = if used_names[base] && used_names[base] != identity
                   sanitize_input_name("#{role}__#{channel}")
                 else
                   base
                 end

          name = ensure_unique_name(name, identity, used_names)
          used_names[name] = identity

          channels[channel][role] = name

          unless input_defs.has_key?(name)
            input_defs[name] = {
              url: flake_url_for(spec),
              flake: false
            }
          end

          rev = spec[:rev]
          next unless rev

          if input_revs[name] && input_revs[name] != rev
            raise ConfCtl::Error, "conflicting revisions for input '#{name}'"
          end

          input_revs[name] = rev
        end
      end

      input_defs.delete('nixpkgs')
      { input_defs: input_defs, channels: channels, input_revs: input_revs }
    end

    def ensure_unique_name(base, identity, used_names)
      name = base
      idx = 2
      while used_names.has_key?(name) && used_names[name] != identity
        name = "#{base}__#{idx}"
        idx += 1
      end
      name
    end

    def pin_identity(spec)
      [
        spec[:type],
        spec[:url],
        spec[:path],
        spec[:ref],
        spec[:submodules],
        spec[:rev]
      ]
    end

    def sanitize_input_name(name)
      sanitized = name.to_s.gsub(/[^A-Za-z0-9_-]/, '_')
      raise ConfCtl::Error, "unable to sanitize input name from '#{name}'" if sanitized.empty?

      sanitized
    end

    def normalize_ref(ref)
      return nil if ref.nil? || ref == ''

      ref.sub(%r{\Arefs/(heads|tags)/}, '')
    end

    def github_repo_from_url(url)
      m = url.match(%r{\Ahttps://github\.com/([^/]+)/([^/]+?)(?:\.git)?/?\z})
      return nil unless m

      [m[1], m[2]]
    end

    def flake_url_for(spec)
      case spec[:type]
      when 'directory'
        "path:#{spec[:path]}"
      when 'git', 'git-rev'
        url = spec[:url].to_s.sub(/\Agit\+/, '')
        ref = spec[:ref]
        submodules = spec[:submodules]

        gh = github_repo_from_url(url)
        if gh && !submodules
          base = "github:#{gh[0]}/#{gh[1]}"
          return ref ? "#{base}/#{ref}" : base
        end

        git_url = "git+#{url}"
        git_url = append_query(git_url, 'ref', ref) if ref && ref != ''
        git_url = append_query(git_url, 'submodules', '1') if submodules
        git_url
      else
        raise ConfCtl::Error, "unsupported swpin type '#{spec[:type]}'"
      end
    end

    def append_query(url, key, value)
      separator = url.include?('?') ? '&' : '?'
      "#{url}#{separator}#{key}=#{value}"
    end

    def nix_attr_key(name)
      str = name.to_s
      if str.match?(/\A[A-Za-z_][A-Za-z0-9_]*\z/)
        str
      else
        "\"#{nix_string(str)}\""
      end
    end

    def nix_attr_path(parts)
      parts.map { |p| nix_attr_key(p) }.join('.')
    end

    def nix_string(str)
      str.to_s.gsub('\\', '\\\\').gsub('"', '\"')
    end

    def render_flake(core_url, input_defs, channels)
      lines = []
      lines << '{'
      lines << '  description = "confctl migrated swpins config";'
      lines << ''
      lines << '  inputs = {'
      lines << '    confctl.url = "github:vpsfreecz/confctl";'
      lines << "    nixpkgs.url = \"#{nix_string(core_url)}\";"
      lines << ''
      lines << "    #{INPUTS_BEGIN}"
      lines.concat(render_input_defs(input_defs, '    '))
      lines << "    #{INPUTS_END}"
      lines << '  };'
      lines << ''
      lines << '  outputs = inputs@{ self, confctl, ... }:'
      lines << '    let'
      lines << '      channels = {'
      lines << "        #{CHANNELS_BEGIN}"
      lines.concat(render_channels(channels, '        '))
      lines << "        #{CHANNELS_END}"
      lines << '      };'
      lines << '    in'
      lines << '    {'
      lines << '      confctl = confctl.lib.mkConfctlOutputs {'
      lines << '        confDir = ./.;'
      lines << '        inherit inputs channels;'
      lines << '      };'
      lines << ''
      lines << '      devShells.x86_64-linux.default = confctl.lib.mkConfigDevShell {'
      lines << '        system = "x86_64-linux";'
      lines << '        mode = "minimal";'
      lines << '      };'
      lines << '    };'
      lines << '}'
      lines << ''
      lines.join("\n")
    end

    def render_input_defs(input_defs, indent)
      lines = []
      input_defs.keys.sort.each do |name|
        spec = input_defs[name]
        key = nix_attr_key(name)
        lines << "#{indent}#{key} = {"
        lines << "#{indent}  url = \"#{nix_string(spec[:url])}\";"
        lines << "#{indent}  flake = false;" if spec[:flake] == false
        lines << "#{indent}};"
        lines << ''
      end
      lines
    end

    def render_channels(channels, indent)
      lines = []
      channels.keys.sort.each do |channel|
        roles = channels[channel] || {}
        lines << "#{indent}#{nix_attr_key(channel)} = {"
        roles.keys.sort.each do |role|
          input_name = roles[role]
          lines << "#{indent}  #{nix_attr_key(role)} = \"#{nix_string(input_name)}\";"
        end
        lines << "#{indent}};"
        lines << ''
      end
      lines
    end

    def insert_overrides(content, overrides)
      return [content, :no_overrides] if overrides.empty?

      lines = content.lines
      idx = lines.index { |line| line.match?(/\binputs\.channels\b/) && !line.lstrip.start_with?('#') }
      return [content, :missing_anchor] unless idx

      indent = lines[idx][/^\s*/]
      new_lines = []

      overrides.each do |role, input_name|
        next if override_line_present?(content, role)

        attr_path = nix_attr_path(%w[inputs overrides] + [role])
        new_lines << "#{indent}#{attr_path} = \"#{nix_string(input_name)}\";\n"
      end

      return [content, :already_present] if new_lines.empty?

      lines.insert(idx + 1, *new_lines)
      [lines.join, :inserted]
    end

    def override_line_present?(content, role)
      if role.match?(/\A[A-Za-z_][A-Za-z0-9_]*\z/)
        content.match?(/\binputs\.overrides\.#{Regexp.escape(role)}\b/)
      else
        quoted = Regexp.escape("\"#{role}\"")
        content.match?(/inputs\.overrides\.#{quoted}/)
      end
    end

    def add_inputs_to_flake(content, inputs)
      return [content, []] if inputs.empty?

      lines = content.lines
      begin_idx = lines.index { |line| line.include?(INPUTS_BEGIN) }
      end_idx = lines.index { |line| line.include?(INPUTS_END) }

      unless begin_idx && end_idx && begin_idx < end_idx
        raise ConfCtl::Error, 'flake.nix missing inputs marker block'
      end

      marker_indent = lines[begin_idx][/^\s*/]
      entry_indent = marker_indent
      lines[(begin_idx + 1)...end_idx].each do |line|
        stripped = line.strip
        next if stripped.empty? || stripped.start_with?('#')

        entry_indent = line[/^\s*/]
        break
      end

      existing = extract_input_names(lines[(begin_idx + 1)...end_idx], entry_indent)

      new_names = inputs.keys.reject { |n| existing.include?(n) }.sort
      return [content, []] if new_names.empty?

      new_lines = []
      new_names.each do |name|
        spec = inputs[name]
        key = nix_attr_key(name)
        new_lines << "#{entry_indent}#{key} = {"
        new_lines << "#{entry_indent}  url = \"#{nix_string(flake_url_for(spec))}\";"
        new_lines << "#{entry_indent}  flake = false;"
        new_lines << "#{entry_indent}};"
        new_lines << ''
      end

      lines.insert(end_idx, *new_lines)
      [lines.join, new_names]
    end

    def extract_input_names(lines, entry_indent)
      names = []
      lines.each do |line|
        stripped = line.strip
        next if stripped.empty? || stripped.start_with?('#')
        next unless line.start_with?(entry_indent)
        next if line.start_with?("#{entry_indent} ")

        match = stripped.match(/^([^=\s]+)\s*=\s*/)
        match ||= stripped.match(/^([^=\s]+)\.url\s*=/)
        next unless match

        name = unquote(match[1])
        names << name
      end
      names
    end

    def unquote(str)
      return str unless str.start_with?('"') && str.end_with?('"')

      str[1..-2]
    end

    def scan_legacy_imports
      matches = []
      dirs = %w[cluster configs modules environments data overlays]
      dirs.each do |dir|
        base = File.join(conf_dir, dir)
        next unless Dir.exist?(base)

        Dir.glob(File.join(base, '**', '*.nix')).each do |path|
          File.readlines(path).each_with_index do |line, idx|
            next unless line.match?(/<[^>\n]+>/)

            matches << {
              path: display_path(path),
              line: idx + 1,
              content: line.rstrip
            }
          end
        end
      end
      matches
    end

    def legacy_confctl_change
      path = File.join(conf_dir, 'configs', 'confctl.nix')

      if File.exist?(path)
        content = File.read(path)
        needs_impure = !content.match?(/\bconfctl\.nix\.impureEval\s*=/)
        needs_legacy = !content.match?(/\bconfctl\.nix\.legacyNixPath\s*=/)
        return nil unless needs_impure || needs_legacy

        {
          path: path,
          content: content,
          needs_impure: needs_impure,
          needs_legacy: needs_legacy,
          create: false
        }
      else
        {
          path: path,
          content: nil,
          needs_impure: true,
          needs_legacy: true,
          create: true
        }
      end
    end

    def write_confctl_change(change)
      path = change[:path]

      if change[:create]
        FileUtils.mkdir_p(File.dirname(path))
        content = <<~NIX
          { ... }:
          {
            # confctl migrate swpins-to-flakes: legacy NIX_PATH compatibility
            confctl.nix.impureEval = true;
            confctl.nix.legacyNixPath = true;
          }
        NIX
        write_atomic(path, content)
        return
      end

      lines = change[:content].lines
      close_idx = lines.rindex { |line| line.strip == '}' }
      close_idx ||= lines.rindex { |line| line.strip == '};' }
      raise ConfCtl::Error, 'unable to edit configs/confctl.nix' unless close_idx

      indent = lines[close_idx][/^\s*/]
      inner_indent = "#{indent}  "

      insert_lines = []
      insert_lines << "#{inner_indent}# confctl migrate swpins-to-flakes: legacy NIX_PATH compatibility\n"
      insert_lines << "#{inner_indent}confctl.nix.impureEval = true;\n" if change[:needs_impure]
      insert_lines << "#{inner_indent}confctl.nix.legacyNixPath = true;\n" if change[:needs_legacy]

      lines.insert(close_idx, *insert_lines)
      write_atomic(path, lines.join)
    end

    def apply_input_revisions(input_revs)
      with_tracked_flake do
        input_revs.keys.sort.each do |input|
          rev = input_revs[input]
          next if rev.nil? || rev == ''

          ConfCtl::Inputs::Setter.run!(
            conf_dir: conf_dir,
            inputs: [input],
            rev: rev,
            commit: false,
            changelog: false,
            downgrade: false,
            editor: false
          )
        end
      end
    end

    def run_nix_flake_lock!
      with_tracked_flake do
        cmd = ConfCtl::SystemCommand.new
        extra_experimental = false

        loop do
          args = ['nix']
          if extra_experimental
            args << '--extra-experimental-features' << 'nix-command'
            args << '--extra-experimental-features' << 'flakes'
          end
          args << 'flake' << 'lock'

          begin
            Dir.chdir(conf_dir) { cmd.run(*args) }
            break
          rescue TTY::Command::ExitError => e
            if !extra_experimental && experimental_error?(e.message)
              extra_experimental = true
              next
            end

            raise
          end
        end
      end
    end

    def experimental_error?(message)
      message.match?(/experimental/i) && message.match?(/nix-command|flakes/i)
    end

    def with_tracked_flake
      flake_path = File.join(conf_dir, 'flake.nix')
      return yield unless File.exist?(flake_path)
      return yield unless git_repo?

      added = false

      unless git_tracked_file?('flake.nix')
        git_add_intent('flake.nix')
        added = true
      end

      yield
    ensure
      git_reset_file('flake.nix') if added
    end

    def git_repo?
      cmd = ConfCtl::SystemCommand.new
      Dir.chdir(conf_dir) { cmd.run('git', 'rev-parse', '--is-inside-work-tree') }
      true
    rescue TTY::Command::ExitError
      false
    end

    def git_tracked_file?(path)
      cmd = ConfCtl::SystemCommand.new
      Dir.chdir(conf_dir) { cmd.run('git', 'ls-files', '--error-unmatch', path) }
      true
    rescue TTY::Command::ExitError
      false
    end

    def git_add_intent(path)
      cmd = ConfCtl::SystemCommand.new
      Dir.chdir(conf_dir) { cmd.run('git', 'add', '--intent-to-add', path) }
    end

    def git_reset_file(path)
      cmd = ConfCtl::SystemCommand.new
      Dir.chdir(conf_dir) { cmd.run('git', 'reset', '-q', '--', path) }
    end
  end
end
