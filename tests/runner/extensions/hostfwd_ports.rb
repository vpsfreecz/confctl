# frozen_string_literal: true

require 'osvm'

module ConfctlHostfwdPorts
  PLACEHOLDER = /\bnet\d+\b/
  @ports = {}

  class << self
    def reserve(name)
      key = name.to_s
      @ports[key] ||= OsVm::PortReservation.get_port(key: "hostfwd:#{key}")
    end

    def port(name)
      @ports[name.to_s]
    end

    def ports
      @ports.dup
    end

    def replace_placeholders(str)
      str.gsub(PLACEHOLDER) { |token| reserve(token).to_s }
    end
  end
end

module ConfctlHostfwdUserNetworkPatch
  def qemu_options
    host_forward = @opts['hostForward']
    return super unless host_forward.is_a?(String)

    @opts['hostForward'] = ConfctlHostfwdPorts.replace_placeholders(host_forward)
    super
  ensure
    @opts['hostForward'] = host_forward
  end
end

OsVm::MachineConfig::UserNetwork.prepend(ConfctlHostfwdUserNetworkPatch)
