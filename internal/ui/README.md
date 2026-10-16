# Comparison paging and prompt input

Basic paging wraps text with `x/ansi.Hardwrap` in grapheme-width mode and keeps
spaces and ANSI sequences. Combining marks, emoji sequences and wide characters
stay intact, including a cluster wider than the viewport. Wrapping can differ
from the original Ruby presentation. Existing line feeds and content order stay
intact; the adapter rejects invalid UTF-8 when wrapping is enabled.

`WrapBasic` keeps arbitrary-precision width arguments at its boundary. Zero width
bypasses wrapping and decoding; negative widths and widths beyond the string's
byte length leave valid text unchanged. It never allocates a viewport-sized
buffer or narrows a huge width to a native integer.

Pagination, q/Q and continuation, error propagation, and the shared confirmation
input reader remain application-owned. SystemPager discovery, its package-bound
shell, child wait and early-close behavior are independent of text wrapping.
The [Linux terminal bindings](../core/terminal.md) own borrowed descriptors,
mode restoration and cancellation wakeups. Authentication uses those primitives
with its own editing and protected-input policy; it does not use a second reader
from a terminal library.

The live operation model copies its compact semantic header and task snapshots,
retains bounded recent output, and lays out safe display columns with `x/ansi`.
It places bordered plain log rows above bounded progress rows, reserving at least
twelve log slots at 80×24. Confctl's finite field/state palette is applied after
sanitation and grapheme clipping; child data cannot supply terminal styles.
Complete machine/input preludes belong to the build caller and never enter the
model, repaint or final summary. Its caller serializes updates;
the model has no reader, worker, terminal handle or execution policy. The Linux
session owns rendering and prompt barriers. See [live operations](../../docs/live-operations.md)
for the command families, progress and controls.
