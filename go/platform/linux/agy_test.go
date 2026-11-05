//go:build linux

package linux

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/llcoolkm/rookery/go/platform"
)

const agyTestConversation = "11111111-1111-4111-8111-111111111111"

func TestAGYAncestorUsesStatNameAndPinnedSameOwner(t *testing.T) {
	ctx := context.Background()
	refs := map[int]platform.ProcessRef{}
	for _, pid := range []int{10, 20, 30} {
		refs[pid] = platform.ProcessRef{PID: pid, Principal: "owner", Start: strconv.Itoa(100 - pid), BootID: "boot"}
	}
	names := map[int]string{10: "shell", 20: "agy", 30: "other"}
	parents := map[int]int{10: 20, 20: 30, 30: 1}
	lookup := func(_ context.Context, pid int) (platform.ProcessRef, error) {
		ref, ok := refs[pid]
		if !ok {
			return platform.ProcessRef{}, errors.New("gone")
		}
		return ref, nil
	}
	stat := func(_ context.Context, ref platform.ProcessRef) (string, int, error) {
		return names[ref.PID], parents[ref.PID], nil
	}
	pin := func(context.Context, platform.ProcessRef) error { return nil }
	got, err := agyAncestor(ctx, 10, lookup, pin, stat)
	if err != nil || got != refs[20] {
		t.Fatalf("ancestor = %+v, %v", got, err)
	}
	names[20] = "agy-similar"
	if _, err = agyAncestor(ctx, 10, lookup, pin, stat); err == nil {
		t.Fatal("similar process name accepted")
	}
	names[20] = "agy"
	foreign := refs[20]
	foreign.Principal = "other-owner"
	refs[20] = foreign
	if _, err = agyAncestor(ctx, 10, lookup, pin, stat); err == nil {
		t.Fatal("foreign owner accepted")
	}
	foreign.Principal = "owner"
	refs[20] = foreign
	failedPin := func(_ context.Context, ref platform.ProcessRef) error {
		if ref.PID == 20 {
			return errors.New("reused")
		}
		return nil
	}
	if _, err = agyAncestor(ctx, 10, lookup, failedPin, stat); err == nil {
		t.Fatal("reused process accepted")
	}
	names[20] = "shell"
	parents[20] = 10
	if _, err = agyAncestor(ctx, 10, lookup, pin, stat); err == nil {
		t.Fatal("ancestor cycle accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = agyAncestor(cancelled, 10, lookup, pin, stat); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestAGYAncestorDepthBound(t *testing.T) {
	lookups := 0
	lookup := func(_ context.Context, pid int) (platform.ProcessRef, error) {
		lookups++
		return platform.ProcessRef{PID: pid, Principal: "owner", Start: "100", BootID: "boot"}, nil
	}
	stat := func(_ context.Context, ref platform.ProcessRef) (string, int, error) {
		return "shell", ref.PID + 1, nil
	}
	if _, err := agyAncestor(context.Background(), 2, lookup, func(context.Context, platform.ProcessRef) error { return nil }, stat); err == nil || lookups != 32 {
		t.Fatalf("unbounded ancestor lookup: calls=%d err=%v", lookups, err)
	}
}

func TestAGYAncestorRevalidatesParentRelationship(t *testing.T) {
	ctx := context.Background()
	for _, fault := range []string{"reparented", "child-exit", "newer-parent", "boot-change", "bad-start"} {
		t.Run(fault, func(t *testing.T) {
			child := platform.ProcessRef{PID: 10, Principal: "owner", Start: "100", BootID: "boot"}
			parent := platform.ProcessRef{PID: 20, Principal: "owner", Start: "50", BootID: "boot"}
			parentLookedUp := false
			lookup := func(_ context.Context, pid int) (platform.ProcessRef, error) {
				if pid == child.PID {
					return child, nil
				}
				parentLookedUp = true
				switch fault {
				case "newer-parent":
					parent.Start = "101"
				case "boot-change":
					parent.BootID = "other-boot"
				case "bad-start":
					parent.Start = "invalid"
				}
				return parent, nil
			}
			pin := func(_ context.Context, ref platform.ProcessRef) error {
				if fault == "child-exit" && parentLookedUp && ref == child {
					return errors.New("gone")
				}
				return nil
			}
			stat := func(_ context.Context, ref platform.ProcessRef) (string, int, error) {
				if ref.PID == child.PID {
					if fault == "reparented" && parentLookedUp {
						return "shell", 30, nil
					}
					return "shell", parent.PID, nil
				}
				return "agy", 1, nil
			}
			if _, err := agyAncestor(ctx, child.PID, lookup, pin, stat); err == nil {
				t.Fatal("changed ancestry accepted")
			}
		})
	}
}

func TestAGYConversationClassifiesPositiveStoreAndSubagent(t *testing.T) {
	brain := t.TempDir()
	ctx := context.Background()
	check := func(want string) {
		t.Helper()
		got, err := classifyAGYConversation(ctx, brain, agyTestConversation)
		if err != nil || got != want {
			t.Fatalf("classification = %s, %v; want %s", got, err, want)
		}
	}
	check("unknown")
	if err := os.Mkdir(filepath.Join(brain, agyTestConversation), 0700); err != nil {
		t.Fatal(err)
	}
	check("top-level")
	subagents := filepath.Join(brain, "parent", ".system_generated", "subagents")
	if err := os.MkdirAll(subagents, 0700); err != nil {
		t.Fatal(err)
	}
	check("top-level")
	if err := os.WriteFile(filepath.Join(subagents, agyTestConversation+".json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	check("subagent")
}

func TestAGYConversationRefusesLinksAndUnsafeMetadata(t *testing.T) {
	for _, kind := range []string{"brain", "own", "generated", "child", "writable"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			brain := filepath.Join(base, "brain")
			if err := os.Mkdir(brain, 0700); err != nil {
				t.Fatal(err)
			}
			own := filepath.Join(brain, agyTestConversation)
			if kind == "own" {
				if err := os.Symlink(base, own); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(own, 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "brain":
				link := filepath.Join(base, "link")
				if err := os.Symlink(brain, link); err != nil {
					t.Fatal(err)
				}
				brain = link
			case "generated":
				if err := os.Symlink(base, filepath.Join(own, ".system_generated")); err != nil {
					t.Fatal(err)
				}
			case "child":
				dir := filepath.Join(own, ".system_generated", "subagents")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(base, "missing"), filepath.Join(dir, agyTestConversation+".json")); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err := os.Chmod(own, 0777); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := classifyAGYConversation(context.Background(), brain, agyTestConversation); err != nil || got != "unknown" {
				t.Fatalf("classification = %s, %v", got, err)
			}
		})
	}
}

func TestAGYConversationArtifactAndHomeSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	brain := filepath.Join(home, ".gemini", "antigravity-cli", "brain")
	if err := os.MkdirAll(filepath.Join(brain, agyTestConversation), 0700); err != nil {
		t.Fatal(err)
	}
	p := &paths{}
	for _, artifact := range []string{"", "relative/" + agyTestConversation, filepath.Join(home, "wrong")} {
		if got, err := p.AGYConversation(context.Background(), agyTestConversation, artifact); err != nil || got != "top-level" {
			t.Fatalf("home classification = %s, %v", got, err)
		}
	}
	other := t.TempDir()
	artifact := filepath.Join(other, agyTestConversation)
	if got, _ := p.AGYConversation(context.Background(), agyTestConversation, artifact); got != "unknown" {
		t.Fatal("artifact missing store accepted")
	}
	if err := os.Mkdir(artifact, 0700); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.AGYConversation(context.Background(), agyTestConversation, artifact); got != "top-level" {
		t.Fatal("artifact store not used")
	}
	for _, id := range []string{"", "../escape", strings.ToUpper(agyTestConversation[:8]) + "-BAD", "11111111-1111-4111-8111-11111111111/"} {
		if got, _ := p.AGYConversation(context.Background(), id, artifact); got != "unknown" {
			t.Fatal("invalid conversation ID accepted")
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.AGYConversation(cancelled, agyTestConversation, artifact); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestRemoveEndpointRefusesUnissuedOrUnsafeObjects(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "rkagy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	h, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	p := h.Paths.(*paths)
	ctx := context.Background()
	if err := p.RemoveEndpoint(ctx, platform.Endpoint("unissued")); err == nil {
		t.Fatal("unissued handle accepted")
	}
	provider, err := p.issue(endpoint{path: filepath.Join(root, "provider.sock")})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveEndpoint(ctx, provider); err == nil {
		t.Fatal("provider handle accepted")
	}
	for _, kind := range []string{"file", "link", "public-socket", "private-socket", "missing"} {
		t.Run(kind, func(t *testing.T) {
			name := kind + ".sock"
			handle, err := p.Endpoint(name)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, name)
			switch kind {
			case "file":
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "link":
				if err := os.Symlink(filepath.Join(root, "target"), path); err != nil {
					t.Fatal(err)
				}
			case "public-socket", "private-socket":
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				listener.SetUnlinkOnClose(false)
				if err := listener.Close(); err != nil {
					t.Fatal(err)
				}
				mode := os.FileMode(0600)
				if kind == "public-socket" {
					mode = 0666
				}
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
			}
			err = p.RemoveEndpoint(ctx, handle)
			if kind == "private-socket" || kind == "missing" {
				if err != nil {
					t.Fatalf("safe removal = %v", err)
				}
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal("socket retained")
				}
			} else {
				if err == nil {
					t.Fatal("unsafe object removed")
				}
				if _, err := os.Lstat(path); err != nil {
					t.Fatal("unsafe object changed")
				}
			}
		})
	}
}
