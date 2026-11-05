// Package helper assembles the portable provider registry and explicit polling.
package helper

import (
	"context"
	"sort"

	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/platform"
	"github.com/llcoolkm/rookery/go/provider"
	"github.com/llcoolkm/rookery/go/provider/agy"
	"github.com/llcoolkm/rookery/go/provider/claude"
	"github.com/llcoolkm/rookery/go/provider/codex"
)

type Registry struct {
	Config    provider.Config
	Host      platform.Host
	Providers map[string]provider.Provider
}

func NewRegistry(c provider.Config, h platform.Host) *Registry {
	co, _ := codex.New(c, h)
	cl, _ := claude.New(c, h)
	ag, _ := agy.New(c, h)
	return &Registry{Config: c, Host: h, Providers: map[string]provider.Provider{"codex": co, "claude": cl, "agy": ag}}
}
func (r *Registry) Provider(name string) provider.Provider { return r.Providers[name] }
func (r *Registry) Discover(ctx context.Context) ([]core.Session, map[string]string) {
	sessions := []core.Session{}
	problems := map[string]string{}
	names := make([]string, 0, len(r.Providers))
	for name := range r.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !r.Config.AllowsProvider(name) {
			continue
		}
		rows, err := r.Providers[name].Discover(ctx)
		if err != nil {
			problems[name] = "provider discovery unavailable"
			continue
		}
		for _, s := range rows {
			if s.Provider != name || !r.Config.Allows(name, s.ID) {
				continue
			}
			if _, err := core.RequestCreated(s.ID); err != nil {
				continue
			}
			s.Admission = string(r.Providers[name].Admission(s))
			if s.Admission != "none" && s.Admission != "idle" && s.Admission != "busy" {
				s.Admission = "none"
			}
			sessions = append(sessions, s)
		}
	}
	counts := map[string]int{}
	for _, s := range sessions {
		counts[s.Provider+":"+s.ID]++
	}
	merged := map[string]core.Session{}
	for _, s := range sessions {
		key := s.Provider + ":" + s.ID
		if counts[key] == 1 {
			merged[key] = s
		} else {
			problems[s.Provider] = "ambiguous live session identity"
		}
	}
	root, err := r.Host.Paths.StateDir()
	if err == nil {
		polling, e := discoverPolling(ctx, r.Host, root, r.Config)
		if e != nil {
			problems["polling"] = "polling discovery unavailable"
		} else {
			for _, s := range polling {
				key := s.Provider + ":" + s.ID
				if current, ok := merged[key]; !ok || current.Delivery != "attached" {
					merged[key] = s
				}
			}
		}
	} else {
		problems["polling"] = "polling discovery unavailable"
	}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]core.Session, 0, len(keys))
	for _, key := range keys {
		result = append(result, merged[key])
	}
	return result, problems
}
func (r *Registry) Ready(s core.Session) bool {
	p := r.Provider(s.Provider)
	if p == nil || s.Delivery == "polling" {
		return false
	}
	policy := p.Admission(s)
	return policy == provider.AdmitBusy && (s.Status == "idle" || s.Status == "busy" || s.Provider == "agy" && s.Delivery == "attached") || policy == provider.AdmitIdle && s.Status == "idle"
}
func (r *Registry) Submit(ctx context.Context, s core.Session, m core.Message) (core.DeliveryResult, error) {
	p := r.Provider(s.Provider)
	if p == nil {
		return core.DeliveryResult{Status: "held", Detail: "unknown provider; no input submitted"}, nil
	}
	if !r.Ready(s) {
		return core.DeliveryResult{Status: "held", Detail: "recipient not ready; no input submitted"}, nil
	}
	result, err := p.Deliver(ctx, s, m)
	switch result.Status {
	case "held", "submitted", "failed", "uncertain":
	default:
		result = core.DeliveryResult{Status: "uncertain", Detail: "unrecognized provider result"}
	}
	return result, err
}
func (r *Registry) NativeHooks() core.NativeHooks {
	return core.NativeHooks{Discover: r.Discover, Ready: r.Ready, Submit: r.Submit}
}
