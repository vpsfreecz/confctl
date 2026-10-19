# Original-library qualification only. This does not emulate an installed
# helper's whole process or replace any immutable baseline source/capture.
require 'json'
require 'net/http'

raise 'unexpected Ruby' unless RUBY_VERSION == '3.4.9'
raise 'unexpected JSON' unless JSON::VERSION == '2.21.2'
raise 'unexpected Net::HTTP' unless Net::HTTP::VERSION == '0.6.0'

url, mode = ARGV
uri = URI.parse(url)
raise 'qualification is loopback HTTP only' unless uri.scheme == 'http' && %w[127.0.0.1 localhost].include?(uri.host)

decoded = +''.b
result = { 'scope' => 'original Net::HTTP boundary only', 'ruby' => RUBY_VERSION,
           'json' => JSON::VERSION, 'net_http' => Net::HTTP::VERSION }
code = 0
begin
  if mode == 'index'
    response = Net::HTTP.get_response(uri)
    decoded << response.body
    JSON.parse(decoded)
  elsif mode == 'body'
    Net::HTTP.start(uri.host, uri.port) do |http|
      http.request_get(uri.request_uri) do |response|
        response.read_body { |chunk| decoded << chunk }
      end
    end
  else
    raise 'unknown qualification mode'
  end
  result['success'] = true
rescue StandardError => e
  code = 1
  result['success'] = false
  result['error_class'] = e.class.name
  result['error_primary'] = e.message
  result['raw_backtrace'] = e.backtrace
  if e.is_a?(JSON::ParserError)
    result['original_helper_stderr'] = "ERROR: Could not parse JSON: #{e}\n"
  end
end
result['decoded_bytes'] = decoded.bytes
puts JSON.pretty_generate(result)
exit code
