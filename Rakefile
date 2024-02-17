require 'bundler/gem_tasks'
require 'confctl'
require 'md2man/rakefile'
require 'md2man/roff/engine'
require 'md2man/html/engine'

# Override markdown engine to add extra parameter
[Md2Man::Roff, Md2Man::HTML].each do |mod|
  mod.send(:remove_const, :ENGINE)
  mod.send(:const_set, :ENGINE, Redcarpet::Markdown.new(mod.const_get(:Engine),
                                                        tables: true,
                                                        autolink: true,
                                                        superscript: true,
                                                        strikethrough: true,
                                                        no_intra_emphasis: false,
                                                        fenced_code_blocks: true,

                                                        # This option is needed for command options to be rendered property
                                                        disable_indented_code_blocks: true))
end

desc 'Generate man/man8/confctl-options.nix.8.md'
task 'confctl-options' do
  ConfCtl::Logger.open('rake', output: $stdout)

  opts = ConfCtl::ModuleOptions.new(nix: ConfCtl::Nix.stateless)
  opts.read

  ConfCtl::ErbTemplate.render_to('confctl-options.nix/main', {
    date: Time.now,
    version: 'master',
    opts:,
    print_options: proc do |opt_list|
      ConfCtl::ErbTemplate.render('confctl-options.nix/options', {
        opts: opt_list,
        indent: proc { |s, n| s.split("\n").join("\n#{' ' * n}") }
      })
    end
  }, 'man/man8/confctl-options.nix.8.md')
end
