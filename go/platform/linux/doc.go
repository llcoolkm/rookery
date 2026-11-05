//go:build linux

// Package linux will implement platform.Host using owner-only Unix sockets,
// SO_PEERCRED, process start time plus boot ID, no-follow files, advisory locks,
// and per-user systemd. WSL detection belongs here. TODO phase 2.
package linux
