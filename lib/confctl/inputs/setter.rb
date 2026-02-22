require 'confctl/flake_lock'
require 'confctl/flake_lock_diff'
require 'confctl/inputs/commit_message'
require 'confctl/inputs/git_commit'
require 'confctl/system_command'
require 'fileutils'
require 'json'
require 'securerandom'

module ConfCtl
  module Inputs
    class Setter
      def self.run!(conf_dir:, inputs:, rev:, commit:, changelog:, downgrade:, editor:)
        raise ArgumentError, 'inputs empty' if inputs.nil? || inputs.empty?

        lock_path = File.join(conf_dir, 'flake.lock')
        unless File.exist?(lock_path)
          raise ConfCtl::Error, 'flake.lock missing; run confctl inputs update first'
        end

        old_lock = ConfCtl::FlakeLock.load(lock_path)
        old_lock_data = JSON.parse(File.read(lock_path))

        saved_original = {}
        override_urls = {}

        inputs.each do |input|
          node_id = resolve_node_id(old_lock_data, input)
          saved_original[input] = deep_dup(old_lock_data.dig('nodes', node_id, 'original'))

          input_info = old_lock.input_info(input)
          type = input_info[:type]
          unless %w[github git].include?(type)
            raise ConfCtl::Error, "input '#{input}' has unsupported type '#{type}' for set"
          end

          override_urls[input] = build_override_url(input_info, rev)
        end

        apply_inputs!(conf_dir, inputs, override_urls, saved_original, lock_path)

        new_lock = ConfCtl::FlakeLock.load(lock_path)
        changes = ConfCtl::FlakeLockDiff.diff(old_lock, new_lock, inputs: inputs)

        return { changed: false, changes: [] } if changes.empty?

        if commit
          msg = ConfCtl::Inputs::CommitMessage.build(
            changes: changes,
            changelog: changelog,
            downgrade: downgrade,
            action: :set
          )
          ConfCtl::Inputs::GitCommit.commit!(
            conf_dir: conf_dir,
            message: msg,
            editor: editor,
            files: ['flake.lock']
          )
        end

        { changed: true, changes: changes }
      end

      def self.resolve_node_id(lock_data, input_name)
        node_id = lock_data.dig('nodes', 'root', 'inputs', input_name)

        return node_id if node_id.is_a?(String)
        return node_id[0] if node_id.is_a?(Array) && node_id[0].is_a?(String)

        raise ConfCtl::Error, "unknown input '#{input_name}'"
      end

      def self.build_override_url(input_info, rev)
        locked = input_info[:locked] || {}
        original = input_info[:original] || {}

        case input_info[:type]
        when 'github'
          owner = locked['owner'] || original['owner']
          repo = locked['repo'] || original['repo']
          dir = locked['dir'] || original['dir']
          raise ConfCtl::Error, 'missing github owner or repo' unless owner && repo

          url = "github:#{owner}/#{repo}/#{rev}"
          url += "?dir=#{dir}" if dir
          url
        when 'git'
          url = locked['url'] || original['url']
          raise ConfCtl::Error, 'missing git url' unless url

          url = url.sub(/^git\+/, '')
          separator = url.include?('?') ? '&' : '?'
          "git+#{url}#{separator}rev=#{rev}"
        else
          raise ConfCtl::Error, "unsupported input type '#{input_info[:type]}'"
        end
      end

      def self.run_nix_flake_lock!(conf_dir, input, override_url, tmpfile)
        cmd = ConfCtl::SystemCommand.new
        extra_experimental = false

        loop do
          args = ['nix']
          if extra_experimental
            args << '--extra-experimental-features' << 'nix-command'
            args << '--extra-experimental-features' << 'flakes'
          end
          args << 'flake' << 'lock'
          args << '--update-input' << input
          args << '--override-input' << input << override_url
          args << '--output-lock-file' << tmpfile

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

      def self.experimental_error?(message)
        message.match?(/experimental/i) && message.match?(/nix-command|flakes/i)
      end

      def self.apply_inputs!(conf_dir, inputs, override_urls, saved_original, lock_path)
        inputs.each do |input|
          tmpfile = build_tmp_lock_path(conf_dir)

          begin
            run_nix_flake_lock!(conf_dir, input, override_urls[input], tmpfile)

            tmp_lock_data = JSON.parse(File.read(tmpfile))
            inputs.each do |name|
              node_id = resolve_node_id(tmp_lock_data, name)
              node = tmp_lock_data.dig('nodes', node_id) || {}
              original = saved_original[name]

              if original.nil?
                node.delete('original')
              else
                node['original'] = deep_dup(original)
              end

              tmp_lock_data['nodes'][node_id] = node
            end

            File.write(tmpfile, "#{JSON.pretty_generate(tmp_lock_data)}\n")
            File.rename(tmpfile, lock_path)
          ensure
            FileUtils.rm_f(tmpfile) if tmpfile && File.exist?(tmpfile)
          end
        end
      end

      def self.deep_dup(obj)
        return nil if obj.nil?

        Marshal.load(Marshal.dump(obj))
      end

      def self.build_tmp_lock_path(conf_dir)
        File.join(conf_dir, "flake.lock.tmp-#{Process.pid}-#{SecureRandom.hex(6)}")
      end

      private_class_method :resolve_node_id, :build_override_url, :run_nix_flake_lock!,
                           :experimental_error?, :apply_inputs!, :deep_dup, :build_tmp_lock_path
    end
  end
end
