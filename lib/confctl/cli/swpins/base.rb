require 'confctl/cli/command'
require 'confctl/cli/swpins/utils'

module ConfCtl::Cli
  class Swpins::Base < Command
    include Swpins::Utils

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
          puts "Reconfiguring deployment #{cn.name}"
          cn.save
        else
          puts "Deployment #{cn.name} needs update"
          cn.specs.each do |name, s|
            puts "  update #{name}" unless s.valid?
          end
        end
      end
    end
  end
end
