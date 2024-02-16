require 'etc'

module ConfCtl
  class MachineControl
    # @return [Machine]
    attr_reader :machine

    # @param machine [Machine]
    def initialize(machine)
      @machine = machine
      @extra_ssh_opts = []
      @cmd = SystemCommand.new
    end

    # Try to open SSH connection
    # @raise [TTY::Command::ExitError]
    def test_connection
      with_ssh_opts(
        '-o', 'ConnectTimeout=3',
        '-o', 'ServerAliveInterval=3',
        '-o', 'ServerAliveCountMax=1'
      ) { get_uptime }
    end

    def interactive_shell
      if machine.localhost?
        Kernel.system(Etc.getpwuid(0).shell)
      else
        Kernel.system(*ssh_args)
      end
    end

    # Reboot the machine
    def reboot
      execute!('reboot')
    end

    # Reboot the machine and wait for it to come back online
    # @param timeout [Integer, nil]
    # @yieldparam [:reboot, :went_down, :is_down, :is_up, :timeout] state
    # @return [Integer] seconds it took to reboot the machine
    def reboot_and_wait(timeout: nil)
      initial_uptime = get_uptime
      t = Time.now
      went_down = false
      reboot
      yield :reboot, timeout if block_given?

      loop do
        state = nil

        begin
          current_uptime = with_ssh_opts(
            '-o', 'ConnectTimeout=3',
            '-o', 'ServerAliveInterval=3',
            '-o', 'ServerAliveCountMax=1'
          ) { get_uptime }

          if current_uptime < initial_uptime
            yield :is_up, nil
            return Time.now - t
          end
        rescue TTY::Command::ExitError
          if went_down
            state = :is_down
          else
            state = :went_down
            went_down = true
          end
        end

        if timeout
          timeleft = (t + timeout) - Time.now

          raise 'timeout' if timeleft <= 0

          yield state, timeleft if block_given?
        elsif block_given?
          yield state, nil
        end

        sleep(0.3)
      end
    end

    # @return [Integer] uptime in seconds
    def get_uptime
      read_file('/proc/uptime').strip.split[0].to_f
    end

    # @return [Array<String>]
    def get_timezone
      out, = run_cmd('date', '+%Z;%z')
      out.strip.split(';')
    end

    # @param path [String]
    # @return [String]
    def read_file(path)
      out, = run_cmd('cat', path)
      out
    end

    # @param path [String]
    # @return [String]
    def read_symlink(path)
      out, = run_cmd('readlink', path)
      out.strip
    end

    # Execute command, raises exception on error
    # @yieldparam out [String]
    # @yieldparam err [String]
    # @raise [TTY::Command::ExitError]
    # @return [TTY::Command::Result]
    def execute(...)
      run_cmd(...)
    end

    # Execute command, no exception raised on error
    # @yieldparam out [String]
    # @yieldparam err [String]
    # @return [TTY::Command::Result]
    def execute!(...)
      run_cmd!(...)
    end

    # @param script [String]
    # @return [TTY::Command::Result]
    def bash_script(script)
      run_cmd('bash', '--norc', input: script)
    end

    protected

    attr_reader :cmd, :extra_ssh_opts

    def run_cmd(...)
      do_run_cmd(:run, ...)
    end

    def run_cmd!(...)
      do_run_cmd(:run!, ...)
    end

    # rubocop:disable Style/ArgumentsForwarding

    def do_run_cmd(method, *command, **kwargs, &)
      args =
        if machine.localhost?
          command
        else
          ssh_args + command
        end

      cmd.method(method).call(*args, **kwargs, &)
    end

    # rubocop:enable Style/ArgumentsForwarding

    def with_ssh_opts(*opts)
      @extra_ssh_opts = opts
      ret = yield
      @extra_ssh_opts = []
      ret
    end

    def ssh_args
      ['ssh', '-l', 'root'] + extra_ssh_opts + [machine.target_host]
    end
  end
end
