# Protocol kernel provenance

Selected from JaxsonWang/cpa-plugin-oai-basispoints v0.1.18, commit 11df6f8855847ec1957b0d2f4271a9cd9b13cfe1 (MIT; ../../third_party/CPA_LICENSE).

Reused: tool catalogue/instructions, function/custom/namespace translation, history reconstruction, final response validation, incremental text delivery, synthetic terminal/tool frames and selected pure helpers/tests.

Not imported: CPA C ABI, virtual accounts, OAuth discovery/refresh, Service lifecycle, host HTTP callbacks, native execution, marketplace or automatic relay regeneration.

Local changes: trusted tenant/account/conversation + effective-catalog cache namespace; bounded cache bytes; number-preserving clones; custom call IDs; real JSON Schema validation with all external loading disabled; bounded response stream; max_output_tokens is forwarded only after integer validation. Actual upstream support/enforcement still requires live acceptance.

The managed plugin invokes this kernel through its Go converter process. Image upload transport and WebSocket continuation are integrated by the Rust adapter and scoped history cache. Hosted web search is not implemented as a server-side BPS capability. Model context capacity remains an upstream limit; a successful large-body conversion is not a model-window guarantee.


Secondary compatibility reference: ranxi2001/sub2api `production` (inspected 2026-09-27). Its `internal/pkg/apicompat/responses_client_tools.go` supplies the reversible lowering pattern for custom tools, namespace flattening, tool_search proxying, history rewriting, item-ID retyping, and stateful SSE restoration. The local kernel keeps its scoped cache and strict relay validation, and now accepts `tool_search` through the same run_officejs relay boundary.
