//go:build linux

package linux

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

type brokenDatabase struct{}

func (brokenDatabase) Read([]byte) (int, error) { return 0, errors.New("synthetic read failure") }

func TestPrivateGroupDatabase(t *testing.T) {
	const passwd = "example:x:1001:1001::/example:/bin/false\nother:x:1002:1002::/other:/bin/false\n"
	for _, tc := range []struct {
		name, passwd, group string
		want                bool
	}{
		{"private named", passwd, "example:x:1001:\n", true},
		{"private explicit", passwd, "example:x:1001:example\n", true},
		{"private differently named", passwd, "private:x:1001:example\n", true},
		{"another member", passwd, "example:x:1001:example,other\n", false},
		{"another primary", strings.Replace(passwd, "1002:1002", "1002:1001", 1), "example:x:1001:\n", false},
		{"not primary", passwd, "example:x:1002:\n", false},
		{"missing group", passwd, "", false},
		{"malformed passwd", passwd + "bad\n", "example:x:1001:\n", false},
		{"malformed group", passwd, "example:x:1001:\nbad\n", false},
		{"bad uid", strings.Replace(passwd, "1002:1002", "invalid:1002", 1), "example:x:1001:\n", false},
		{"bad gid", passwd, "example:x:-1:\n", false},
		{"bad name", passwd, "bad name:x:1001:\n", false},
		{"bad member", passwd, "example:x:1001:example\t\n", false},
		{"empty member", passwd, "example:x:1001:example,\n", false},
		{"duplicate uid", passwd + "duplicate:x:1001:1003::/:/bin/false\n", "example:x:1001:\n", false},
		{"duplicate gid", passwd, "example:x:1001:\nsecond:x:1001:\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := privateGroupFrom(strings.NewReader(tc.passwd), strings.NewReader(tc.group), 1001, 1001); got != tc.want {
				t.Fatalf("private group = %v, want %v", got, tc.want)
			}
		})
	}
	if privateGroupFrom(brokenDatabase{}, strings.NewReader("example:x:1001:\n"), 1001, 1001) || privateGroupFrom(strings.NewReader(passwd), brokenDatabase{}, 1001, 1001) {
		t.Fatal("accepted unreadable database")
	}
	if privateGroupFrom(strings.NewReader(passwd), strings.NewReader(strings.Repeat("x", 65537)), 1001, 1001) {
		t.Fatal("accepted oversized database line")
	}
}

func TestStrictModesPrivateGroup(t *testing.T) {
	for _, directory := range []bool{false, true} {
		mode := uint32(0664)
		if directory {
			mode = 0775
		}
		st := syscall.Stat_t{Uid: uint32(os.Geteuid()), Gid: 1001, Mode: mode}
		private := func(gid uint32) bool { return gid == 1001 }
		if !safeStatWithGroup(&st, false, directory, private) {
			t.Fatal("rejected private-group mode")
		}
		if safeStatWithGroup(&st, true, directory, private) {
			t.Fatal("relaxed Rookery private mode")
		}
		if safeStatWithGroup(&st, false, directory, func(uint32) bool { return false }) {
			t.Fatal("accepted shared or unverified group")
		}
		st.Mode |= 0002
		if safeStatWithGroup(&st, false, directory, private) {
			t.Fatal("accepted other-writable mode")
		}
		st.Mode &^= 0002
		st.Uid = uint32(os.Geteuid()) + 1
		if safeStatWithGroup(&st, false, directory, private) {
			t.Fatal("accepted foreign-owned group writable object")
		}
	}
	st := syscall.Stat_t{Uid: rootUID, Mode: 01777}
	if !safeStatWithGroup(&st, false, true, func(uint32) bool { return false }) {
		t.Fatal("changed root sticky rule")
	}
}
