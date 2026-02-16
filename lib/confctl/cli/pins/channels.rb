require 'confctl/cli/command'
require 'confctl/config_type'
require 'confctl/flake_lock'
require 'confctl/nix'
require 'confctl/pattern'
require 'confctl/pins/updater'

module ConfCtl::Cli
  class Pins::Channels < Command
    def list
      ensure_flake_config!

      raise GLI::BadCommandLine, 'usage: confctl pins channel ls [channel-pattern]' if args.length > 1

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

      channels = eval_channels
      selected = select_channels(channels.keys, selector)
      raise ConfCtl::Error, "no channels matched '#{selector}'" if selected.empty?

      inputs = selected.flat_map do |ch|
        mapping = channels[ch] || {}
        if role
          v = mapping[role] || mapping[role.to_s]
          v ? [v] : []
        else
          mapping.values
        end
      end.compact.uniq

      raise ConfCtl::Error, 'no inputs selected (check role name?)' if inputs.empty?

      puts "Channels: #{selected.join(', ')}"
      puts "Inputs: #{inputs.join(', ')}"

      res = ConfCtl::Pins::Updater.run!(
        conf_dir: ConfCtl::ConfDir.path,
        inputs: inputs,
        commit: opts[:commit],
        changelog: opts[:changelog],
        downgrade: opts[:downgrade],
        editor: opts[:editor]
      )

      puts(res[:changed] ? "Updated #{res[:changes].length} inputs." : 'No changes.')
    end

    protected

    def ensure_flake_config!
      return if ConfCtl::ConfigType.flake?(ConfCtl::ConfDir.path)

      raise ConfCtl::Error, 'confctl pins channel is available only in flake configs'
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
  end
end
