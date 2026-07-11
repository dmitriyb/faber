// Package failure implements faber's failure semantics and durability: the
// structured step-result contract shared by every consumer (threading,
// journal, metering, reporting), fail-stop with declared on_failure cleanup
// hooks, opt-in retry with between-attempt cleanup, the append-only run
// journal keyed (step-id, input-hash), and the resume/fresh/interactive
// recovery modes.
//
// The package is mechanism, not policy: hooks are opaque user scripts run
// host-side via os/exec, records carry no domain vocabulary, and every
// decision derives from (journal bytes, deterministic IR). It depends only on
// the config module.
//
// Secrets discipline: this package never receives credential material. Journal
// records carry step ids, input hashes, result payloads, and error text —
// never resolved secret values — and logging emits step ids, attempt numbers,
// and error reasons, never input values or payloads.
package failure
