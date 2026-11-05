//go:build linux

package linux

import (
	"errors"
	"strings"

	"github.com/llcoolkm/rookery/go/platform"
)

func shellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'" }

// WSLBootCommand renders the reviewed boot.command for root to install in
// /etc/wsl.conf. It carries public startup options only, never credentials.
// The target account is explicit because boot.command executes as root.
func WSLBootCommand(spec platform.ServiceSpec, account, home, searchPath string) (string, error) {
	if _, e := UserUnit(spec); e != nil {
		return "", e
	}
	if account == "" || home == "" || !strings.HasPrefix(home, "/") {
		return "", errors.New("WSL startup requires an explicit target account and home")
	}
	args := []string{"/usr/sbin/runuser", "-u", account, "--", "/usr/bin/env", "HOME=" + home, "PATH=" + searchPath, spec.Executable, "--state-dir", spec.StateDir, "service", "supervise", "--detach"}
	for i, v := range args {
		if strings.ContainsAny(v, "\x00\r\n") {
			return "", errors.New("service paths must not contain newlines")
		}
		args[i] = shellQuote(v)
	}
	return strings.Join(args, " "), nil
}

// MergeWSLConfig changes only boot.command, preserving unrelated settings and
// pre-existing startup commands. Previous is the helper's prior exact command.
func MergeWSLConfig(original, command, previous string) string {
	lines := strings.SplitAfter(original, "\n")
	start, end := -1, -1
	for i, line := range lines {
		v := strings.TrimSpace(line)
		if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
			if start >= 0 {
				end = i
				break
			}
			if strings.EqualFold(v, "[boot]") {
				start = i
			}
		}
	}
	if start < 0 {
		return strings.TrimRight(original, "\n") + "\n\n[boot]\ncommand=" + command + "\n"
	}
	if end < 0 {
		end = len(lines)
	}
	for i := start + 1; i < end; i++ {
		key, value, ok := strings.Cut(lines[i], "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "command") {
			continue
		}
		old := strings.TrimSpace(value)
		if old == command || strings.HasSuffix(old, "; "+command) {
			return original
		}
		if previous != "" && (old == previous || strings.HasSuffix(old, "; "+previous)) {
			old = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(old, previous), "; "))
		}
		if old != "" {
			old += "; "
		}
		lines[i] = "command=" + old + command + "\n"
		return strings.Join(lines, "")
	}
	replacement := []string{"command=" + command + "\n"}
	lines = append(lines[:end], append(replacement, lines[end:]...)...)
	return strings.Join(lines, "")
}
