//go:build linux

// Package linux implements platform.Host with private no-follow files, durable
// atomic replacement, flock, SO_PEERCRED-authenticated AF_UNIX connections,
// process start/boot identities, bounded direct native execution, separate Go
// state roots, user systemd integration and a supervised WSL/manual fallback.
package linux
