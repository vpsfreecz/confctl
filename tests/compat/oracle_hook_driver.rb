# Test-only entrypoint for the two original hooks; no runtime monkey patches.
require 'confctl'

log = ConfCtl::Logger.instance
log.open('compat-hook')
begin
  case ARGV.fetch(0)
  when 'rediscover.after-write'
    ConfCtl::Hook.call(:configuration_rediscover)
  when 'deploy.prepare'
    names = ARGV.fetch(1, '').split(',')
    # MachineList#select preserves inventory order; it is not Hash#slice.
    machines = ConfCtl::MachineList.new.select { |host, _m| names.member?(host) }
    ConfCtl::Hook.call(:cluster_deploy, kwargs: {
      machines: machines, generation: 'none', action: nil, opts: {}
    })
  else
    raise "Unknown fixture hook #{ARGV[0]}"
  end
  log.close_and_unlink
rescue StandardError
  warn "\nLog file: #{log.path}"
  raise
end
