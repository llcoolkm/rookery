//go:build windows

// Package windows will implement platform.Host using secured AF_UNIX or named
// pipes, peer SID/PID checks, owner-only DACLs, Toolhelp32 process inspection,
// start-time and boot identity pinning, reparse-point refusal, file locks and a
// per-user Scheduled Task. TODO phase 3; x/sys requires a dependency review.
package windows
