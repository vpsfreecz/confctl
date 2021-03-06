require 'confctl/cli/command'
require 'confctl/cli/swpins/utils'

module ConfCtl::Cli
  class Swpins::Base < Command
    include Swpins::Utils

    def update
      run_command(Swpins::Core, :update)
      run_command(Swpins::Channel, :update)
      run_command(Swpins::Cluster, :update)
    end

    def reconfigure
      each_channel('*') do |chan|
        if chan.valid?
          puts "Reconfiguring channel #{chan.name}"
          chan.save
        else
          puts "Channel #{chan.name} needs update"
          chan.specs.each do |name, s|
            puts "  update #{name}" unless s.valid?
          end
        end
      end

      each_cluster_name('*') do |cn|
        if cn.valid?
          puts "Reconfiguring machine #{cn.name}"
          cn.save
        else
          puts "Machine #{cn.name} needs update"
          cn.specs.each do |name, s|
            puts "  update #{name}" unless s.valid?
          end
        end
      end
    end
  end
end
