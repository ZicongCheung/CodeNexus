//go:build !windows

package crypto

// newDPAPIKeeper is only meaningful on Windows; elsewhere the OS keystore is
// reached through a different backend.
func newDPAPIKeeper(string) Keeper { return nil }
