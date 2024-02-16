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
      api = get_vpsadmin_client

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

      nixer = ConfCtl::Nixer.new(data)
      update_file('vpsadmin/containers.nix') do |f|
        f.puts(nixer.serialize)
      end
    end

    def vpsadmin_network
      vpsadmin_network_containers
    end

    def vpsadmin_network_containers
      api = get_vpsadmin_client
      networks = api.network.list
      data = {}

      [4, 6].each do |ip_v|
        data["ipv#{ip_v}"] = networks.select { |net| net.ip_version == ip_v }.map do |net|
          { address: net.address, prefix: net.prefix }
        end
      end

      nixer = ConfCtl::Nixer.new(data)
      update_file('vpsadmin/networks/containers.nix') do |f|
        f.puts(nixer.serialize)
      end
    end

    protected

    def get_vpsadmin_client
      return @api if @api

      @api = VpsFree::Client.new

      user = ask('User name: ') { |q| q.default = nil }.to_s
      password = ask('Password: ') do |q|
        q.default = nil
        q.echo = false
      end.to_s

      @api.authenticate(:basic, user:, password:)
      @api
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
