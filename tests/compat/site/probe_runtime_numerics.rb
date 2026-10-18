require 'json'
require 'digest'
raise 'unexpected Ruby' unless RUBY_VERSION == '3.4.9'
raise 'unexpected JSON' unless JSON::VERSION == '2.21.2'

mask = (1 << 64) - 1
bits = [0, 1, 2, 3, 7, 0x000ffffffffffffe, 0x000fffffffffffff,
        0x0010000000000000, 0x0010000000000001,
        0x3fefffffffffffff, 0x3ff0000000000000, 0x3ff0000000000001,
        0x7feffffffffffffe, 0x7fefffffffffffff]
# Adjacent binary64 values around decimal emission/precision thresholds.
[-324, -323, -308, -100, -10, -9, -8, -7, -6, -5, -4, -3, -1, 0, 1, 9, 10, 14, 15, 16, 17, 18, 20, 22, 23, 100, 307, 308].each do |exp|
  value = Float("1e#{exp}")
  next unless value.finite?

  center = [value].pack('G').unpack1('Q>')
  (-2..2).each { |delta| bits << (center + delta) if center + delta >= 0 }
end
[0, 1, 2, 10, 511, 512, 1022, 1023, 1024, 1535, 2045, 2046].each do |exp|
  [0, 1, 0x5555555555555, 0xaaaaaaaaaaaaa, 0xfffffffffffff].each do |frac|
    bits << ((exp << 52) | frac)
  end
end
state = 0x243f6a8885a308d3
1024.times do
  state = ((state * 6_364_136_223_846_793_005) + 1_442_695_040_888_963_407) & mask
  bits << state
end
bits += bits.map { |value| value ^ (1 << 63) }
rows = bits.uniq.filter_map do |value|
  float = [value].pack('Q>').unpack1('G')
  next unless float.finite?

  { bits: format('%016x', value), pretty: JSON.pretty_generate(float) }
end
ports = [nil, true, false, [], {}, 22, 22.9, -22.9, 184_467_440_737_095_516_160_001,
         -184_467_440_737_095_516_160_001, '', '  +0022rest', '-000', '0x16', '2_2', '1__2',
         '_22', '22_', '2_2_2', ' +2_2x', '1_0.9', "\v22", "\00022", '9999999999999999999999999999999999999999999999999999']
port_rows = ports.map do |value|
  { value: value, converted: value.to_i.to_s }
rescue StandardError => e
  { value: value, error_class: e.class.name }
end
json_inputs = ['1e400', '-1e400', '1e-400', '-1e-400', '-0', '0e0',
               '{"unselected":{"z":184467440737095516160001,"a":-184467440737095516160001,"empty":[[],{}],"f":[1e15,-0.0,5e-324]}}']
json_rows = json_inputs.map do |input|
  value = JSON.parse(input)
  { input: input, pretty: JSON.pretty_generate(value) }
rescue StandardError => e
  { input: input, error_class: e.class.name }
end
nesting = [99, 100, 101, 127, 128].map do |n|
  input = "#{'[' * n}0#{']' * n}"
  begin
    value = JSON.parse(input)
    { depth: n, pretty: JSON.pretty_generate(value) }
  rescue StandardError => e
    { depth: n, error_class: e.class.name }
  end
end
puts JSON.pretty_generate(
  {
    ruby: RUBY_VERSION, json: JSON::VERSION,
    float_cases: rows, port_cases: port_rows, json_cases: json_rows, nesting: nesting
  }
)
