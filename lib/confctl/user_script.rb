module ConfCtl
  class UserScript
    def self.register
      UserScripts.register(self)
    end

    # @param hooks [Hook]
    def setup_hooks(hooks); end

    # @param app [GLI::App]
    def setup_cli(app); end
  end
end
