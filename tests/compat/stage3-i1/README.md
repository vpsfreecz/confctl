# Read-only inputs references

These 15 cases cover `inputs ls`, `inputs channel ls` and their help using the
pinned original Ruby CLI. `provenance.json` records acceptance, oracle/controller/
tool identities and raw, normalized and fixture hashes. Expected JSON is copied
verbatim from the accepted original captures. Fixture files preserve the exact
original bytes, including their historical `UNACCEPTED-<ID>.json` expected names.
Those names are relative symlinks to the accepted canonical `expected/<ID>.json`
records. Provenance records each literal link and resolved reference hash. The
existing harness normalization policy is unchanged.

Pass `--cases 'tests/compat/stage3-i1/fixtures/inputs-*.json'` to `compat` with the
explicit installed candidate, fixture tools and a fresh evidence directory.
This selector includes exactly 15 cases and excludes the reference aliases.
The default 41-case glob and the separate Stage 2 configuration cases stay
unchanged. Four original machine-update observations stopped at an injected
mutation refusal and remain preparatory evidence outside this differential set.
These references establish original behavior; candidate acceptance is separate.
