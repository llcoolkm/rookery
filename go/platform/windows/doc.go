//go:build windows

// Package windows implements native process, private filesystem, pipe, AF_UNIX,
// execution and per-user Scheduled Task facilities. golang.org/x/sys/windows
// supplies typed Win32 bindings and structures missing from the standard library;
// no shell, cgo, external executable or other runtime dependency is required.
// Native Windows acceptance is required; cross-compilation is not runtime proof.
package windows
