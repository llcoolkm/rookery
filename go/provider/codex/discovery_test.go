package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"testing"

	"github.com/llcoolkm/rookery/go/platform"
	"github.com/llcoolkm/rookery/go/provider"
)

type refusedControl struct {
	platform.Paths
	err error
}

func (p refusedControl) ControlEndpoint(context.Context, string) (platform.Endpoint, error) {
	return "", p.err
}

type refusedIPC struct {
	platform.IPC
	err error
}

func (p refusedIPC) Dial(context.Context, platform.Endpoint) (platform.Connection, error) {
	return nil, p.err
}

func TestDiscoveryFailureLogRedactsResolverAndDialErrors(t *testing.T) {
	var output bytes.Buffer
	oldWriter, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	defer func() { log.SetOutput(oldWriter); log.SetFlags(oldFlags); log.SetPrefix(oldPrefix) }()
	log.SetOutput(&output)
	log.SetFlags(0)
	log.SetPrefix("")
	privateError := "synthetic private path/session/token\nforged"
	for _, stage := range []string{"resolve", "dial"} {
		for _, unsafe := range []bool{false, true} {
			output.Reset()
			err := errors.New(privateError)
			wantReason := "discovery_unavailable"
			if unsafe {
				err = fmt.Errorf("%s: %w", privateError, platform.ErrControlEndpointUnsafe)
				wantReason = "control_endpoint_unsafe"
			}
			host := platform.Host{Paths: refusedControl{err: err}}
			options := Options{}
			if stage == "dial" {
				host.IPC = refusedIPC{err: err}
				options.Endpoint = "issued"
			}
			adapter, _ := NewWithOptions(provider.Config{}, host, options)
			rows, gotErr := adapter.Discover(context.Background())
			if rows != nil || !errors.Is(gotErr, errDiscovery) || gotErr.Error() != errDiscovery.Error() {
				t.Fatal("discovery did not return a redacted failure")
			}
			want := "provider discovery failed provider=codex reason=" + wantReason + "\n"
			if output.String() != want {
				t.Fatalf("unexpected discovery log: %q", output.String())
			}
		}
	}
	output.Reset()
	logDiscovery(&discoveryError{reason: privateError})
	if output.String() != "provider discovery failed provider=codex reason=discovery_unavailable\n" {
		t.Fatal("discovery log accepted an unknown reason")
	}
}

func TestSuccessfulOrExcludedDiscoveryDoesNotLogFailure(t *testing.T) {
	var output bytes.Buffer
	oldWriter := log.Writer()
	defer log.SetOutput(oldWriter)
	log.SetOutput(&output)
	fixture(t)
	adapter, _ := New(provider.Config{SessionAllowlist: []string{}}, platform.Host{})
	if _, err := adapter.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatal("successful discovery logged a failure")
	}
}
