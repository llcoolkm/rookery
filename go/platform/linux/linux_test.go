//go:build linux

package linux

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/llcoolkm/rookery/go/platform"
)

func testHost(t *testing.T) (platform.Host, string) {
	t.Helper()
	root := t.TempDir()
	os.Chmod(root, 0700)
	host, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	return host, root
}
func TestFilesPrivateDurableAndNoFollow(t *testing.T) {
	ctx := context.Background()
	host, root := testHost(t)
	if e := host.Files.AtomicWrite(ctx, root, "item", []byte("first")); e != nil {
		t.Fatal(e)
	}
	if e := host.Files.AtomicWrite(ctx, root, "item", []byte("second")); e != nil {
		t.Fatal(e)
	}
	f, e := host.Files.OpenNoFollow(ctx, root, "item", false)
	if e != nil {
		t.Fatal(e)
	}
	data, e := io.ReadAll(f)
	f.Close()
	if e != nil || string(data) != "second" {
		t.Fatal("atomic write lost data")
	}
	info, _ := os.Stat(filepath.Join(root, "item"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe file mode")
	}
	if _, e = host.Files.OpenNoFollow(ctx, root, "missing", false); !errors.Is(e, fs.ErrNotExist) {
		t.Fatal("missing file not classified")
	}
	os.Symlink("item", filepath.Join(root, "link"))
	if _, e = host.Files.OpenNoFollow(ctx, root, "link", false); e == nil {
		t.Fatal("followed symlink")
	}
	if e = host.Files.AtomicWrite(ctx, root, "link", nil); e == nil {
		t.Fatal("replaced symlink")
	}
	if _, e = host.Files.OpenNoFollow(ctx, root, "../outside", true); e == nil {
		t.Fatal("escaped root")
	}
	os.Mkdir(filepath.Join(root, "nested"), 0700)
	os.Symlink("nested", filepath.Join(root, "dirlink"))
	if _, e = host.Files.OpenNoFollow(ctx, root, "dirlink/item", true); e == nil {
		t.Fatal("followed directory symlink")
	}
	os.Chmod(filepath.Join(root, "item"), 0644)
	if _, e = host.Files.OpenNoFollow(ctx, root, "item", false); e == nil {
		t.Fatal("accepted public file")
	}
	lock, e := host.Files.AdvisoryLock(ctx, root, "lock", false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = host.Files.AdvisoryLock(ctx, root, "lock", false); e == nil {
		t.Fatal("duplicate lock acquired")
	}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, e = host.Files.AdvisoryLock(short, root, "lock", true); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("lock wait ignored cancellation")
	}
	lock.Close()
}
func TestPrivateStateRejectsUnsafeRoots(t *testing.T) {
	root := t.TempDir()
	os.Chmod(root, 0755)
	if _, e := New(root); e == nil {
		t.Fatal("accepted nonprivate root")
	}
	os.Chmod(root, 0700)
	link := filepath.Join(t.TempDir(), "state")
	os.Symlink(root, link)
	if _, e := New(link); e == nil {
		t.Fatal("followed state symlink")
	}
}
func TestProcessPinsAndReuse(t *testing.T) {
	host, _ := testHost(t)
	ctx := context.Background()
	ref, e := host.Process.Current(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = host.Process.Pin(ctx, ref); e != nil {
		t.Fatal(e)
	}
	ref.Start += "changed"
	if e = host.Process.Pin(ctx, ref); e == nil {
		t.Fatal("accepted changed process identity")
	}
	ref, e = host.Process.Current(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = host.Process.CommandLine(ctx, ref); e != nil {
		t.Fatal(e)
	}
	if ancestors, e := host.Process.Ancestors(ctx, ref); e != nil || len(ancestors) == 0 || ancestors[0] != ref {
		t.Fatal("invalid ancestry")
	}
}
func TestExecOutcomesAndBounds(t *testing.T) {
	host, _ := testHost(t)
	ctx := context.Background()
	path, e := exec.LookPath("cat")
	if e != nil {
		t.Skip("native cat unavailable")
	}
	path, _ = filepath.Abs(path)
	spec := platform.Spec{Path: path, Stdin: []byte("private body"), Timeout: time.Second, MaxOutput: 1024}
	result, e := host.Exec.Exec(ctx, spec)
	if e != nil || result.State != platform.Completed || string(result.Stdout) != "private body" {
		t.Fatal("stdin execution failed")
	}
	spec.Path = "relative"
	result, e = host.Exec.Exec(ctx, spec)
	if e == nil || result.State != platform.NotStarted {
		t.Fatal("invalid path may have started")
	}
	spec.Path = path
	spec.MaxOutput = 4
	result, e = host.Exec.Exec(ctx, spec)
	if e == nil || result.State != platform.Uncertain || !result.Truncated || len(result.Stdout)+len(result.Stderr) > 4 {
		t.Fatal("output bound failed")
	}
	sleep, e := exec.LookPath("sleep")
	if e != nil {
		t.Skip("native sleep unavailable")
	}
	sleep, _ = filepath.Abs(sleep)
	result, e = host.Exec.Exec(ctx, platform.Spec{Path: sleep, Args: []string{"2"}, Timeout: 20 * time.Millisecond, MaxOutput: 1024})
	if e == nil || result.State != platform.Uncertain {
		t.Fatal("timeout treated as definite")
	}
	falsePath, e := exec.LookPath("false")
	if e == nil {
		falsePath, _ = filepath.Abs(falsePath)
		result, e = host.Exec.Exec(ctx, platform.Spec{Path: falsePath, Timeout: time.Second, MaxOutput: 1024})
		if e != nil || result.State != platform.Completed || result.ExitCode == 0 {
			t.Fatal("nonzero exit lost known completion")
		}
	}
}
func TestIPCPeerAndOpaqueHandles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	host, _ := testHost(t)
	if _, e := host.IPC.Dial(ctx, platform.Endpoint("invented")); e == nil {
		t.Fatal("accepted unissued endpoint")
	}
	ep, e := host.Paths.Endpoint("ipc.sock")
	if e != nil {
		t.Fatal(e)
	}
	l, e := host.IPC.Listen(ctx, ep)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	accepted := make(chan platform.Connection, 1)
	go func() { c, _ := l.Accept(ctx); accepted <- c }()
	client, e := host.IPC.Dial(ctx, ep)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	server := <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	defer server.Close()
	peer, e := client.Peer()
	if e != nil || peer.PID != os.Getpid() || peer.Start == "" || peer.BootID == "" {
		t.Fatal("peer was not pinned")
	}
	if _, e = client.Write([]byte("hello")); e != nil {
		t.Fatal(e)
	}
	buffer := make([]byte, 5)
	if _, e = io.ReadFull(server, buffer); e != nil || string(buffer) != "hello" {
		t.Fatal("IPC transfer failed")
	}
}
func TestHarnessEndpointRecordProvenance(t *testing.T) {
	host, root := testHost(t)
	ctx := context.Background()
	providerRoot := filepath.Join(root, "claude")
	os.Mkdir(providerRoot, 0700)
	t.Setenv("CLAUDE_CONFIG_DIR", providerRoot)
	socket := filepath.Join(providerRoot, "native.sock")
	listener, e := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	os.Chmod(socket, 0600)
	ref, e := host.Process.Current(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = host.Files.AtomicWrite(ctx, providerRoot, "record.json", []byte("{}")); e != nil {
		t.Fatal(e)
	}
	record, e := host.Files.OpenNoFollow(ctx, providerRoot, "record.json", false)
	if e != nil {
		t.Fatal(e)
	}
	ep, e := host.Paths.HarnessEndpoint(ctx, "claude", record, socket, ref)
	if e != nil {
		t.Fatal(e)
	}
	record.Close()
	c, e := host.IPC.Dial(ctx, ep)
	if e != nil {
		t.Fatal(e)
	}
	c.Close()
	if _, e = host.Paths.HarnessEndpoint(ctx, "claude", record, socket, ref); e == nil {
		t.Fatal("accepted closed record")
	}
	foreign, e := os.Open(filepath.Join(providerRoot, "record.json"))
	if e != nil {
		t.Fatal(e)
	}
	defer foreign.Close()
	if _, e = host.Paths.HarnessEndpoint(ctx, "claude", foreign, socket, ref); e == nil {
		t.Fatal("accepted unissued record")
	}
	record, e = host.Files.OpenNoFollow(ctx, providerRoot, "record.json", false)
	if e != nil {
		t.Fatal(e)
	}
	defer record.Close()
	ref.Start += "changed"
	if _, e = host.Paths.HarnessEndpoint(ctx, "claude", record, socket, ref); e == nil {
		t.Fatal("accepted stale server")
	}
}
func TestSeparateStateAndUnitRendering(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ROOKERY_HOME", filepath.Join(root, "python"))
	t.Setenv("ROOKERY_GO_HOME", "")
	t.Setenv("XDG_STATE_HOME", root)
	p := &paths{}
	dir, e := p.StateDir()
	if e != nil || dir != filepath.Join(root, "rookery-go") {
		t.Fatal("state collision")
	}
	text, e := UserUnit(platform.ServiceSpec{Executable: "/native/%$program", StateDir: "/state/example"})
	if e != nil || !strings.Contains(text, "%%$$program") || strings.Contains(text, "rookery.service") || !strings.Contains(text, "UMask=0077") {
		t.Fatal("unit escaping or isolation failed")
	}
	if _, e = UserUnit(platform.ServiceSpec{Executable: "/native/program\nExecStart=bad", StateDir: "/state/example"}); e == nil {
		t.Fatal("accepted unit injection")
	}
}

func TestFixedControlEndpoint(t *testing.T) {
	host, root := testHost(t)
	ctx := context.Background()
	t.Setenv("CODEX_HOME", root)
	dir := filepath.Join(root, "app-server-control")
	os.Mkdir(dir, 0700)
	socket := filepath.Join(dir, "app-server-control.sock")
	l, e := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	os.Chmod(socket, 0600)
	resolver := host.Paths.(interface {
		ControlEndpoint(context.Context, string) (platform.Endpoint, error)
	})
	if _, e = resolver.ControlEndpoint(ctx, "unknown"); e == nil {
		t.Fatal("arbitrary provider control capability")
	}
	ep, e := resolver.ControlEndpoint(ctx, "codex")
	if e != nil {
		t.Fatal(e)
	}
	c, e := host.IPC.Dial(ctx, ep)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	ref, e := c.Peer()
	if e != nil || ref.PID != os.Getpid() {
		t.Fatal("control server not pinned")
	}
}
func TestWSLPlanPreservesStartup(t *testing.T) {
	spec := platform.ServiceSpec{Executable: "/native/program", StateDir: "/state/example"}
	command, e := WSLBootCommand(spec, "example", "/account/example", "/native/bin")
	if e != nil || !strings.Contains(command, "'service' 'supervise' '--detach'") {
		t.Fatal("invalid WSL boot command")
	}
	original := "[network]\nkey=value\n[boot]\ncommand=existing\n[interop]\nenabled=true\n"
	merged := MergeWSLConfig(original, command, "")
	if !strings.Contains(merged, "command=existing; "+command+"\n") || !strings.Contains(merged, "[interop]\nenabled=true") {
		t.Fatal("lost WSL configuration")
	}
	if MergeWSLConfig(merged, command, "") != merged {
		t.Fatal("WSL merge not idempotent")
	}
	changed := MergeWSLConfig(merged, "replacement", command)
	if !strings.Contains(changed, "command=existing; replacement\n") {
		t.Fatal("old helper command not replaced")
	}
}

func TestListDescriptorRelativeMetadata(t *testing.T) {
	host, root := testHost(t)
	ctx := context.Background()
	if e := host.Files.AtomicWrite(ctx, root, "entry", []byte("body")); e != nil {
		t.Fatal(e)
	}
	os.Symlink("entry", filepath.Join(root, "link"))
	entries, e := host.Files.List(ctx, root)
	if e != nil || len(entries) != 1 || entries[0].Name != "entry" || entries[0].Size != 4 {
		t.Fatal("descriptor relative listing failed", e)
	}
}

func TestWSLInstallReportsIncompleteBootIntegration(t *testing.T) {
	host, root := testHost(t)
	ctx := context.Background()
	svc := host.Service.(interface{ ConfigureBackend(string) error })
	if e := svc.ConfigureBackend("wsl"); e != nil {
		t.Fatal(e)
	}
	if e := host.Service.Install(ctx, platform.ServiceSpec{Executable: "/native/program", StateDir: root}); e == nil || !strings.Contains(e.Error(), "automatic boot installation is unavailable") {
		t.Fatal("WSL install claimed complete integration")
	}
	status, e := host.Service.Status(ctx)
	if e != nil || status.Installed || status.Running || !strings.Contains(status.Detail, "boot integration requires") {
		t.Fatal("WSL status hid incomplete startup integration")
	}
}
