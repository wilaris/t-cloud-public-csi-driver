// Package e2e is the live proof asset for the driver.
//
// Scenario files sit behind the "e2e" build tag, so an ordinary build, `go test ./...` and `make
// verify` never execute them. They drive the real block storage and compute APIs, the real instance
// metadata service and the guest kernel. They create, attach, format, mount and delete real cloud
// resources.
//
// Everything that reaches no cloud is untagged instead, so the offline gate compiles, vets and
// tests it: process reaping and the SDK transport binder here, plus the settings, catalogue, report
// and reclaim packages this harness consumes. A separate compile-only step covers the tagged
// scenarios without executing one.
//
// Build the asset with `make e2e-build`. It produces a self-contained binary next to a copy of
// the stamped driver binary under dist/conformance, both carrying the same build identity. Copy
// both onto one approved compute instance and run the binary there as root; the instance needs
// neither a Go toolchain nor a source checkout.
//
// A run requires one declared audience with -profile, prints selected-check progress to standard
// error, writes its readable report to standard output and writes one JSON evidence record to a
// dedicated file. It bounds its own wall clock with -time-budget, because a compiled test binary
// carries no timeout of its own. It holds a separate -teardown-budget so an expired run still
// reclaims what it created. Use -list-checks to see what it checks without reaching the cloud.
//

// A run refuses to start unless the project asserted as approved equals the configured project.
// The instance metadata service must also report // a server that project also knows. See the
// README in this directory for the full walkthrough.
package e2e
