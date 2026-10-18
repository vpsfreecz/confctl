#!@ruby@/bin/ruby
# Switch to a new system configuration and wait for confctl to confirm
# connectivity. If confctl is unable to reach the deployed machine, this
# script will roll back to the previous configuration.

require 'optparse'

class AutoRollback
  def self.run
    ar = new
    ar.run
  end

  def run
    puts 'Deploying with auto-rollback'

    options = parse_options

    current_system = File.readlink('/run/current-system')
    puts "  current system = #{current_system}"
    puts "  new system     = #{options[:toplevel]}"
    puts "  action         = #{options[:action]}"
    puts "  check file     = #{options[:check_file]}"
    puts "  timeout        = #{options[:timeout]} seconds"
    puts

    puts 'Switching to new configuration'
    File.write(options[:check_file], 'switching')

    pid = Process.spawn(
      File.join(options[:toplevel], 'bin/switch-to-configuration'),
      options[:action]
    )

    Process.wait(pid)

    File.write(options[:check_file], 'switched')

    puts 'Switch complete, waiting for confirmation'
    t = Time.now

    loop do
      sleep(0.5)

      if File.read(options[:check_file]).strip == 'confirmed'
        puts 'Configuration confirmed'
        File.unlink(options[:check_file])
        exit
      end

      break if t + options[:timeout] < Time.now
    end

    puts 'Timeout occurred, rolling back'

    pid = Process.spawn(
      File.join(current_system, 'bin/switch-to-configuration'),
      options[:action]
    )

    Process.wait(pid)

    puts 'Rollback complete'
    exit(false)
  end

  protected

  def parse_options
    options = {
      timeout: 60,
      toplevel: nil,
      action: nil,
      check_file: nil
    }

    opt_parser = OptionParser.new do |parser|
      parser.banner = "Usage: #{$0} [options] <toplevel> <action> <check file>"

      parser.on('-t', '--timeout TIMEOUT', Integer, 'Timeout in seconds') do |v|
        options[:timeout] = v
      end

      parser.on('-h', '--help', 'Print help message and exit') do
        puts parser
        exit
      end
    end

    opt_parser.parse!

    if ARGV.length != 3
      warn 'Invalid arguments'
      warn opt_parser
      exit(false)
    end

    options[:toplevel] = ARGV[0]
    options[:action] = ARGV[1]
    options[:check_file] = ARGV[2]

    options
  end
end

AutoRollback.run
