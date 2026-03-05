# frozen_string_literal: true

$:.unshift(File.expand_path('../lib', __dir__))

RSpec.configure do |config|
  config.disable_monkey_patching!
  config.expect_with :rspec do |expectations|
    expectations.syntax = :expect
  end
end

Dir[File.join(__dir__, 'support', '*.rb')].each { |f| require f }
