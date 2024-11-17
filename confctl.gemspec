lib = File.expand_path('lib', __dir__)
$:.unshift(lib) unless $:.include?(lib)
require 'confctl/version'

Gem::Specification.new do |s|
  s.name = 'confctl'

  s.version     = ConfCtl::VERSION
  s.summary     =
    s.description = 'Nix deployment management tool'
  s.authors     = 'Jakub Skokan'
  s.email       = 'jakub.skokan@vpsfree.cz'
  s.files       = `git ls-files -z`.split("\x0")
  s.files      += Dir['man/man?/*.?']
  s.executables = s.files.grep(%r{^bin/}) { |f| File.basename(f) }
  s.license     = 'GPL-3.0-only'

  s.required_ruby_version = '>= 3.1.0'

  s.add_dependency 'curses'
  s.add_dependency 'gli', '~> 2.22.0'
  s.add_dependency 'json'
  s.add_dependency 'md2man'
  s.add_dependency 'rainbow', '~> 3.1.1'
  s.add_dependency 'rake'
  s.add_dependency 'require_all', '~> 2.0.0'
  s.add_dependency 'tty-command', '~> 0.10.1'
  s.add_dependency 'tty-cursor', '~> 0.7.1'
  s.add_dependency 'tty-pager', '~> 0.14.0'
  s.add_dependency 'tty-progressbar', '~> 0.18.2'
  s.add_dependency 'tty-spinner', '~> 0.9.3'
  s.add_dependency 'vpsfree-client', '~> 0.19.0'
end
