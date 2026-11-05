//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/helper"
	"github.com/llcoolkm/rookery/go/platform"
	linux "github.com/llcoolkm/rookery/go/platform/linux"
	"github.com/llcoolkm/rookery/go/provider"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type arguments struct {
	command     string
	options     map[string]string
	positionals []string
}

func parse(args []string) (arguments, error) {
	a := arguments{options: map[string]string{}}
	bools := map[string]bool{"once": true, "peek": true, "allow-http-loopback": true, "apply": true, "accept-inbound": true, "dry-run": true, "detach": true, "receipt": true}
	values := map[string]bool{"state-dir": true, "as": true, "node": true, "url": true, "token-file": true, "provider": true, "session": true, "to": true, "body": true, "kind": true, "ttl": true, "id": true, "limit": true, "alias": true, "pid": true, "name": true, "backend": true, "executable": true, "user": true, "path": true, "timeout": true}
	for i := 0; i < len(args); i++ {
		s := args[i]
		if strings.HasPrefix(s, "--") {
			key, value, eq := strings.Cut(strings.TrimPrefix(s, "--"), "=")
			if bools[key] {
				if eq {
					return a, errors.New("boolean flag does not accept a value")
				}
				a.options[key] = "true"
			} else if values[key] {
				if !eq {
					i++
					if i >= len(args) {
						return a, errors.New("missing flag value")
					}
					value = args[i]
				}
				a.options[key] = value
			} else {
				return a, errors.New("unknown flag")
			}
		} else if a.command == "" {
			a.command = s
		} else {
			a.positionals = append(a.positionals, s)
		}
	}
	if a.command == "" {
		return a, errors.New("command required")
	}
	return a, nil
}
func (a arguments) pos(n int) string {
	if n < len(a.positionals) {
		return a.positionals[n]
	}
	return ""
}
func readJSON(ctx context.Context, h platform.Host, root, name string, out any) error {
	f, e := h.Files.OpenNoFollow(ctx, root, name, false)
	if e != nil {
		return e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if e != nil || len(b) > 1<<20 || json.Unmarshal(b, out) != nil {
		return errors.New("invalid private configuration")
	}
	return nil
}
func writeJSON(ctx context.Context, h platform.Host, root, name string, value any) error {
	b, e := json.Marshal(value)
	if e != nil {
		return errors.New("invalid private configuration")
	}
	return h.Files.AtomicWrite(ctx, root, name, b)
}
func runPlatform(ctx context.Context, args []string) (any, error) {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer stop()
	a, e := parse(args)
	if e != nil {
		return nil, e
	}
	state := a.options["state-dir"]
	if state != "" {
		state, e = filepath.Abs(state)
		if e != nil {
			return nil, errors.New("invalid state directory")
		}
	}
	h, e := linux.New(state)
	if e != nil {
		return nil, errors.New("state directory must be private (0700), owned by this user, and not a symlink")
	}
	root, _ := h.Paths.StateDir()
	if a.command == "service" {
		return serviceCommand(ctx, h, root, a)
	}
	if a.command == "configure" {
		tokenPath, e := filepath.Abs(a.options["token-file"])
		if e != nil || a.options["token-file"] == "" {
			return nil, errors.New("--token-file required")
		}
		f, e := h.Files.OpenNoFollow(ctx, filepath.Dir(tokenPath), filepath.Base(tokenPath), false)
		if e != nil {
			return nil, errors.New("token file must be a private regular file")
		}
		raw, e := io.ReadAll(io.LimitReader(f, 4098))
		f.Close()
		if e != nil || len(raw) > 4097 {
			return nil, errors.New("invalid token")
		}
		c := core.Config{Node: a.options["node"], URL: a.options["url"], Token: strings.TrimSpace(string(raw)), AllowHTTP: a.options["allow-http-loopback"] == "true"}
		if e = c.Validate(); e != nil {
			return nil, e
		}
		c.URL, _ = core.ValidateOrigin(c.URL, c.AllowHTTP)
		var old core.Config
		e = readJSON(ctx, h, root, "config.json", &old)
		if e == nil {
			if old.Node != c.Node || old.URL != c.URL {
				return nil, errors.New("use a fresh state directory to change hub or node")
			}
			c.BusyDelivery = old.BusyDelivery
		} else if !errors.Is(e, fs.ErrNotExist) {
			return nil, errors.New("invalid private configuration")
		}
		if e = writeJSON(ctx, h, root, "config.json", c); e != nil {
			return nil, errors.New("configuration write failed")
		}
		return map[string]any{"node": c.Node, "url": c.URL}, nil
	}
	if a.command == "install" {
		return installCommand(ctx, h, root, a)
	}
	store, e := core.NewLocalStore(ctx, h.Files, root)
	if e != nil {
		return nil, e
	}
	if a.command == "register-polling" || a.command == "unregister-polling" {
		p, id, ok := strings.Cut(a.pos(0), ":")
		if !ok {
			return nil, errors.New("polling target must be local provider:UUID")
		}
		if a.command == "unregister-polling" {
			return helper.UnregisterPolling(ctx, h, root, p, id)
		}
		pid, e := strconv.Atoi(a.options["pid"])
		if e != nil {
			return nil, errors.New("polling PID must be a positive integer")
		}
		return helper.RegisterPolling(ctx, h, root, p, id, pid, a.options["name"])
	}
	var config core.Config
	if e = readJSON(ctx, h, root, "config.json", &config); e != nil {
		return nil, errors.New("no valid helper configuration; run rookery configure first")
	}
	if e = config.Validate(); e != nil {
		return nil, e
	}
	if a.command == "busy-delivery" {
		if a.pos(0) != "claude" || a.pos(1) != "on" && a.pos(1) != "off" {
			return nil, errors.New("busy-delivery requires claude on or off")
		}
		config.BusyDelivery = []string{}
		if a.pos(1) == "on" {
			config.BusyDelivery = []string{"claude"}
		}
		if e = writeJSON(ctx, h, root, "config.json", config); e != nil {
			return nil, errors.New("configuration write failed")
		}
		return map[string]any{"busy_delivery": config.BusyDelivery, "restart_helper": true, "detail": "takes effect, on or off, only after the helper restarts; until then the old policy applies"}, nil
	}
	registry := helper.NewRegistry(provider.Config{BusyDelivery: config.BusyDelivery}, h)
	actor := a.options["as"]
	if actor == "" {
		actor = os.Getenv("ROOKERY_SESSION")
	}
	if actor == "operator" {
		actor = ""
	}
	client := &core.Client{Store: store, Config: config, Actor: actor, Timeout: 5 * time.Second}
	if client.Actor != "" {
		if _, e := client.Identity(ctx); e != nil {
			return nil, e
		}
	}
	if a.command == "daemon" {
		hub, e := core.NewHTTPHub(config.URL, config.Token, config.AllowHTTP)
		if e != nil {
			return nil, e
		}
		busy := config.BusyDelivery
		if busy == nil {
			busy = []string{"claude"}
		}
		daemon := core.Helper{Process: h.Process, Store: store, Hub: hub, Node: config.Node, Native: registry.NativeHooks(), BusyDelivery: busy}
		if e = daemon.Run(ctx, a.options["once"] == "true"); e != nil {
			return nil, e
		}
		if a.options["once"] == "true" {
			return map[string]any{"cycle": "complete"}, nil
		}
		return map[string]any{"stopped": true}, nil
	}
	if a.command == "mcp" {
		if a.options["provider"] != "" && (a.options["session"] != "" || client.Actor != "") {
			return nil, errors.New("--provider cannot be combined with --session, --as or ROOKERY_SESSION")
		}
		if a.options["session"] != "" {
			client.Actor = a.options["session"]
		}
		if a.options["provider"] == "" {
			if strings.TrimSpace(client.Actor) == "" {
				return nil, errors.New("MCP requires --session, ROOKERY_SESSION or --provider")
			}
			if _, e := client.Identity(ctx); e != nil {
				return nil, e
			}
		}
		var resolver core.MCPResolver
		if name := a.options["provider"]; name != "" {
			p := registry.Provider(name)
			if p == nil {
				return nil, errors.New("provider unavailable")
			}
			resolver = func(ctx context.Context, params map[string]any) (core.MCPBackend, error) {
				meta := core.CallMetadata{}
				if raw, ok := params["_meta"].(map[string]any); ok {
					for k, v := range raw {
						meta[k], _ = json.Marshal(v)
					}
				}
				identity, e := p.Identity(ctx, meta)
				if e != nil {
					return nil, core.InvalidParams(e.Error())
				}
				bound := *client
				bound.Actor = identity
				return mcpClient{&bound}, nil
			}
		}
		return nil, core.NewMCP(mcpClient{client}, resolver).Serve(ctx, os.Stdin, os.Stdout)
	}
	params := map[string]any{}
	for _, k := range []string{"to", "body", "kind", "id", "alias"} {
		if v, ok := a.options[k]; ok {
			params[k] = v
		}
	}
	for _, k := range []string{"ttl", "limit"} {
		if v, ok := a.options[k]; ok {
			n, e := strconv.Atoi(v)
			if e != nil {
				return nil, errors.New("invalid integer flag")
			}
			params[k] = n
		}
	}
	if a.options["peek"] == "true" {
		params["peek"] = true
	}
	switch a.command {
	case "send", "reply":
		if _, ok := params["body"]; !ok {
			return nil, errors.New("--body required")
		}
		if params["body"] == "-" {
			b, e := io.ReadAll(io.LimitReader(os.Stdin, core.MaxBodyBytes+1))
			if e != nil || len(b) > core.MaxBodyBytes {
				return nil, errors.New("body must contain text and be at most 16384 UTF-8 bytes")
			}
			params["body"] = string(b)
		}
		if a.command == "reply" {
			params["message_id"] = a.pos(0)
		} else if params["to"] == nil {
			return nil, errors.New("--to required")
		}
	case "ack", "status":
		params["message_id"] = a.pos(0)
	case "enroll", "disable", "clear-alias":
		if a.pos(0) == "" {
			return nil, errors.New("target required")
		}
		params["target"] = a.pos(0)
	case "inbox", "list", "health":
	case "doctor":
		health, e := client.Invoke(ctx, "health", nil)
		if e != nil {
			return nil, e
		}
		out := health.(map[string]any)
		configured := config.BusyDelivery
		if configured == nil {
			configured = []string{"claude"}
		}
		out["busy_delivery"] = map[string]any{"configured": configured, "effective": out["busy_delivery"], "pending_restart": out["running"] != true || !samePolicy(out["busy_delivery"], configured)}
		out["attachments"] = []any{}
		return out, nil
	case "wait":
		return waitCommand(ctx, client, a)
	default:
		return nil, errors.New("unknown command or deferred provider operation")
	}
	return client.Invoke(ctx, a.command, params)
}

type mcpClient struct{ *core.Client }

func (c mcpClient) Invoke(ctx context.Context, op string, args map[string]any) (any, error) {
	v, e := c.Client.Invoke(ctx, op, args)
	if e != nil {
		return nil, core.OperationError(e.Error())
	}
	return v, nil
}
func (c mcpClient) Identity(ctx context.Context) (string, error) {
	v, e := c.Client.Identity(ctx)
	if e != nil {
		return "", core.InvalidParams(e.Error())
	}
	return v, nil
}
func serviceCommand(ctx context.Context, h platform.Host, root string, a arguments) (any, error) {
	s := h.Service
	switch a.pos(0) {
	case "install":
		exe := a.options["executable"]
		if exe == "" {
			exe, _ = os.Executable()
		}
		spec := platform.ServiceSpec{Executable: exe, StateDir: root, Account: a.options["user"], SearchPath: a.options["path"]}
		if b, ok := s.(interface{ ConfigureBackend(string) error }); ok {
			backend := a.options["backend"]
			if backend == "" {
				backend = "auto"
			}
			if e := b.ConfigureBackend(backend); e != nil {
				return nil, e
			}
		}
		if a.options["dry-run"] == "true" {
			if planner, ok := s.(interface {
				Plan(context.Context, platform.ServiceSpec) (any, error)
			}); ok {
				return planner.Plan(ctx, spec)
			}
			return nil, errors.New("service installation plan unavailable")
		}
		if e := s.Install(ctx, spec); e != nil {
			return nil, errors.New("service installation failed")
		}
		return map[string]any{"installed": true, "running": false}, nil
	case "uninstall":
		return map[string]any{"uninstalled": true}, s.Uninstall(ctx)
	case "status":
		return s.Status(ctx)
	case "start":
		if x, ok := s.(interface{ Start(context.Context) error }); ok {
			return map[string]any{"started": true}, x.Start(ctx)
		}
	case "stop":
		if x, ok := s.(interface{ Stop(context.Context) error }); ok {
			return map[string]any{"stopped": true}, x.Stop(ctx)
		}
	case "supervise":
		if a.options["detach"] == "true" {
			if x, ok := s.(interface{ Start(context.Context) error }); ok {
				return nil, x.Start(ctx)
			}
		}
		if x, ok := s.(interface{ Supervise(context.Context) error }); ok {
			return nil, x.Supervise(ctx)
		}
	}
	return nil, errors.New("invalid service action")
}
func waitCommand(ctx context.Context, c *core.Client, a arguments) (any, error) {
	timeout := 60.0
	if v := a.options["timeout"]; v != "" {
		n, e := strconv.ParseFloat(v, 64)
		if e != nil || n < 0 || n > 86400 {
			return nil, errors.New("invalid wait timeout")
		}
		timeout = n
	}
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	for time.Now().Before(deadline) {
		v, e := c.Invoke(ctx, "status", map[string]any{"message_id": a.pos(0)})
		if e != nil {
			return nil, e
		}
		m := v.(map[string]any)
		if id, ok := m["reply_id"].(string); ok && id != "" {
			r, e := c.Invoke(ctx, "status", map[string]any{"message_id": id})
			return map[string]any{"message": m, "reply": r}, e
		}
		s, _ := m["status"].(string)
		if a.options["receipt"] == "true" && (s == "acknowledged" || s == "replied") {
			return m, nil
		}
		if s == "failed" || s == "uncertain" || s == "expired" || s == "cancelled" {
			return nil, errors.New("message " + s)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil, errors.New("wait timed out; message was not resent")
}

func samePolicy(value any, configured []string) bool {
	b, _ := json.Marshal(value)
	var effective []string
	if json.Unmarshal(b, &effective) != nil || len(effective) != len(configured) {
		return false
	}
	set := map[string]bool{}
	for _, name := range effective {
		set[name] = true
	}
	for _, name := range configured {
		if !set[name] {
			return false
		}
	}
	return true
}
