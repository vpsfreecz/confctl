module ConfCtl
  module UserScripts
    def self.load_scripts
      dir = ConfDir.user_script_dir

      begin
        files = Dir.entries(dir)
      rescue Errno::ENOENT
        return
      end

      files.each do |f|
        abs_path = File.join(dir, f)
        next unless File.file?(abs_path)

        load(abs_path)
      end
    end

    def self.register(klass)
      @scripts ||= []
      @scripts << klass.new
    end

    def self.setup_all
      each do |script|
        script.setup_hooks(Hook)
      end
    end

    # @return [Array<UserScript>]
    def self.get
      (@scripts || [])
    end

    # @yieldparam [UserScript]
    def self.each(&block)
      get.each(&block)
    end
  end
end
