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
