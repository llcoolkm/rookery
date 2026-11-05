//go:build linux

package linux

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/user"
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
	// Private injection points keep tests away from account databases and /etc.
	wslConfigPath  string
	lookupAccount  func(string) (*user.User, error)
	controlCommand func(context.Context, ...string) error
}
type serviceConfig struct {
	Backend       string               `json:"backend"`
	Spec          platform.ServiceSpec `json:"spec"`
	BootCommand   string               `json:"boot_command,omitempty"`
	BootInstalled bool                 `json:"boot_installed,omitempty"`
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
	environment := ""
	for _, v := range []string{spec.Home, spec.SearchPath} {
		if strings.ContainsAny(v, "\r\n\x00") {
			return "", errors.New("service paths must not contain newlines")
		}
	}
	if spec.Home != "" {
		if !filepath.IsAbs(spec.Home) {
			return "", errors.New("service home must be absolute")
		}
		environment += "WorkingDirectory=" + strings.ReplaceAll(spec.Home, "%", "%%") + "\nEnvironment=" + environmentQuote("HOME="+spec.Home) + "\n"
	}
	if spec.SearchPath != "" {
		environment += "Environment=" + environmentQuote("PATH="+spec.SearchPath) + "\n"
	}
	return "[Unit]\nDescription=Rookery Go message helper\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\n" + environment + "ExecStart=" + strings.Join(args, " ") + "\nRestart=always\nRestartSec=5\nTimeoutStopSec=60\nKillMode=mixed\nUMask=0077\nNoNewPrivileges=true\n\n[Install]\nWantedBy=default.target\n", nil
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
	if s.controlCommand != nil {
		return s.controlCommand(ctx, args...)
	}
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
	if e := validateServiceExecutable(spec.Executable); e != nil {
		return e
	}
	backend := s.selectedBackend()
	resolved, e := s.resolveAccount(spec)
	if e != nil {
		return e
	}
	spec = resolved
	text, e := UserUnit(spec)
	if e != nil {
		return e
	}
	targetUID, targetGID, e := s.accountIDs(spec.Account)
	if e != nil {
		return e
	}
	handoff := targetUID != os.Getuid()
	if handoff && backend != "wsl" {
		return errors.New("cross-account installation is only supported for a fresh WSL state directory")
	}
	if handoff {
		if e = s.validateHandoff(spec, targetUID, targetGID); e != nil {
			return e
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
		if e = s.writeUserUnit(ctx, root, text); e != nil {
			return e
		}
		if e = s.control(ctx, "daemon-reload"); e != nil {
			return e
		}
		if e = s.control(ctx, "enable", UnitName); e != nil {
			return e
		}
	}

	config := serviceConfig{Backend: backend, Spec: spec}
	if backend == "wsl" {
		resolved, e := s.resolveAccount(spec)
		if e != nil {
			return e
		}
		config.Spec = resolved
		command, e := WSLBootCommand(resolved, resolved.Account, resolved.Home, resolved.SearchPath)
		if e != nil {
			return e
		}
		config.BootCommand = command
		previous := ""
		if old.Backend == "wsl" {
			previous = old.BootCommand
			if previous == "" && old.Spec.Account != "" {
				previous, _ = WSLBootCommand(old.Spec, old.Spec.Account, old.Spec.Home, old.Spec.SearchPath)
			}
		}
		if e = s.writeConfig(ctx, config); e != nil {
			return e
		}
		if e = s.installWSL(command, previous); e != nil {
			return e
		}
		config.BootInstalled = true
	}
	if e = s.writeConfig(ctx, config); e != nil {
		return e
	}
	if handoff {
		return s.handoffState(targetUID, targetGID)
	}
	return nil
}
func (s *service) writeConfig(ctx context.Context, config serviceConfig) error {
	data, e := json.Marshal(config)
	if e != nil {
		return errors.New("invalid service specification")
	}
	return s.paths.files.AtomicWrite(ctx, s.paths.state, "service.json", data)
}
func (s *service) resolveAccount(spec platform.ServiceSpec) (platform.ServiceSpec, error) {
	lookup := s.lookupAccount
	if lookup == nil {
		lookup = user.Lookup
	}
	account := spec.Account
	if account == "" {
		account = os.Getenv("SUDO_USER")
		if account == "" {
			current, e := user.Current()
			if e != nil {
				return spec, errors.New("service target account unavailable")
			}
			account = current.Username
		}
	}
	if account == "" || strings.ContainsAny(account, "\x00\r\n:/ \t") {
		return spec, errors.New("invalid service target account")
	}
	target, e := lookup(account)
	if e != nil {
		return spec, errors.New("service target account unavailable")
	}
	uid, e := strconv.Atoi(target.Uid)
	if e != nil || (uid != os.Getuid() && os.Geteuid() != 0) {
		return spec, errors.New("service target must own this private state directory; cross-account installation requires a fresh state directory")
	}
	if spec.Home != "" && spec.Home != target.HomeDir {
		return spec, errors.New("service home differs from target account")
	}
	spec.Account = target.Username
	spec.Home = target.HomeDir
	if !filepath.IsAbs(spec.Home) || strings.ContainsAny(spec.Home, "\x00\r\n") {
		return spec, errors.New("invalid service home")
	}
	if spec.SearchPath == "" {
		path := os.Getenv("PATH")
		if path == "" {
			path = "/usr/local/bin:/usr/bin:/bin"
		}
		spec.SearchPath = filepath.Join(spec.Home, ".local", "bin") + ":" + filepath.Join(spec.Home, ".npm-global", "bin") + ":" + path
	}
	if strings.ContainsAny(spec.SearchPath, "\x00\r\n") {
		return spec, errors.New("service PATH must not contain newlines")
	}
	return spec, nil
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
	if config.Backend == "wsl" && config.BootInstalled {
		if e = s.uninstallWSL(config.BootCommand); e != nil {
			return e
		}
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
	if config.Backend == "wsl" && (!config.BootInstalled || !s.wslPresent(config.BootCommand)) {
		return platform.ServiceStatus{Installed: false, Running: running, Detail: "WSL supervisor configured; boot integration is incomplete"}, nil
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
	if config.Spec.Account != "" {
		if _, e = s.resolveAccount(config.Spec); e != nil {
			return e
		}
	}
	cmd := exec.Command(config.Spec.Executable, "--state-dir", s.paths.state, "service", "supervise")
	cmd.Env = serviceEnvironment(config.Spec)
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
	if config.Spec.Account != "" {
		if _, e = s.resolveAccount(config.Spec); e != nil {
			return e
		}
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
		cmd.Env = serviceEnvironment(config.Spec)
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

func serviceEnvironment(spec platform.ServiceSpec) []string {
	env := []string{}
	for _, item := range os.Environ() {
		if spec.Home != "" && strings.HasPrefix(item, "HOME=") {
			continue
		}
		if spec.SearchPath != "" && strings.HasPrefix(item, "PATH=") {
			continue
		}
		env = append(env, item)
	}
	if spec.Home != "" {
		env = append(env, "HOME="+spec.Home)
	}
	if spec.SearchPath != "" {
		env = append(env, "PATH="+spec.SearchPath)
	}
	return env
}

func environmentQuote(v string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%").Replace(v) + "\""
}
func (s *service) accountIDs(account string) (int, int, error) {
	lookup := s.lookupAccount
	if lookup == nil {
		lookup = user.Lookup
	}
	target, e := lookup(account)
	if e != nil {
		return 0, 0, errors.New("service target account unavailable")
	}
	uid, e := strconv.Atoi(target.Uid)
	if e != nil {
		return 0, 0, errUnsafe
	}
	gid, e := strconv.Atoi(target.Gid)
	if e != nil {
		return 0, 0, errUnsafe
	}
	return uid, gid, nil
}

// A cross-account boot install never adopts existing helper data. Only an empty
// root-owned directory under ancestry traversable by the target may be handed off.
func (s *service) validateHandoff(spec platform.ServiceSpec, uid, gid int) error {
	entries, e := os.ReadDir(spec.StateDir)
	if e != nil || len(entries) != 0 {
		return errors.New("cross-account WSL installation requires an empty fresh state directory")
	}
	for path := filepath.Dir(spec.StateDir); ; path = filepath.Dir(path) {
		info, e := os.Lstat(path)
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errUnsafe
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errUnsafe
		}
		executable := st.Mode&0001 != 0
		if int(st.Uid) == uid {
			executable = st.Mode&0100 != 0
		} else if int(st.Gid) == gid {
			executable = st.Mode&0010 != 0
		}
		if !executable {
			return errors.New("service state ancestry is inaccessible to target account")
		}
		if path == "/" {
			break
		}
	}
	return nil
}
func (s *service) handoffState(uid, gid int) error {
	fd, e := dirOpen(s.paths.state, true)
	if e != nil {
		return e
	}
	defer syscall.Close(fd)
	raw, e := syscall.Openat(fd, "service.json", syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return errUnsafe
	}
	defer syscall.Close(raw)
	var info syscall.Stat_t
	if syscall.Fstat(raw, &info) != nil || info.Mode&syscall.S_IFMT != syscall.S_IFREG || info.Nlink != 1 || info.Uid != uint32(os.Geteuid()) || info.Mode&0077 != 0 {
		return errUnsafe
	}
	if syscall.Fchown(raw, uid, gid) != nil || syscall.Fsync(raw) != nil || syscall.Fchown(fd, uid, gid) != nil || syscall.Fsync(fd) != nil {
		return errors.New("service state ownership handoff failed")
	}
	return nil
}

// User-unit directories conventionally use 0755. Unit text is public metadata,
// so its parent need not be private helper state; ownership/no-follow and
// no-group-write checks still apply and the unit itself is created 0600.
func (s *service) writeUserUnit(ctx context.Context, root, text string) error {
	fd, e := dirOpen(root, false)
	if e != nil {
		if _, e = s.paths.files.PrivateState(ctx, root); e != nil {
			return e
		}
		fd, e = dirOpen(root, false)
		if e != nil {
			return e
		}
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0 {
		return errUnsafe
	}
	if _, _, e = readWSLFile(fd, UnitName); e != nil {
		return e
	}
	return atomicWSLFile(fd, UnitName, []byte(text), 0600, false)
}

func validateServiceExecutable(path string) error {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return errors.New("specify an absolute installed native executable")
	}
	file, e := os.Open(path)
	if e != nil {
		return errors.New("service executable unavailable")
	}
	defer file.Close()
	info, e := file.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return errors.New("service executable unavailable")
	}
	var magic [4]byte
	if _, e = io.ReadFull(file, magic[:]); e != nil || magic != [4]byte{0x7f, 'E', 'L', 'F'} {
		return errors.New("service executable must be a native ELF binary")
	}
	return nil
}

func (s *service) selectedBackend() string {
	backend := s.backend
	if backend != "" && backend != "auto" {
		return backend
	}
	if info, e := os.Stat("/run/systemd/system"); e == nil && info.IsDir() {
		return "systemd"
	}
	if s.paths.IsWSL() {
		return "wsl"
	}
	return "manual"
}

// Plan validates and renders startup integration without changing files,
// contacting a service manager, or starting a supervisor.
func (s *service) Plan(ctx context.Context, spec platform.ServiceSpec) (any, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if spec.StateDir != s.paths.state {
		return nil, errors.New("service configuration belongs to another state directory")
	}
	if e := validateServiceExecutable(spec.Executable); e != nil {
		return nil, e
	}
	spec, e := s.resolveAccount(spec)
	if e != nil {
		return nil, e
	}
	backend := s.selectedBackend()
	content := ""
	switch backend {
	case "systemd":
		uid, _, e := s.accountIDs(spec.Account)
		if e != nil {
			return nil, e
		}
		if uid != os.Getuid() {
			return nil, errors.New("systemd user installation requires the current account")
		}
		content, e = UserUnit(spec)
		if e != nil {
			return nil, e
		}
	case "wsl":
		content, e = WSLBootCommand(spec, spec.Account, spec.Home, spec.SearchPath)
		if e != nil {
			return nil, e
		}
	case "manual":
		args := append([]string{spec.Executable, "--state-dir", spec.StateDir}, spec.Arguments...)
		if len(spec.Arguments) == 0 {
			args = append(args, "daemon")
		}
		for i, arg := range args {
			if strings.ContainsAny(arg, "\r\n\x00") {
				return nil, errors.New("service paths must not contain newlines")
			}
			args[i] = shellQuote(arg)
		}
		content = strings.Join(args, " ")
	default:
		return nil, errors.New("invalid service backend")
	}
	return map[string]any{"backend": backend, "content": content, "started": false}, nil
}
