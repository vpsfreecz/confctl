require 'tempfile'

module ConfCtl
  module Inputs
    class GitCommit
      def self.commit!(conf_dir:, message:, editor:, files:)
        # Generated changelogs can exceed the process argument-size limit.
        Tempfile.create('confctl-commit-') do |message_file|
          message_file.write(message)
          message_file.flush

          args = %w[git commit]
          args << '-e' if editor && $stdout.tty?
          args << '-F' << File.expand_path(message_file.path)
          args.concat(files)

          Dir.chdir(conf_dir) do
            ok = Kernel.system(*args)
            raise 'git commit failed' unless ok
          end
        end
      end
    end
  end
end
