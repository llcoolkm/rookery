// Package codex implements provider.Provider over an authenticated control socket.
// Native delivery is idle-only, revalidates the pinned process and expiry, and
// calls thread/queue/add without starting any command on any operating system.
package codex
