require 'spec_helper'
require 'confctl/inputs/github_compare'

RSpec.describe ConfCtl::Inputs::GitHubCompare do
  let(:transport) { class_double(Net::HTTP) }
  let(:http) { instance_double(Net::HTTP) }
  let(:old_rev) { 'a' * 40 }
  let(:new_rev) { 'b' * 40 }
  let(:client) { described_class.new(token: 'private-token', http: transport) }

  def compare
    client.compare(owner: 'example', repo: 'source', old_rev: old_rev, new_rev: new_rev)
  end

  def summary(status = 'ahead')
    {
      'status' => status,
      'base_commit' => { 'sha' => old_rev },
      'merge_base_commit' => { 'sha' => old_rev },
      'ahead_by' => 5,
      'behind_by' => 0,
      'total_commits' => 5,
      'commits' => []
    }
  end

  def response(body: summary.to_json, code: '200')
    allow(transport).to receive(:start).with('api.github.com', 443,
                                             use_ssl: true, open_timeout: 10, read_timeout: 30).and_yield(http)
    allow(http).to receive(:max_retries=).with(0)
    allow(http).to receive(:request).and_return(instance_double(Net::HTTPResponse, code: code, body: body))
  end

  it 'uses exact full SHAs, fixed HTTPS host, explicit token and bounded network settings' do
    response
    expect(http).to receive(:request) do |request|
      expect(request.path).to eq("/repos/example/source/compare/#{old_rev}...#{new_rev}?per_page=1")
      expect(request['Authorization']).to eq('Bearer private-token')
      expect(request['Accept']).to eq('application/vnd.github+json')
      expect(request['User-Agent']).to eq('confctl-forward-guard')
      instance_double(Net::HTTPResponse, code: '200', body: summary.to_json)
    end
    expect(compare).to eq(:ahead)
  end

  it 'decides equality locally' do
    expect(transport).not_to receive(:start)
    expect(client.compare(owner: 'example', repo: 'source', old_rev: old_rev, new_rev: old_rev)).to eq(:identical)
  end

  it 'accepts a large comparison using complete summary metadata despite truncated commit lists' do
    data = summary.merge('ahead_by' => 50_000, 'total_commits' => 50_000,
                         'commits' => [{ 'sha' => new_rev }])
    response(body: data.to_json)
    expect(compare).to eq(:ahead)
  end

  it 'reports behind history' do
    data = summary('behind').merge('ahead_by' => 0, 'behind_by' => 50_000, 'total_commits' => 0,
                                   'merge_base_commit' => { 'sha' => new_rev })
    response(body: data.to_json)
    expect(compare).to eq(:behind)
  end

  it 'reports divergent history, including an equivalent cherry pick or rebase' do
    data = summary('diverged').merge('behind_by' => 1, 'merge_base_commit' => { 'sha' => 'c' * 40 })
    response(body: data.to_json)
    expect(compare).to eq(:diverged)
  end

  {
    'missing fields' => ->(data) { data.delete('behind_by') },
    'wrong base' => ->(data) { data['base_commit']['sha'] = 'd' * 40 },
    'wrong merge base' => ->(data) { data['merge_base_commit']['sha'] = new_rev },
    'abbreviated merge base' => ->(data) { data['merge_base_commit']['sha'] = 'short' },
    'ahead and behind' => ->(data) { data['behind_by'] = 1 },
    'missing ahead count' => ->(data) { data['ahead_by'] = 0 },
    'wrong total' => ->(data) { data['total_commits'] = 6 },
    'negative counts' => ->(data) { data['ahead_by'] = -1 },
    'string counts' => ->(data) { data['behind_by'] = '0' },
    'unknown status' => ->(data) { data['status'] = 'maybe' },
    'false equality' => ->(data) { data['status'] = 'identical' },
    'inconsistent behind status' => ->(data) { data['status'] = 'behind' },
    'inconsistent diverged status' => ->(data) { data['status'] = 'diverged' }
  }.each do |name, mutate|
    it "fails closed on #{name}" do
      data = summary
      instance_exec(data, &mutate)
      response(body: data.to_json)
      expect { compare }.to raise_error(described_class::Error, /ancestry evidence/)
    end
  end

  ['null', '[]', '{bad-json', '{"message":"private-token"}'].each do |body|
    it "rejects invalid response shape #{body[0, 10]} without leaking response data" do
      response(body: body)
      expect { compare }.to raise_error(described_class::Error) { |e| expect(e.message).not_to include('private-token') }
    end
  end

  %w[301 302 401 403 404 409 422 429 500 503].each do |code|
    it "fails closed for HTTP #{code} without redirecting or leaking credentials/body" do
      response(code: code, body: 'private-token')
      expect { compare }.to raise_error(described_class::Error, "GitHub comparison failed (HTTP #{code})")
    end
  end

  [Net::OpenTimeout, Net::ReadTimeout, SocketError, Errno::ECONNRESET, OpenSSL::SSL::SSLError,
   Net::HTTPBadResponse, EOFError].each do |error|
    it "returns sanitized evidence failure for #{error}" do
      allow(transport).to receive(:start).and_raise(error, 'private-token')
      expect { compare }.to raise_error(described_class::Error, 'Unable to obtain GitHub ancestry evidence')
    end
  end

  it 'supports unauthenticated public requests without sending an empty authorization header' do
    response
    client = described_class.new(token: nil, http: transport)
    expect(http).to receive(:request) do |request|
      expect(request['Authorization']).to be_nil
      instance_double(Net::HTTPResponse, code: '200', body: summary.to_json)
    end
    expect(client.compare(owner: 'example', repo: 'source', old_rev: old_rev, new_rev: new_rev)).to eq(:ahead)
  end

  it 'rejects malformed repository coordinates or mutable refs before HTTP' do
    expect(transport).not_to receive(:start)
    expect do
      client.compare(owner: 'example/other', repo: 'source', old_rev: old_rev, new_rev: new_rev)
    end.to raise_error(described_class::Error)
    expect do
      client.compare(owner: 'example', repo: 'source', old_rev: 'main', new_rev: new_rev)
    end.to raise_error(described_class::Error)
  end
end
