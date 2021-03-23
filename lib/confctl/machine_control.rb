module ConfCtl
  class MachineControl
    # @return [Deployment]
    attr_reader :deployment

    # @param deployment [Deployment]
    def initialize(deployment)
      @deployment = deployment
      @extra_ssh_opts = []
    end

    # Reboot the machine
    def reboot
      execute('reboot')
    end

    # Reboot the machine and wait for it to come back online
    # @param timeout [Integer, nil]
    # @yieldparam [:reboot, :went_down, :is_down, :is_up, :timeout] state
    # @return [Integer] seconds it took to reboot the machine
    def reboot_and_wait(timeout: nil)
      initial_uptime = get_uptime
      t = Time.now
      went_down = false
      yield :reboot if block_given?
      reboot

      loop do
        begin
          current_uptime = with_ssh_opts(
            '-o', 'ConnectTimeout=3',
            '-o', 'ServerAliveInterval=3',
            '-o', 'ServerAliveCountMax=1',
          ) { get_uptime }

          if current_uptime < initial_uptime
            yield :is_up if block_given?
            return Time.now - t
          end
        rescue CommandFailed
          if went_down
            yield :is_down if block_given?
          else
            yield :went_down if block_given?
            went_down = true
          end
        end

        if timeout && (t + timeout) < Time.now
          yield :timeout if block_given?
          fail 'timeout'
        end

        sleep(0.3)
      end
    end

    # @return [Integer] uptime in seconds
    def get_uptime
      read_file!('/proc/uptime').strip.split[0].to_f
    end

    # @return [Array<String>]
    def get_timezone
      popen_read!('date +"%Z;%z"').output.strip.split(';')
    end

    # @param path [String]
    # @return [String]
    def read_file!(path)
      popen_read!('cat', path).output
    end

    # @param path [String]
    # @return [String]
    def read_symlink!(path)
      popen_read!('readlink', path).output.strip
    end

    # @return [Process::Status]
    def execute(*command)
      system_exec(*command)
    end

    def popen_read(*command)
      args =
        if deployment.localhost?
          command
        else
          ssh_args + command
        end

      args << {
        err: [:child, :out],
      }

      out = ''

      IO.popen(args, 'r') do |io|
        out = io.read
      end

      CommandResult.new(args, $?.exitstatus, output: out)
    end

    def popen_read!(*command)
      res = popen_read(*command)

      if res.failed?
        raise CommandFailed, res
      end

      res
    end

    protected
    attr_reader :extra_ssh_opts

    # @return [CommandResult]
    def system_exec(*command)
      args =
        if deployment.localhost?
          command
        else
          ssh_args + command
        end

      pid = Process.spawn(*args)
      Process.wait(pid)
      CommandResult.new(args, $?.exitstatus)
    end

    def with_ssh_opts(*opts)
      @extra_ssh_opts = opts
      ret = yield
      @extra_ssh_opts = []
      ret
    end

    def ssh_args
      ['ssh', '-l', 'root'] + extra_ssh_opts + [deployment.target_host]
    end
  end
end
