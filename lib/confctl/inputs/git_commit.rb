module ConfCtl
  module Inputs
    class GitCommit
      def self.commit!(conf_dir:, message:, editor:, files:)
        args = %w[git commit]
        args << '-e' if editor && $stdout.tty?
        args << '-m' << message
        args.concat(files)

        Dir.chdir(conf_dir) do
          ok = Kernel.system(*args)
          raise 'git commit failed' unless ok
        end
      end
    end
  end
end
