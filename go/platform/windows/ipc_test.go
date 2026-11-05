//go:build windows

package windows

import (
	"bufio"
	"context"
	"github.com/llcoolkm/rookery/go/platform"
	win "golang.org/x/sys/windows"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func testIPC(t *testing.T) (*ipc, *paths) {
	t.Helper()
	f, e := newFiles()
	if e != nil {
		t.Fatal(e)
	}
	proc, e := NewProcess()
	if e != nil {
		t.Fatal(e)
	}
	p, e := newPaths(filepath.Join(t.TempDir(), "state"), f, proc)
	if e != nil {
		t.Fatal(e)
	}
	return &ipc{paths: p}, p
}
func TestPipeTwoProcesses(t *testing.T) {
	i, p := testIPC(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ep, e := p.Endpoint("fixture")
	if e != nil {
		t.Fatal(e)
	}
	listener, e := i.Listen(ctx, ep)
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	if _, e = i.Listen(ctx, ep); e == nil {
		t.Fatal("first-instance collision accepted")
	}
	entry, _ := p.get(ep)
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPipeChild$")
	child.Env = append(os.Environ(), "ROOKERY_PIPE_FIXTURE="+entry.path, "ROOKERY_PIPE_PARENT="+strconv.Itoa(os.Getpid()))
	if e = child.Start(); e != nil {
		t.Fatal(e)
	}
	defer child.Process.Kill()
	conn, e := listener.Accept(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	pin, e := conn.Peer()
	if e != nil || pin.PID != child.Process.Pid {
		t.Fatal("client process not pinned", e)
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	b := make([]byte, 4)
	if _, e = io.ReadFull(conn, b); e != nil || string(b) != "ping" {
		t.Fatal("pipe read", e)
	}
	if _, e = conn.Write([]byte("pong")); e != nil {
		t.Fatal(e)
	}
	if e = child.Wait(); e != nil {
		t.Fatal(e)
	}
	if _, e = conn.Peer(); e == nil {
		t.Fatal("exited process remained valid")
	}
}
func TestPipeChild(t *testing.T) {
	name := os.Getenv("ROOKERY_PIPE_FIXTURE")
	if name == "" {
		return
	}
	i, p := testIPC(t)
	pid, e := strconv.Atoi(os.Getenv("ROOKERY_PIPE_PARENT"))
	if e != nil {
		t.Fatal(e)
	}
	pin, e := p.process.Lookup(context.Background(), pid)
	if e != nil {
		t.Fatal(e)
	}
	ep, e := p.issue(endpoint{path: name, expected: &pin})
	if e != nil {
		t.Fatal(e)
	}
	conn, e := i.Dial(context.Background(), ep)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, e = conn.Write([]byte("ping")); e != nil {
		t.Fatal(e)
	}
	b := make([]byte, 4)
	if _, e = io.ReadFull(conn, b); e != nil || string(b) != "pong" {
		t.Fatal("pipe reply", e)
	}
}
func TestUnissuedEndpoint(t *testing.T) {
	i, _ := testIPC(t)
	if _, e := i.Dial(context.Background(), platform.Endpoint("unissued")); e == nil {
		t.Fatal("unissued endpoint accepted")
	}
}

// The native pipe carries Claude's documented first-line auth frame without
// putting tokens or message text in a command line. This synthetic wire fixture
// does not assert an undocumented harness session-record token schema.
func TestFakeClaudePipeFirstLine(t *testing.T) {
	i, p := testIPC(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ep, e := p.Endpoint("claude-fixture")
	if e != nil {
		t.Fatal(e)
	}
	l, e := i.Listen(ctx, ep)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	done := make(chan error, 1)
	go func() {
		c, e := i.Dial(ctx, ep)
		if e != nil {
			done <- e
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		_, e = c.Write([]byte("{\"type\":\"auth\",\"token\":\"synthetic-fixture\"}\n{\"type\":\"user\",\"msgV\":1}\n"))
		done <- e
	}()
	c, e := l.Accept(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	r := bufio.NewReader(c)
	first, e := r.ReadString('\n')
	if e != nil || first != "{\"type\":\"auth\",\"token\":\"synthetic-fixture\"}\n" {
		t.Fatal("auth first line absent", e)
	}
	second, e := r.ReadString('\n')
	if e != nil || second != "{\"type\":\"user\",\"msgV\":1}\n" {
		t.Fatal("message frame absent", e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}

// Lower only a disposable duplicate token. This exercises the real native
// integrity query and refusal without changing this process or another account.
func TestLowerIntegrityTokenRefused(t *testing.T) {
	var current win.Token
	if e := win.OpenProcessToken(win.CurrentProcess(), win.TOKEN_QUERY|win.TOKEN_DUPLICATE, &current); e != nil {
		t.Fatal(e)
	}
	defer current.Close()
	level, e := tokenIntegrity(current)
	if e != nil || level <= 4096 {
		t.Fatal("fixture requires a token above low integrity", e)
	}
	if e = tokenAllowed(current, current, false); e != nil {
		t.Fatal("baseline token refused", e)
	}
	var lower win.Token
	if e = win.DuplicateTokenEx(current, win.TOKEN_QUERY|win.TOKEN_ADJUST_DEFAULT, nil, win.SecurityImpersonation, win.TokenImpersonation, &lower); e != nil {
		t.Fatal(e)
	}
	defer lower.Close()
	sid, e := win.StringToSid("S-1-16-4096")
	if e != nil {
		t.Fatal(e)
	}
	label := win.Tokenmandatorylabel{Label: win.SIDAndAttributes{Sid: sid, Attributes: win.SE_GROUP_INTEGRITY}}
	if e = win.SetTokenInformation(lower, win.TokenIntegrityLevel, (*byte)(unsafe.Pointer(&label)), label.Size()); e != nil {
		t.Fatal(e)
	}
	runtime.KeepAlive(sid)
	if level, e = tokenIntegrity(lower); e != nil || level != 4096 {
		t.Fatal("duplicate did not acquire low integrity", e)
	}
	original, e := current.GetTokenUser()
	if e != nil {
		t.Fatal(e)
	}
	changed, e := lower.GetTokenUser()
	if e != nil || !win.EqualSid(original.User.Sid, changed.User.Sid) {
		t.Fatal("duplicate unexpectedly changed principal", e)
	}
	if e = tokenAllowed(lower, current, false); e == nil {
		t.Fatal("same-user lower-integrity token accepted")
	}
}

//go:noinline
func growPendingIOStack(depth int) byte {
	var frame [8192]byte
	frame[0] = byte(depth)
	if depth > 0 {
		frame[1] = growPendingIOStack(depth - 1)
	}
	runtime.KeepAlive(&frame)
	return frame[0] ^ frame[1]
}

func TestPendingIOStackGrowth(t *testing.T) {
	i, p := testIPC(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ep, err := p.Endpoint("stack-growth-fixture")
	if err != nil {
		t.Fatal(err)
	}
	pl, err := i.Listen(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	l := pl.(*listener)
	defer l.Close()
	ref, err := p.process.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := p.get(ep)
	name, _ := win.UTF16PtrFromString(entry.path)
	client, err := win.CreateFile(name, win.GENERIC_READ|win.GENERIC_WRITE, 0, nil, win.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer win.CloseHandle(client)
	// A client may connect before ConnectNamedPipe. This raw synthetic pair
	// isolates transport from the first-message token protocol.
	if err = win.ConnectNamedPipe(l.pending, nil); err != nil && err != win.ERROR_PIPE_CONNECTED {
		t.Fatal(err)
	}
	c := &connection{handle: l.pending, peer: ref, process: p.process}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	oldWait := waitForIO
	defer func() { waitForIO = oldWait }()
	pending := make(chan struct{})
	grown := false
	waitForIO = func(event win.Handle, timeout uint32) (uint32, error) {
		if !grown {
			var n uint32
			if err := win.GetOverlappedResult(c.handle, c.readOverlapped, &n, false); err != win.ERROR_IO_INCOMPLETE {
				t.Fatalf("read was not pending: %v", err)
			}
			growPendingIOStack(64)
			runtime.GC()
			grown = true
			close(pending)
		}
		return oldWait(event, timeout)
	}
	writeDone := make(chan error, 1)
	go func() {
		select {
		case <-pending:
		case <-ctx.Done():
			writeDone <- ctx.Err()
			return
		}
		var n uint32
		writeDone <- win.WriteFile(client, []byte("x"), &n, nil)
	}()
	var b [1]byte
	if n, err := c.Read(b[:]); err != nil || n != 1 || b[0] != 'x' || !grown {
		t.Fatalf("pending read after stack growth: bytes=%d error=%v", n, err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}

func TestPendingAcceptStackGrowth(t *testing.T) {
	i, p := testIPC(t)
	ep, err := p.Endpoint("accept-stack-growth-fixture")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pl, err := i.Listen(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	l := pl.(*listener)
	defer l.Close()
	oldWait := waitForIO
	defer func() { waitForIO = oldWait }()
	grown := false
	waitForIO = func(event win.Handle, timeout uint32) (uint32, error) {
		if !grown {
			var n uint32
			if err := win.GetOverlappedResult(l.pending, l.overlapped, &n, false); err != win.ERROR_IO_INCOMPLETE {
				t.Fatalf("accept was not pending: %v", err)
			}
			growPendingIOStack(64)
			runtime.GC()
			grown = true
			cancel()
		}
		return oldWait(event, timeout)
	}
	if _, err := l.Accept(ctx); err != context.Canceled || !grown {
		t.Fatalf("pending accept cancellation after stack growth: %v", err)
	}
}

func TestCloseDrainsPendingAccept(t *testing.T) {
	i, p := testIPC(t)
	ep, err := p.Endpoint("accept-close-fixture")
	if err != nil {
		t.Fatal(err)
	}
	l, err := i.Listen(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	oldWait := waitForIO
	defer func() { waitForIO = oldWait }()
	pending := make(chan struct{})
	var once sync.Once
	waitForIO = func(event win.Handle, timeout uint32) (uint32, error) {
		once.Do(func() { close(pending) })
		return oldWait(event, timeout)
	}
	accepted := make(chan error, 1)
	go func() {
		_, err := l.Accept(context.Background())
		accepted <- err
	}()
	select {
	case <-pending:
	case <-time.After(3 * time.Second):
		t.Fatal("accept did not become pending")
	}
	closed := make(chan error, 1)
	go func() { closed <- l.Close() }()
	select {
	case err := <-accepted:
		if err == nil {
			t.Fatal("closed listener accepted a connection")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending accept did not drain on close")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("close did not finish after draining accept")
	}
}
