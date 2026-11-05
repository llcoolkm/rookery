//go:build linux

package linux

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/llcoolkm/rookery/go/platform"
)

func testWSLService(t *testing.T) (*service, string, string, platform.ServiceSpec) {
	t.Helper()
	host, root := testHost(t)
	svc := host.Service.(*service)
	svc.backend = "wsl"
	targetRoot := t.TempDir()
	if e := os.Chmod(targetRoot, 0700); e != nil {
		t.Fatal(e)
	}
	svc.wslConfigPath = filepath.Join(targetRoot, "wsl.conf")
	executable := filepath.Join(targetRoot, "program")
	if e := os.WriteFile(executable, []byte{0x7f, 'E', 'L', 'F'}, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(executable, 0700); e != nil {
		t.Fatal(e)
	}
	svc.lookupAccount = func(name string) (*user.User, error) {
		return &user.User{Username: "example", Uid: strconv.Itoa(os.Getuid()), Gid: strconv.Itoa(os.Getgid()), HomeDir: root}, nil
	}
	if fd, _, e := svc.wslDirectory(false); e != nil {
		t.Fatal("isolated WSL directory verification failed")
	} else {
		syscall.Close(fd)
	}
	return svc, root, svc.wslConfigPath, platform.ServiceSpec{Executable: executable, StateDir: root, Account: "example", Home: root, SearchPath: "/native/bin"}
}
func TestWSLInstallAndUninstallPreserveUnrelatedCommands(t *testing.T) {
	svc, _, target, spec := testWSLService(t)
	ctx := context.Background()
	original := "[network]\nkey=value\n[boot]\ncommand=first; second\n[interop]\nenabled=true\n"
	if e := os.WriteFile(target, []byte(original), 0600); e != nil {
		t.Fatal(e)
	}
	if e := svc.Install(ctx, spec); e != nil {
		t.Fatal(e)
	}
	installed, e := os.ReadFile(target)
	if e != nil || !strings.Contains(string(installed), "command=first; second; ") {
		t.Fatal("unrelated boot commands lost")
	}
	oldExecutable := spec.Executable
	spec.Executable = filepath.Join(filepath.Dir(target), "replacement")
	if e := os.WriteFile(spec.Executable, []byte{0x7f, 'E', 'L', 'F'}, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(spec.Executable, 0700); e != nil {
		t.Fatal(e)
	}
	if e = svc.Install(ctx, spec); e != nil {
		t.Fatal(e)
	}
	updated, e := os.ReadFile(target)
	if e != nil || strings.Contains(string(updated), shellQuote(oldExecutable)) || !strings.Contains(string(updated), shellQuote(spec.Executable)) {
		t.Fatal("upgrade did not replace exact prior command")
	}
	backup, e := os.ReadFile(target + ".rookery-go.bak")
	if e != nil || string(backup) != original {
		t.Fatal("upgrade overwrote original backup")
	}
	if e = svc.Uninstall(ctx); e != nil {
		t.Fatal(e)
	}
	removed, e := os.ReadFile(target)
	if e != nil || string(removed) != original {
		t.Fatal("uninstall changed unrelated configuration")
	}
	status, e := svc.Status(ctx)
	if e != nil || status.Installed {
		t.Fatal("removed integration reported installed")
	}
}
func TestWSLInstallRefusesUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"target-symlink", "target-hardlink", "target-writable", "backup-symlink", "lock-symlink"} {
		t.Run(kind, func(t *testing.T) {
			svc, _, target, spec := testWSLService(t)
			other := filepath.Join(filepath.Dir(target), "other")
			if e := os.WriteFile(other, []byte("preserve"), 0600); e != nil {
				t.Fatal(e)
			}
			var e error
			switch kind {
			case "target-symlink":
				e = os.Symlink(other, target)
			case "target-hardlink":
				e = os.Link(other, target)
			case "target-writable":
				e = os.WriteFile(target, []byte("original"), 0666)
				if e == nil {
					e = os.Chmod(target, 0666)
				}
			case "backup-symlink":
				e = os.Symlink(other, target+".rookery-go.bak")
			case "lock-symlink":
				e = os.Symlink(other, filepath.Join(filepath.Dir(target), ".rookery-go-wsl.lock"))
			}
			if e != nil {
				t.Fatal(e)
			}
			if e = svc.Install(context.Background(), spec); e == nil {
				t.Fatal("unsafe WSL integration accepted")
			}
			untouched, e := os.ReadFile(other)
			if e != nil || string(untouched) != "preserve" {
				t.Fatal("unsafe path modified unrelated data")
			}
			status, e := svc.Status(context.Background())
			if e != nil || status.Installed {
				t.Fatal("failed integration reported installed")
			}
		})
	}
}
func TestWSLStatusDetectsRemovedStartup(t *testing.T) {
	svc, _, target, spec := testWSLService(t)
	if e := svc.Install(context.Background(), spec); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(target, []byte("[boot]\ncommand=unrelated\n"), 0600); e != nil {
		t.Fatal(e)
	}
	status, e := svc.Status(context.Background())
	if e != nil || status.Installed || !strings.Contains(status.Detail, "incomplete") {
		t.Fatal("removed boot command remained installed")
	}
}
func TestServiceEnvironmentAndUnitExpansion(t *testing.T) {
	spec := platform.ServiceSpec{Executable: "/native/program", StateDir: "/state/example", Home: "/account/example space%", SearchPath: "/native/$literal:%bin"}
	unit, e := UserUnit(spec)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(unit, "WorkingDirectory=/account/example space%%\n") || !strings.Contains(unit, `Environment="PATH=/native/$literal:%%bin"`) {
		t.Fatal("unit environment escaping incorrect")
	}
	environment := serviceEnvironment(spec)
	home, path := false, false
	for _, entry := range environment {
		if strings.HasPrefix(entry, "HOME=") {
			if home {
				t.Fatal("duplicate HOME")
			}
			home = true
			if entry != "HOME="+spec.Home {
				t.Fatal("wrong HOME")
			}
		}
		if strings.HasPrefix(entry, "PATH=") {
			if path {
				t.Fatal("duplicate PATH")
			}
			path = true
			if entry != "PATH="+spec.SearchPath {
				t.Fatal("wrong PATH")
			}
		}
	}
	if !home || !path {
		t.Fatal("service environment missing target values")
	}
}
func TestWSLAccountValidation(t *testing.T) {
	svc, _, _, spec := testWSLService(t)
	for _, bad := range []platform.ServiceSpec{{Account: "bad\nname"}, {Account: "example", Home: "/different"}, {Account: "example", SearchPath: "/native\nwrong"}} {
		bad.Executable = spec.Executable
		bad.StateDir = spec.StateDir
		if e := svc.Install(context.Background(), bad); e == nil {
			t.Fatal("invalid target service specification accepted")
		}
	}
}

func TestSystemdExistingPublicUnitDirectory(t *testing.T) {
	svc, root, _, spec := testWSLService(t)
	config := t.TempDir()
	if e := os.Chmod(config, 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("XDG_CONFIG_HOME", config)
	unitRoot := filepath.Join(config, "systemd", "user")
	if e := os.MkdirAll(unitRoot, 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(filepath.Join(config, "systemd"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(unitRoot, 0755); e != nil {
		t.Fatal(e)
	}
	svc.backend = "systemd"
	calls := []string{}
	svc.controlCommand = func(_ context.Context, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	if e := svc.Install(context.Background(), spec); e != nil {
		t.Fatal(e)
	}
	info, e := os.Stat(unitRoot)
	if e != nil || info.Mode().Perm() != 0755 {
		t.Fatal("installation changed existing user-unit directory permissions")
	}
	unit, e := os.ReadFile(filepath.Join(unitRoot, UnitName))
	if e != nil || !strings.Contains(string(unit), root) {
		t.Fatal("user unit missing")
	}
	file, e := os.Stat(filepath.Join(unitRoot, UnitName))
	if e != nil || file.Mode().Perm() != 0600 {
		t.Fatal("user unit is not private")
	}
	if strings.Join(calls, ",") != "daemon-reload,enable "+UnitName {
		t.Fatal("systemd integration invoked unexpected control actions")
	}
}
func TestWSLFreshStateHandoffValidation(t *testing.T) {
	svc, root, _, spec := testWSLService(t)
	// Validation refuses an existing configured helper instead of adopting its
	// credentials or in-flight commands for another account.
	if e := os.WriteFile(filepath.Join(root, "config.json"), []byte("private"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := svc.validateHandoff(spec, os.Getuid(), os.Getgid()); e == nil {
		t.Fatal("existing helper state was adopted")
	}
}

func TestServiceExecutableValidation(t *testing.T) {
	svc, _, _, spec := testWSLService(t)
	path := filepath.Join(filepath.Dir(spec.Executable), "script")
	if e := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(path, 0700); e != nil {
		t.Fatal(e)
	}
	spec.Executable = path
	if e := svc.Install(context.Background(), spec); e == nil {
		t.Fatal("implicit interpreter service accepted")
	}
	spec.Executable = "relative"
	if e := svc.Install(context.Background(), spec); e == nil {
		t.Fatal("relative service executable accepted")
	}
}

func TestServicePlanMatchesSelectedBackendAndNeverInstalls(t *testing.T) {
	for _, backend := range []string{"wsl", "systemd", "manual"} {
		t.Run(backend, func(t *testing.T) {
			svc, root, target, spec := testWSLService(t)
			svc.backend = backend
			svc.controlCommand = func(context.Context, ...string) error { t.Fatal("dry-run contacted service manager"); return nil }
			result, e := svc.Plan(context.Background(), spec)
			if e != nil {
				t.Fatal(e)
			}
			plan := result.(map[string]any)
			content, ok := plan["content"].(string)
			if !ok || plan["backend"] != backend || plan["started"] != false {
				t.Fatal("invalid dry-run shape")
			}
			switch backend {
			case "wsl":
				if !strings.Contains(content, "'service' 'supervise' '--detach'") || !strings.Contains(content, "'HOME="+root+"'") {
					t.Fatal("WSL plan omitted supervisor target environment")
				}
			case "systemd":
				if !strings.Contains(content, "[Service]") || !strings.Contains(content, "Environment=") {
					t.Fatal("systemd plan omitted unit or target environment")
				}
			case "manual":
				if !strings.HasSuffix(content, "'daemon'") {
					t.Fatal("manual plan omitted daemon command")
				}
			}
			if _, e = os.Lstat(target); !os.IsNotExist(e) {
				t.Fatal("dry-run wrote WSL configuration")
			}
			if _, e = os.Lstat(filepath.Join(root, "service.json")); !os.IsNotExist(e) {
				t.Fatal("dry-run wrote service state")
			}
		})
	}
}
