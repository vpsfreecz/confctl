require 'confctl/cli/command'
require 'confctl/config_type'
require 'confctl/flake_lock'
require 'confctl/nix'
require 'confctl/pattern'
require 'confctl/inputs/setter'
require 'confctl/inputs/updater'

module ConfCtl::Cli
  class Inputs::Channels < Command
    def list
      ensure_flake_config!

      raise GLI::BadCommandLine, 'usage: confctl inputs channel ls [channel-pattern]' if args.length > 1

      conf_dir = ConfCtl::ConfDir.path
      lock = ConfCtl::FlakeLock.load(File.join(conf_dir, 'flake.lock'))
      channels = eval_channels

      selected = select_channels(channels.keys, args[0])

      rows = []
      selected.each do |ch|
        (channels[ch] || {}).each do |role, input|
          info = lock.input_info(input)
          rows << {
            channel: ch,
            role: role,
            input: input,
            rev: info[:short_rev] || '-',
            url: info[:url] || '-'
          }
        end
      end

      OutputFormatter.print(rows, %i[channel role input rev url], layout: :columns)
    end

    def update
      ensure_flake_config!
      require_args!('channels', optional: ['role'])

      selector = args[0]
      role = args[1]
      role_name = role&.to_s

      channels = eval_channels
      selected = select_channels(channels.keys, selector)
      raise ConfCtl::Error, "no channels matched '#{selector}'" if selected.empty?

      targets = selected.flat_map do |ch|
        mapping = channels[ch] || {}
        if role_name
          input = mapping[role] || mapping[role_name]
          input ? [{ channel: ch, role: role_name, input: input }] : []
        else
          mapping.map { |r, input| { channel: ch, role: r.to_s, input: input } }
        end
      end

      inputs = targets.map { |t| t[:input] }.uniq

      raise ConfCtl::Error, 'no inputs selected (check role name?)' if inputs.empty?

      ConfCtl::Inputs::Updater.run!(
        conf_dir: ConfCtl::ConfDir.path,
        inputs: inputs,
        commit: opts[:commit],
        changelog: opts[:changelog],
        downgrade: opts[:downgrade],
        editor: opts[:editor]
      )

      lock = ConfCtl::FlakeLock.load(File.join(ConfCtl::ConfDir.path, 'flake.lock'))
      targets.each do |t|
        info = lock.input_info(t[:input])
        rev = info[:short_rev] || info[:rev] || '-'
        puts "Updating #{t[:role]} in #{t[:channel]} -> #{rev}"
      end
    end

    def set
      ensure_flake_config!
      require_args!('channels', 'role', 'rev')

      selector = args[0]
      role = args[1]
      rev = args[2]

      channels = eval_channels
      selected = select_channels(channels.keys, selector)
      raise ConfCtl::Error, "no channels matched '#{selector}'" if selected.empty?

      role_name = role.to_s
      targets = selected.filter_map do |ch|
        mapping = channels[ch] || {}
        input = mapping[role] || mapping[role_name]
        input ? { channel: ch, role: role_name, input: input } : nil
      end

      raise ConfCtl::Error, 'no inputs selected (check role name?)' if targets.empty?

      inputs = targets.map { |t| t[:input] }.uniq

      shared = shared_input_uses(channels, selected, role_name, inputs)
      if shared.any?
        msg = build_shared_input_message(shared)
        raise ConfCtl::Error, "#{msg} (use --allow-shared to proceed)" unless opts[:allow_shared]

        puts "Warning: #{msg}"
      end

      ConfCtl::Inputs::Setter.run!(
        conf_dir: ConfCtl::ConfDir.path,
        inputs: inputs,
        rev: rev,
        commit: opts[:commit],
        changelog: opts[:changelog],
        downgrade: opts[:downgrade],
        editor: opts[:editor]
      )

      lock = ConfCtl::FlakeLock.load(File.join(ConfCtl::ConfDir.path, 'flake.lock'))
      targets.each do |t|
        info = lock.input_info(t[:input])
        resolved_rev = info[:short_rev] || info[:rev] || '-'
        puts "Configuring #{t[:role]} in #{t[:channel]} -> #{resolved_rev}"
      end
    end

    protected

    def ensure_flake_config!
      return if ConfCtl::ConfigType.flake?(ConfCtl::ConfDir.path)

      raise ConfCtl::Error, 'confctl inputs channel is available only in flake configs'
    end

    def eval_channels
      json = ConfCtl::Nix.new.eval_json('.#confctl.channels')
      json.is_a?(Hash) ? json : {}
    end

    def select_channels(all, selector)
      return all.sort if selector.nil?

      if selector.include?('{') || selector.include?(',')
        names = parse_channel_selectors(selector)
        return names.select { |n| all.include?(n) }
      end

      all.select { |n| ConfCtl::Pattern.match?(selector, n) }.sort
    end

    def parse_channel_selectors(str)
      s = str.strip
      s = s[1..-2] if s.start_with?('{') && s.end_with?('}')
      s.split(',').map(&:strip).reject(&:empty?)
    end

    def shared_input_uses(channels, selected, role, inputs)
      requested = {}
      selected.each { |ch| requested[[ch, role.to_s]] = true }
      shared = {}

      channels.each do |ch, mapping|
        (mapping || {}).each do |r, input|
          next unless inputs.include?(input)

          key = [ch, r.to_s]
          next if requested[key]

          shared[input] ||= []
          shared[input] << { channel: ch, role: r.to_s }
        end
      end

      shared
    end

    def build_shared_input_message(shared)
      details = shared.sort.map do |input, uses|
        roles = uses.map { |u| "#{u[:channel]}/#{u[:role]}" }.sort.join(', ')
        "#{input} (also used by #{roles})"
      end.join('; ')

      "selected input(s) are shared outside the requested channel/role: #{details}"
    end
  end
end
