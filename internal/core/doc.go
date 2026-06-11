// Package core defines VichuFlow's domain types: the on-disk contract for a run.
//
// Every type here is serialized to flat files under .vichu/runs/<run-id>/ and
// forms the public runtime format that any external tool can read. The runtime
// is the single source of truth; CLI, TUI and web are only views over it.
//
// core has no dependencies on other internal packages — it is the leaf of the
// dependency graph.
package core

// SchemaVersion is the version of the on-disk runtime format. It is written
// into state.json so future binaries can detect and migrate older runs.
const SchemaVersion = 1
