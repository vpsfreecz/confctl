# Inputs update references

These 39 cases cover root, channel and machine update, help, retry/warning
failures, lock writes and optional fake Git commits. Fixtures and expected JSON
are verbatim copies of accepted original Ruby captures. `provenance.json` links
literal fixture, raw and normalized hashes, source and executable identities,
acceptance and the existing normalization policy. The fixed
`../expected/<ID>.json` names resolve directly to the canonical sibling records.

Pass `--cases 'tests/compat/stage3-i2/fixtures/inputs-update-*.json'` to `compat`
with the explicit installed fixture candidate, tools and fresh evidence path.
The retained 41, Stage 2 configuration and I1 selectors/references are unchanged.
Expected CLI failures retain exact log/error and post-request file contracts.
Candidate acceptance is separate from these original observations.

Finite fake Git requests characterize argv, inherited pipe/PTY output and the
stdout-only editor flag. They do not establish real Git index/hooks/editor
behavior, terminal stop/resume or foreground restoration parity. Those require
the separate disposable real-Git/PTY gate; no network update or production write
is part of this corpus.
