# Experimental executable extension SDK

The module is `github.com/vpsfreecz/confctl`; import
`github.com/vpsfreecz/confctl/extension`. Its source lives at the repository root.
The SDK remains experimental; no versioned release is published. It uses only
the Go standard library and exposes no `internal/core` types. Go module versions
are independent of the unchanged protocol version 1.0.

The core starts the registry's literal executable argv in the configuration
root. FD3 receives newline-delimited JSON-RPC 2.0 and FD4 sends replies and
reverse service requests. Stdin/stdout/stderr remain UI streams. Both sides use
string request IDs and may issue concurrent requests; duplicate IDs, malformed
frames, unexpected responses and incompatible major versions fail the
invocation. JSON numbers in arbitrary metadata retain their exact integer token
and decimal representation through `json.Number`.

The core sends `initialize` with protocol `{major:1,minor:0}`, extension ID and
available service names. The extension declares required services. One `run`
then names the handler and supplies `Invocation`: root, command/event, options,
arguments, selected names and action. Commands get parsed attrs/tags/yes;
`deploy.prepare` gets an effective `switch` action, whereas the old fixture
hook's `opts[:action]` is absent. This intentional payload difference is separate
from output/state parity. Unknown handlers and required services fail.

```go
package main

import (
    "context"
    ext "github.com/vpsfreecz/confctl/extension"
)

func main() {
    ext.Serve(map[string]ext.Handler{
        "example": func(ctx context.Context, c *ext.Client, in ext.Invocation) error {
            s, err := c.Select(ctx, ext.SelectionFrom(in))
            if err != nil { return err }
            results, err := c.RunMany(ctx, s, []string{"uname", "-r"})
            if err != nil { return err }
            for i, r := range results {
                if r.Error != nil { return r.Error }
                if err := c.Write(ctx, "stdout", s.Machines[i].Name+": "+string(r.Result.Stdout), "plain"); err != nil { return err }
            }
            return nil
        },
    }, []string{"machines.select", "exec.run", "ui.write"})
}
```

Services are `settings.get`, `machines.select`, `exec.run`, `ui.write`,
`ui.machines`, `ui.table`, `ui.confirm`, and `format.nix`. Selection returns an
ordered immutable snapshot ID; `exec.run` accepts only a name from that snapshot
and a nonempty literal argv. OpenSSH receives shell-escaped remote words through
its normal command argument. Generic execution uses the object's own target;
builtin status has its separate carrier-routing rule. `RunMany` uses
machines.length concurrency and preserves result order. Each local `Item` also
records `Completion`, a monotonically increasing worker completion index for
handlers that need completion order. It is not a protocol field.

Wire `stdin_b64` absent/null means an empty pipe kept open until the command
exits. A present empty string means explicit empty input followed by EOF. The
minimal `Client.Exec` uses the former; callers can use the wire method for
explicit input. Output modes are capture/stream; no timeout option is accepted
in parity mode. Result bytes are base64, with exit status, signal and rendered
command. Nonzero host results remain results; cancellation returns RPC code
-32800 and is an invocation failure.

Settings/inventory access is serialized within an invocation. The supervisor
binds reverse services to its context, tracks incoming handlers as well as
outgoing calls, rejects a run that completes with active reverse work, seals new
services at completion, and waits for canceled services before the next hook.
Explicit cancellation uses the approved TERM/two-second/KILL/wait lifecycle.
An executable can still perform its own filesystem effects: this is a trusted
extension boundary, not a security sandbox or rollback transaction.

The static registry schema is version 1. Entries declare IDs, protocol, argv,
command paths/options and the two supported hook events. Command help and
parsing are static: only the packaged `runtime-kernels update` command shape,
descriptions, option sets and optional machine-pattern argument are accepted.
Unsupported shapes and unknown registry fields fail before effects; a
hook-only registry adds no command to help. There is no discovery
daemon, dynamic Ruby loading, persistent worker, new operation lock or automatic
retry. Existing site JSON/Nix files keep their old paths and save behavior.
