//go:build !windows

package preflight

import "context"

// EnsureHyperV is a no-op on non-Windows builds (the agent only ships for
// Windows; this stub keeps the package compiling for local tooling).
func EnsureHyperV(_ context.Context) error { return nil }
