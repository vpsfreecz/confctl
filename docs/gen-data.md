# vpsAdmin data generation

`confctl-rust gen-data vpsadmin containers`, `network` and `all` authenticate
with vpsAdmin and replace generated Nix data. They ask for a user name and a
protected password, followed by any described authentication challenges. Password
spaces are preserved. There is no additional confirmation. Tokens stay in memory;
an expired token repeats authentication with the retained credentials and any
new challenge. No background refresh or token revocation runs.

Containers use the configured Nix inventory after authentication. Only absent or
null container metadata is skipped; unmanaged, carried and nonrunnable machines
are included. Each entry requests its literal VPS ID with node, location and
environment includes. Resolved associations are read directly; unresolved ones
use their described show action and path parameters. Required missing associations
fail. Keys join non-null host name, location and domain fields with dots, retaining
empty strings. Duplicate keys use the last inventory entry.

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

The private HTTP client uses `https://api.vpsfree.cz`, normal proxy configuration,
TLS verification and the `haveapi-client` User-Agent. It follows the API's described
methods, namespaces, action aliases and token header. Described URLs must stay
within that origin and base. Redirects, URL user information, escaping paths and
unresolved arguments are refused before a credential-bearing request. Failed
requests are not replayed. Protocol major 2 is required; a minor mismatch warns.
HTTP success with invalid API status or response shape fails rather than producing
empty data. Diagnostics redact known credentials, challenge values and tokens;
request bodies and authentication responses are never logged.

Scalar challenge conversion, nullability, omission and presence checks follow
the pinned client contract. Other validators run on the server, so a value Ruby
would reject locally may instead receive an API validation error. Unsupported
input/output layouts and blocking data actions fail explicitly. This client
supports the data-generation subset, without action-state polling.

The private renderer sorts attribute keys, keeps list order and uses two-space
indentation with a final newline. Ordinary output matches the Ruby fixture bytes.
It escapes Nix keys, quotes and interpolation and preserves Unicode as literal
text; unusual strings can therefore differ from Ruby's string-dump output.
NUL values or keys fail before replacement.
Saved formats remain loadable by Nix and the preceding Ruby implementation.
The public executable selects Rust.
No host, Git, profile, generation or configuration-input operation is performed.
