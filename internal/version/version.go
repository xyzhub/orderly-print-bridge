// Package version holds the single agent version constant. It is reported on
// every heartbeat (master-plan task 21) so the manager page can show which
// build a venue is running.
package version

// Version is the bridge build identity sent to Orderly on every heartbeat.
// Bump it in the same commit as any behaviour change the server may need to
// reason about.
const Version = "0.2.0"

// UserAgent is the HTTP User-Agent every agent call carries.
const UserAgent = "orderly-print-bridge/" + Version
