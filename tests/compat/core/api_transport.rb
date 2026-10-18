# Test-only transport injection: retain the real locked HaveAPI client behavior.
require 'haveapi/client'
require 'json'
require 'socket'
require 'uri'

module ConfctlCompatApiTransport
  URL = URI(ENV.fetch('CONFCTL_COMPAT_API_URL'))
  unless URL.scheme == 'http' && URL.host == '127.0.0.1' && URL.port > 0 &&
         URL.userinfo.nil? && URL.path.empty?
    raise SecurityError, 'compat API origin must be explicit IPv4 loopback'
  end

  def self.check_address(host, port)
    return if host == URL.host && port.to_i == URL.port

    raise SecurityError, 'compat API blocked non-fixture socket'
  end

  module Communicator
    def initialize(url, *, **opts)
      raise SecurityError, 'unexpected original vpsFree API endpoint' unless url == 'https://api.vpsfree.cz'

      File.write(
        File.join(ENV.fetch('CONFCTL_COMPAT_ROOT'), 'api-origin.json'),
        JSON.generate({ original_url: url, verify_ssl: opts.fetch(:verify_ssl, true) })
      )
      super(ConfctlCompatApiTransport::URL.to_s, *, **opts)
    end
  end

  module Request
    def execute(args, &)
      url = URI(args.fetch(:url))
      unless url.scheme == 'http' && url.host == URL.host &&
             url.port == URL.port && url.userinfo.nil?
        raise SecurityError, 'compat API blocked non-fixture HTTP request'
      end

      super
    end
  end

  module SocketClass
    def tcp(host, port, *, **, &)
      ConfctlCompatApiTransport.check_address(host, port)
      super
    end
  end

  module TCPSocketClass
    def new(host, port, *)
      ConfctlCompatApiTransport.check_address(host, port)
      super
    end

    def open(host, port, *, &)
      ConfctlCompatApiTransport.check_address(host, port)
      super
    end
  end

  module SocketInstance
    def connect(address)
      port, host = Socket.unpack_sockaddr_in(address)
      ConfctlCompatApiTransport.check_address(host, port)
      super
    end

    def connect_nonblock(address, **)
      port, host = Socket.unpack_sockaddr_in(address)
      ConfctlCompatApiTransport.check_address(host, port)
      super
    end
  end
end

HaveAPI::Client::Communicator.prepend(ConfctlCompatApiTransport::Communicator)
RestClient::Request.singleton_class.prepend(ConfctlCompatApiTransport::Request)
Socket.singleton_class.prepend(ConfctlCompatApiTransport::SocketClass)
TCPSocket.singleton_class.prepend(ConfctlCompatApiTransport::TCPSocketClass)
Socket.prepend(ConfctlCompatApiTransport::SocketInstance)

case ENV.fetch('CONFCTL_COMPAT_API_GUARD_PROBE', nil)
when 'http'
  RestClient::Request.execute(method: :get, url: 'https://api.vpsfree.cz/')
when 'socket'
  TCPSocket.new('192.0.2.1', 80)
end
