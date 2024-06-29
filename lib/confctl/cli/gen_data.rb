require 'highline/import'
require 'vpsfree/client'
require 'confctl/conf_dir'

module ConfCtl::Cli
  class GenData < Command
    def vpsadmin_all
      vpsadmin_containers
      vpsadmin_network
    end

    def vpsadmin_containers
      api = vpsadmin_client

      machines = ConfCtl::MachineList.new
      data = {}

      machines.each_value do |m|
        next if m['container'].nil?

        ct = api.vps.show(
          m['container.id'],
          meta: { includes: 'node__location__environment' }
        )

        ct_fqdn = [
          m['host.name'],
          m['host.location'],
          m['host.domain']
        ].compact.join('.')

        data[ct_fqdn] = {
          node: {
            id: ct.node.id,
            name: ct.node.name,
            location: ct.node.location.domain,
            domain: ct.node.location.environment.domain,
            fqdn: "#{ct.node.domain_name}.#{ct.node.location.environment.domain}"
          }
        }
      end

      update_file('vpsadmin/containers.nix') do |f|
        f.puts(ConfCtl::NixFormat.to_nix(data))
      end
    end

    def vpsadmin_network
      vpsadmin_network_containers
    end

    def vpsadmin_network_containers
      api = vpsadmin_client
      networks = api.network.list
      data = {}

      [4, 6].each do |ip_v|
        data["ipv#{ip_v}"] = networks.select { |net| net.ip_version == ip_v }.map do |net|
          { address: net.address, prefix: net.prefix }
        end
      end

      update_file('vpsadmin/networks/containers.nix') do |f|
        f.puts(ConfCtl::NixFormat.to_nix(data))
      end
    end

    protected

    def vpsadmin_client
      return @vpsadmin_client if @vpsadmin_client

      @vpsadmin_client = VpsFree::Client.new

      user = ask('User name: ') { |q| q.default = nil }.to_s
      password = ask('Password: ') do |q|
        q.default = nil
        q.echo = false
      end.to_s

      @vpsadmin_client.authenticate(:token, user:, password:, lifetime: 'fixed', interval: 60) do |_action, params|
        ret = {}

        params.each do |name, desc|
          ret[name] = read_auth_param(name, desc)
        end

        ret
      end

      @vpsadmin_client
    end

    def read_auth_param(name, p)
      prompt = "#{p[:label] || name}: "

      ask(prompt) do |q|
        q.default = nil
        q.echo = !p[:protected]
      end
    end

    def update_file(relpath, &)
      abs = File.join(data_dir, relpath)
      tmp = "#{abs}.new"

      File.open(tmp, 'w', &)
      File.rename(tmp, abs)
    end

    def data_dir
      @data_dir ||= File.join(ConfCtl::ConfDir.path, 'data')
    end
  end
end
