//go:build linux

package linux

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/llcoolkm/rookery/go/platform"
)

type processes struct{}

func processStat(pid int) ([]string, error) {
	if pid <= 0 {
		return nil, errUnsafe
	}
	data, e := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if e != nil {
		return nil, errors.New("process unavailable")
	}
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 {
		return nil, errUnsafe
	}
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 20 || !strings.Contains("RSDI", fields[0]) {
		return nil, errors.New("process unavailable")
	}
	return fields, nil
}
func (p *processes) Current(ctx context.Context) (platform.ProcessRef, error) {
	return p.Lookup(ctx, os.Getpid())
}
func (p *processes) Lookup(ctx context.Context, pid int) (platform.ProcessRef, error) {
	if ctx.Err() != nil {
		return platform.ProcessRef{}, ctx.Err()
	}
	fields, e := processStat(pid)
	if e != nil {
		return platform.ProcessRef{}, e
	}
	info, e := os.Stat("/proc/" + strconv.Itoa(pid))
	if e != nil {
		return platform.ProcessRef{}, errors.New("process unavailable")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		return platform.ProcessRef{}, errUnsafe
	}
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil || len(strings.TrimSpace(string(boot))) == 0 {
		return platform.ProcessRef{}, errors.New("process unavailable")
	}
	again, e := processStat(pid)
	if e != nil || again[19] != fields[19] {
		return platform.ProcessRef{}, errors.New("process identity changed")
	}
	return platform.ProcessRef{PID: pid, Principal: strconv.FormatUint(uint64(st.Uid), 10), Start: fields[19], BootID: strings.TrimSpace(string(boot))}, nil
}
func (p *processes) Pin(ctx context.Context, ref platform.ProcessRef) error {
	live, e := p.Lookup(ctx, ref.PID)
	if e != nil {
		return e
	}
	if live != ref {
		return errors.New("process identity changed")
	}
	return nil
}
func (p *processes) Ancestors(ctx context.Context, ref platform.ProcessRef) ([]platform.ProcessRef, error) {
	var out []platform.ProcessRef
	seen := map[int]bool{}
	for i := 0; i < 1024; i++ {
		if seen[ref.PID] {
			return nil, errUnsafe
		}
		seen[ref.PID] = true
		if e := p.Pin(ctx, ref); e != nil {
			return nil, e
		}
		out = append(out, ref)
		fields, e := processStat(ref.PID)
		if e != nil {
			return nil, e
		}
		parent, e := strconv.Atoi(fields[1])
		if e != nil {
			return nil, errUnsafe
		}
		if parent == 0 || parent == ref.PID {
			return out, nil
		}
		next, e := p.Lookup(ctx, parent)
		if e != nil {
			return out, nil
		}
		if e = p.Pin(ctx, ref); e != nil {
			return nil, e
		}
		ref = next
	}
	return nil, errUnsafe
}
func (p *processes) CommandLine(ctx context.Context, ref platform.ProcessRef) ([]string, error) {
	if e := p.Pin(ctx, ref); e != nil {
		return nil, e
	}
	data, e := os.ReadFile("/proc/" + strconv.Itoa(ref.PID) + "/cmdline")
	if e != nil {
		return nil, errors.New("process unavailable")
	}
	if e = p.Pin(ctx, ref); e != nil {
		return nil, e
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00"), nil
}
