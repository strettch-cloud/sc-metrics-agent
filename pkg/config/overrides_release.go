//go:build !e2e

package config

// applyTestOverrides changes nothing: release builds have no way to change the
// fixed settings. See overrides_e2e.go.
func applyTestOverrides(*Config) {}
