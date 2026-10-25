# vpsAdmin data generation

`confctl-rust gen-data vpsadmin containers`, `network` and `all` authenticate
with vpsAdmin and replace generated Nix data. They ask for a user name and a
protected password, followed by TOTP or password-reset challenges when required.
Password spaces are preserved. There is no additional confirmation. Tokens stay
in memory; an expired token repeats authentication with the retained credentials
and any new challenge. No background refresh or token revocation runs.

Containers use the configured Nix inventory after authentication. Only absent or
null container metadata is skipped; unmanaged, carried and nonrunnable machines
are included. Each entry requests its literal VPS ID with node, location and
environment includes. Resolved associations are read directly; unresolved ones
use API 7.0 show actions with the returned path parameters. Missing required
associations fail. Keys join non-null host name, location and domain fields with
dots, retaining empty strings. Duplicate keys use the last inventory entry.

Networks make one list request, preserving source order and duplicates in separate
IPv4 and IPv6 lists. Only numeric versions 4 and 6 are selected. Count metadata
does not trigger pagination. Network-only generation requires no flake or Nix
invocation.

The fixed destinations are `data/vpsadmin/containers.nix` and
`data/vpsadmin/networks/containers.nix`. Their parent directories must already
exist. Each file is fetched and rendered completely before replacement. `all`
publishes containers before requesting networks, so a later failure can leave
only the first file updated. Empty results still publish complete empty data.
Generated file contents are not printed.

One canonical configuration mutation lock covers authentication, observation and
publication. It coordinates cooperating Rust writers; avoid concurrent Ruby or
external writers. Replacement uses an owned unique temporary file, file and
parent fsync, containment checks and existing-mode preservation. It does not
create missing parents, repair symlink paths or provide a two-file transaction.
An error after rename or cancellation can leave the new file in place. Inspect
both files before explicitly rerunning the desired route; there is no automatic
retry, rollback or compensation.

The `vpsadmin-client` crate provides asynchronous API 7.0 requests and typed
responses. The endpoint is `https://api.vpsfree.cz`, with normal environment proxy
handling, TLS verification and the `haveapi-client` User-Agent. Redirects and automatic
retries are disabled. The client confines action paths and literal IDs to the
configured origin and base. Its request timeout is 30 seconds, connection timeout
10 seconds and response limit 16 MiB. Canceling a request stops waiting for it;
it cannot undo an action the server already accepted. Failed requests, including
HTTP 401 and 403, are not replayed or used to trigger fresh login.

Authentication supports the API 7.0 token request, TOTP and password-reset
continuations. Challenge values use protected input. Unknown continuations fail
without sending another action. The client uses a generated API contract. It
does not query OPTIONS for dynamic methods, paths, token headers or arbitrary
challenge fields. A server requiring another contract needs a compatible client
release and an explicit confctl update.

Invalid API status, response shapes or schema field types fail before publication.
Diagnostics redact known credentials, challenge values and tokens; request bodies
and authentication responses are never logged. The adapter does not poll action
state or refresh tokens in the background. Building confctl requires Rust 1.95
or newer; its configured development and CI toolchain remains Rust 1.95.0.

The private renderer sorts attribute keys, keeps list order and uses two-space
indentation with a final newline. Ordinary output matches the Ruby fixture bytes.
It escapes Nix keys, quotes and interpolation and preserves Unicode as literal
text; unusual strings can therefore differ from Ruby's string-dump output.
NUL values or keys fail before replacement.
Saved formats remain loadable by Nix and the preceding Ruby implementation.
The public executable selects Rust.
No host, Git, profile, generation or configuration-input operation is performed.
