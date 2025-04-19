#!@ruby@/bin/ruby

require 'net/http'
require 'json'
require 'uri'
require 'optparse'
require 'tempfile'

class KexecNetboot
  MACHINE_FQDN = '@machineFqdn@'.freeze

  KEXEC = '@kexecTools@/bin/kexec'.freeze

  def initialize
    @server_url    = nil
    @machine_fqdn  = nil
    @machine_gen   = nil
    @variant_name  = nil
    @interactive   = false
    @append_params = ''
    @exec          = false
    @unload        = false
    @machines_json = nil
    @tmp_files     = []
  end

  def run
    parse_arguments

    if @unload && @exec
      warn 'ERROR: use either --unload or --exec, not both'
      exit 1
    elsif @unload
      return unload_kexec
    elsif @exec
      return exec_kexec
    end

    httproot = parse_httproot_from_cmdline
    unless httproot
      warn "ERROR: Could not find 'httproot=' parameter in /proc/cmdline"
      exit 1
    end

    machines_url =
      if @server_url
        File.join(@server_url, 'machines.json')
      else
        derive_machines_json_url(httproot)
      end

    @machines_json = fetch_machines_json(machines_url)

    machine_data = pick_machine(@machine_fqdn)

    if machine_data.nil?
      warn 'ERROR: No suitable machine found.'
      exit 1
    end

    generation = pick_generation(machine_data, @machine_gen)

    if generation.nil?
      warn 'ERROR: No generation found/selected.'
      exit 1
    end

    variant = pick_variant(generation, @variant_name)

    combined_params = generation['kernel_params'].dup

    if variant && variant['kernel_params']
      combined_params.concat(variant['kernel_params'])
    end

    combined_params << @append_params
    final_params = combined_params.join(' ')

    if @interactive
      puts
      puts 'Selected configuration:'
      puts "  Machine:       #{machine_data['fqdn']} (spin=#{machine_data['spin']})"
      puts "  Generation:    #{generation['generation']} (#{generation['time_s']}, rev=#{generation['shortrev']}, kernel=#{generation['kernel_version']})"
      if variant
        puts "  Variant:       #{variant['name']} (#{variant['label']})"
      else
        puts '  Variant:       none'
      end
      puts "  Kernel params: #{final_params}"
      puts

      loop do
        print 'Continue? [y/N]: '

        case $stdin.readline.strip.downcase
        when 'y'
          puts
          break
        when 'n'
          warn 'Aborting'
          exit(false)
        end
      end
    end

    # Download kernel + initrd
    kernel_path = download_file(generation['boot_files']['bzImage'])
    initrd_path = download_file(generation['boot_files']['initrd'])

    # kexec -l
    load_kexec(kernel_path, initrd_path, final_params)

    # Cleanup
    cleanup_downloads
  end

  private

  def parse_arguments
    opt_parser = OptionParser.new do |opts|
      opts.banner = "Usage: #{$0} [options]"

      opts.on('-s', '--server-url URL', 'Specify URL of the netboot server') do |val|
        @server_url = val
      end

      opts.on('-m', '--machine FQDN', 'Select machine by FQDN') do |val|
        @machine_fqdn = val
      end

      opts.on('-g', '--generation N', 'Select generation by number or negative offset') do |val|
        @machine_gen = val
      end

      opts.on('-v', '--variant NAME', 'Select a specific variant by name') do |val|
        @variant_name = val
      end

      opts.on('-i', '--interactive', 'Enable interactive mode') do
        @interactive = true
      end

      opts.on('-a', '--append PARAMS', 'Append parameters to kernel command line') do |val|
        @append_params = val
      end

      opts.on('-e', '--exec', 'Run the currently loaded kernel') do
        @exec = true
      end

      opts.on('-u', '--unload', 'Unload the current kexec target kernel and exit') do
        @unload = true
      end
    end

    opt_parser.parse!(ARGV)
  end

  def parse_httproot_from_cmdline
    cmdline = File.read('/proc/cmdline').strip
    cmdline[/\bhttproot=([^\s]+)/, 1]
  end

  def derive_machines_json_url(httproot)
    uri = URI.parse(httproot)
    path_parts = uri.path.split('/').reject(&:empty?)

    if path_parts.size >= 3
      3.times { path_parts.pop }
    end

    new_path = "/#{path_parts.join('/')}"
    new_path << '/' unless new_path.end_with?('/')
    new_path << 'machines.json'

    URI::HTTP.build(host: uri.host, port: uri.port, path: new_path).to_s
  end

  def fetch_machines_json(machines_url)
    uri = URI.parse(machines_url)
    response = Net::HTTP.get_response(uri)

    unless response.is_a?(Net::HTTPSuccess)
      warn "ERROR: Could not download #{machines_url}, HTTP #{response.code}"
      exit 1
    end

    begin
      JSON.parse(response.body)
    rescue JSON::ParserError => e
      warn "ERROR: Could not parse JSON: #{e}"
      exit 1
    end
  end

  def pick_machine(requested_fqdn)
    machines = @machines_json['machines']
    return if machines.nil? || machines.empty?

    if requested_fqdn.nil? && @interactive
      return interactive_pick_machine(machines)
    end

    requested_fqdn ||= MACHINE_FQDN

    found = machines.detect { |m| m['fqdn'] == requested_fqdn }

    if found.nil?
      warn "Machine '#{requested_fqdn}' not found."
      return
    end

    found
  end

  def interactive_pick_machine(machines)
    format_str = "%5s  %-30s  %s\n"
    default_machine = machines.detect { |m| m['fqdn'] == MACHINE_FQDN }

    loop do
      puts format(format_str, '', 'FQDN', 'SPIN')

      machines.each_with_index do |m, idx|
        current_mark = m['fqdn'] == default_machine['fqdn'] ? '*' : ''
        puts format(format_str, "[#{idx + 1}]" + current_mark, m['fqdn'], m['spin'])
      end

      print 'Select a machine by number: '
      input = $stdin.gets
      return if input.nil?

      idx = input.strip.to_i

      if idx == 0 && default_machine
        return default_machine
      elsif idx.between?(1, machines.size)
        return machines[idx - 1]
      end

      puts 'Invalid selection. Please try again.'
    end
  end

  def pick_generation(machine_data, generation_input)
    gens = machine_data['generations'] || []
    return if gens.empty?

    unless generation_input.nil?
      parsed = parse_generation_input(generation_input, gens)

      if parsed.nil?
        warn "Requested generation '#{generation_input}' not found (or invalid)."
        return
      end

      return parsed
    end

    if @interactive
      loop do
        current = gens.find { |x| x['current'] == true }
        default_label = if current
                          "(default is #{current['generation']} - current)"
                        else
                          "(no current labeled, default is newest: #{gens[0]['generation']})"
                        end

        puts "Available generations for #{machine_data['fqdn']}: #{default_label}"
        list_generations(gens)
        print 'Enter generation number (or negative offset) [ENTER for default]: '
        input = $stdin.gets
        return if input.nil?

        line = input.strip
        return current || gens[0] if line == ''

        parsed = parse_generation_input(line, gens)
        return parsed if parsed

        puts "Invalid generation '#{line}'. Please try again."
      end
    else
      current = gens.find { |x| x['current'] == true }
      current || gens[0]
    end
  end

  def list_generations(gens)
    format_str = "%5s  %-19s  %-10s  %s\n"

    puts format(format_str, '', 'TIME', 'REVISION', 'KERNEL')

    gens.each do |g|
      current_mark = g['current'] ? '*' : ''

      line = format(
        format_str,
        "[#{g['generation']}]" + current_mark,
        g['time_s'],
        g['shortrev'],
        g['kernel_version']
      )
      puts line
    end
  end

  def parse_generation_input(input_str, gens)
    begin
      val = Integer(input_str)
    rescue ArgumentError
      return
    end

    if val >= 0
      gens.find { |g| g['generation'] == val }
    else
      offset = -val
      return if offset >= gens.size

      gens[offset]
    end
  end

  def pick_variant(generation_data, desired_variant_name)
    variants = generation_data['variants'] || []
    return if variants.empty?

    if desired_variant_name.nil? && @interactive
      interactive_pick_variant(generation_data)
    elsif desired_variant_name
      v = variants.find { |x| x['name'] == desired_variant_name }

      if v.nil?
        warn "Requested variant '#{desired_variant_name}' not found in generation #{generation_data['generation']}."
        return
      end

      v
    else
      variants.first
    end
  end

  def interactive_pick_variant(generation_data)
    variants = generation_data['variants']

    loop do
      puts "Variants available for generation #{generation_data['generation']}:"
      format_str = "%5s  %s\n"

      puts format(format_str, '', 'LABEL')

      variants.each_with_index do |v, idx|
        line = format(format_str, "[#{idx + 1}]", v['label'])
        puts line
      end

      print "Choose variant by number [ENTER for '#{variants[0]['name']}']: "

      input = $stdin.gets
      return if input.nil?

      line = input.strip
      return variants[0] if line == ''

      idx = line.to_i
      return variants[idx - 1] if idx.between?(1, variants.size)

      puts 'Invalid selection. Please try again.'
    end
  end

  def download_file(url)
    uri = URI.parse(url)
    basename = File.basename(uri.path)
    tmp_file = Tempfile.new(basename)

    puts "Downloading #{url} -> #{tmp_file.path}"

    Net::HTTP.start(uri.host, uri.port) do |http|
      resp = http.get(uri.request_uri)

      unless resp.is_a?(Net::HTTPSuccess)
        warn "ERROR: Could not download #{url}, HTTP #{resp.code}"
        exit 1
      end

      tmp_file.write(resp.body)
    end

    tmp_file.close

    @tmp_files << tmp_file
    tmp_file.path
  end

  def load_kexec(kernel_path, initrd_path, kernel_params_string)
    cmd = [
      KEXEC,
      '-l',
      kernel_path,
      "--initrd=#{initrd_path}",
      "--append=\"#{kernel_params_string}\""
    ].join(' ')

    puts "Executing: #{cmd}"
    system(cmd)

    if $?.exitstatus == 0
      puts 'kexec -l completed successfully.'
    else
      warn 'ERROR: kexec -l failed!'
      exit 1
    end
  end

  def exec_kexec
    cmd = [KEXEC, '-e'].join(' ')

    puts "Executing: #{cmd}"
    system(cmd)

    if $?.exitstatus == 0
      puts 'kexec -e completed successfully.'
    else
      warn 'ERROR: kexec -u failed!'
      exit 1
    end
  end

  def unload_kexec
    cmd = [KEXEC, '-u'].join(' ')

    puts "Executing: #{cmd}"
    system(cmd)

    if $?.exitstatus == 0
      puts 'kexec -u completed successfully.'
    else
      warn 'ERROR: kexec -u failed!'
      exit 1
    end
  end

  def cleanup_downloads
    @tmp_files.each do |f|
      f.unlink
    rescue StandardError => e
      warn "Warning: Could not remove tmp file #{f.path}: #{e}"
    end
  end
end

loader = KexecNetboot.new
loader.run
