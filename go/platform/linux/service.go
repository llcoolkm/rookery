//go:build linux

package linux

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/llcoolkm/rookery/go/platform"
)

const UnitName = "rookery-go.service"
const logLimit = 1024 * 1024

type service struct {
	paths   *paths
	backend string
}
type serviceConfig struct {
	Backend string               `json:"backend"`
	Spec    platform.ServiceSpec `json:"spec"`
}

func (s *service) ConfigureBackend(backend string) error {
	if backend != "auto" && backend != "systemd" && backend != "wsl" && backend != "manual" {
		return errors.New("invalid service backend")
	}
	s.backend = backend
	return nil
}
func unitQuote(v string) string {
	v = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%", "$", "$$").Replace(v)
	return "\"" + v + "\""
}

// UserUnit is pure rendering, used by install --dry-run and isolated tests.
func UserUnit(spec platform.ServiceSpec) (string, error) {
	if !filepath.IsAbs(spec.Executable) || !filepath.IsAbs(spec.StateDir) {
		return "", errors.New("service paths must be absolute")
	}
	args := append([]string{spec.Executable, "--state-dir", spec.StateDir}, spec.Arguments...)
	if len(spec.Arguments) == 0 {
		args = append(args, "daemon")
	}
	for i, v := range args {
		if strings.ContainsAny(v, "\r\n\x00") {
			return "", errors.New("service paths must not contain newlines")
		}
		args[i] = unitQuote(v)
	}
	return "[Unit]\nDescription=Rookery Go message helper\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\nExecStart=" + strings.Join(args, " ") + "\nRestart=always\nRestartSec=5\nTimeoutStopSec=60\nKillMode=mixed\nUMask=0077\nNoNewPrivileges=true\n\n[Install]\nWantedBy=default.target\n", nil
}
func (s *service) unitRoot() (string, error) {
	root := os.Getenv("XDG_CONFIG_HOME")
	if root == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return "", errUnsafe
		}
		root = filepath.Join(home, ".config")
	}
	return filepath.Join(root, "systemd", "user"), nil
}
func (s *service) control(ctx context.Context, args ...string) error {
	path, e := ResolveExecutable("systemctl")
	if e != nil {
		return errors.New("systemd user manager unavailable")
	}
	spec := platform.Spec{Path: path, Args: append([]string{"--user"}, args...), Timeout: 60 * time.Second, MaxOutput: 65536}
	result, e := executor{}.Exec(ctx, spec)
	if e != nil || result.State != platform.Completed || result.ExitCode != 0 {
		return errors.New("systemd user operation failed")
	}
	return nil
}
func (s *service) Install(ctx context.Context, spec platform.ServiceSpec) error {
	if spec.StateDir != s.paths.state {
		return errors.New("service configuration belongs to another state directory")
	}
	text, e := UserUnit(spec)
	if e != nil {
		return e
	}
	backend := s.backend
	if backend == "" || backend == "auto" {
		backend = "manual"
		if _, e := os.Stat("/run/systemd/system"); e == nil {
			backend = "systemd"
		} else if s.paths.IsWSL() {
			backend = "wsl"
		}
	}
	old, e := s.read(ctx)
	if e == nil && old.Backend != backend {
		return errors.New("remove the previous startup integration before changing service backend")
	}
	if backend == "systemd" {
		root, e := s.unitRoot()
		if e != nil {
			return e
		}
		if _, e = s.paths.files.PrivateState(ctx, root); e != nil {
			return e
		}
		if e = s.paths.files.AtomicWrite(ctx, root, UnitName, []byte(text)); e != nil {
			return e
		}
		if e = s.control(ctx, "daemon-reload"); e != nil {
			return e
		}
		if e = s.control(ctx, "enable", UnitName); e != nil {
			return e
		}
	}
	data, e := json.Marshal(serviceConfig{backend, spec})
	if e != nil {
		return errors.New("invalid service specification")
	}
	if e = s.paths.files.AtomicWrite(ctx, s.paths.state, "service.json", data); e != nil {
		return e
	}
	if backend == "wsl" {
		return errors.New("WSL supervisor configured; automatic boot installation is unavailable; install the reviewed WSL boot command separately")
	}
	return nil
}
func (s *service) read(ctx context.Context) (serviceConfig, error) {
	var config serviceConfig
	f, e := s.paths.files.OpenNoFollow(ctx, s.paths.state, "service.json", false)
	if e != nil {
		return config, errors.New("service is not configured; run service install first")
	}
	defer f.Close()
	data, e := io.ReadAll(io.LimitReader(f, 65537))
	if e != nil || len(data) > 65536 || json.Unmarshal(data, &config) != nil || config.Spec.StateDir != s.paths.state {
		return serviceConfig{}, errors.New("invalid service configuration")
	}
	return config, nil
}
func (s *service) Uninstall(ctx context.Context) error {
	config, e := s.read(ctx)
	if e != nil {
		return e
	}
	if e = s.Stop(ctx); e != nil {
		return e
	}
	if config.Backend == "systemd" {
		if e = s.control(ctx, "disable", UnitName); e != nil {
			return e
		}
		root, e := s.unitRoot()
		if e != nil {
			return e
		}
		if e = s.paths.files.Remove(ctx, root, UnitName); e != nil {
			return e
		}
		if e = s.control(ctx, "daemon-reload"); e != nil {
			return e
		}
	}
	return s.paths.files.Remove(ctx, s.paths.state, "service.json")
}
func (s *service) record(ctx context.Context) (platform.ProcessRef, error) {
	f, e := s.paths.files.OpenNoFollow(ctx, s.paths.state, "supervisor.json", false)
	if e != nil {
		return platform.ProcessRef{}, e
	}
	defer f.Close()
	var record platform.ProcessRef
	e = json.NewDecoder(io.LimitReader(f, 4096)).Decode(&record)
	if e != nil {
		return record, errors.New("invalid supervisor record")
	}
	return record, s.paths.process.Pin(ctx, record)
}
func (s *service) Status(ctx context.Context) (platform.ServiceStatus, error) {
	config, e := s.read(ctx)
	if e != nil {
		return platform.ServiceStatus{Detail: "service is not configured"}, nil
	}
	running := false
	if config.Backend == "systemd" {
		running = s.control(ctx, "is-active", UnitName) == nil
	} else {
		_, e = s.record(ctx)
		running = e == nil
	}
	if config.Backend == "wsl" {
		return platform.ServiceStatus{Installed: false, Running: running, Detail: "WSL supervisor configured; boot integration requires installing the reviewed WSL boot command"}, nil
	}
	return platform.ServiceStatus{Installed: true, Running: running, Detail: config.Backend}, nil
}
func (s *service) Start(ctx context.Context) error {
	config, e := s.read(ctx)
	if e != nil {
		return e
	}
	if config.Backend == "systemd" {
		return s.control(ctx, "start", UnitName)
	}
	if _, e = s.record(ctx); e == nil {
		return nil
	}
	cmd := exec.Command(config.Spec.Executable, "--state-dir", s.paths.state, "service", "supervise")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if e = cmd.Start(); e != nil {
		return errors.New("supervisor failed to start")
	}
	go cmd.Wait()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("supervisor startup timed out; inspect service status")
		case <-ticker.C:
			if _, e = s.record(ctx); e == nil {
				return nil
			}
		}
	}
}
func (s *service) Stop(ctx context.Context) error {
	config, e := s.read(ctx)
	if e != nil {
		return e
	}
	if config.Backend == "systemd" {
		return s.control(ctx, "stop", UnitName)
	}
	record, e := s.record(ctx)
	if e != nil {
		return nil
	}
	// pidfd pins the signal target. Never signal an unpinned numeric PID.
	fd, _, errno := syscall.Syscall(434, uintptr(record.PID), 0, 0)
	if errno != 0 {
		return errors.New("safe service stop requires Linux pidfd support")
	}
	defer syscall.Close(int(fd))
	if e = s.paths.process.Pin(ctx, record); e != nil {
		return nil
	}
	_, _, errno = syscall.Syscall6(424, fd, uintptr(syscall.SIGTERM), 0, 0, 0, 0)
	if errno != 0 && errno != syscall.ESRCH {
		return errors.New("supervisor stop failed")
	}
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("supervisor did not stop within 60 seconds")
		case <-ticker.C:
			if _, e = s.record(ctx); e != nil {
				return nil
			}
		}
	}
}

type logWriter struct {
	root string
	mu   sync.Mutex
}

func (w *logWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	path := filepath.Join(w.root, "service.log")
	if info, e := os.Lstat(path); e == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return 0, errUnsafe
		}
		if info.Size()+int64(len(b)) > logLimit {
			if e = os.Rename(path, path+".1"); e != nil {
				return 0, errors.New("service log unavailable")
			}
		}
	}
	fd, e := syscall.Open(path, syscall.O_WRONLY|syscall.O_APPEND|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return 0, errors.New("service log unavailable")
	}
	f := os.NewFile(uintptr(fd), "log")
	defer f.Close()
	return f.Write(b)
}
func (s *service) Supervise(ctx context.Context) error {
	config, e := s.read(ctx)
	if e != nil {
		return e
	}
	if config.Backend == "systemd" {
		return errors.New("systemd manages this service; use service start")
	}
	guard, e := s.paths.files.AdvisoryLock(ctx, s.paths.state, "supervisor.lock", false)
	if e != nil {
		return nil
	}
	defer guard.Close()
	ref, e := s.paths.process.Current(ctx)
	if e != nil {
		return e
	}
	data, e := json.Marshal(ref)
	if e != nil {
		return e
	}
	if e = s.paths.files.AtomicWrite(ctx, s.paths.state, "supervisor.json", data); e != nil {
		return e
	}
	defer s.paths.files.Remove(context.Background(), s.paths.state, "supervisor.json")
	delay := time.Second
	writer := &logWriter{root: s.paths.state}
	for {
		if ctx.Err() != nil {
			return nil
		}
		started := time.Now()
		args := append([]string{"--state-dir", s.paths.state}, config.Spec.Arguments...)
		if len(config.Spec.Arguments) == 0 {
			args = append(args, "daemon")
		}
		cmd := exec.Command(config.Spec.Executable, args...)
		cmd.Stdout = writer
		cmd.Stderr = writer
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if e = cmd.Start(); e != nil {
			return errors.New("helper failed to start")
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-ctx.Done():
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(55 * time.Second):
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
			}
			return nil
		case <-done:
		}
		writer.Write([]byte("helper exited; restarting in " + strconv.Itoa(int(delay/time.Second)) + "s\n"))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if time.Since(started) > 60*time.Second {
			delay = time.Second
		} else if delay < 60*time.Second {
			delay *= 2
			if delay > 60*time.Second {
				delay = 60 * time.Second
			}
		}
	}
}
