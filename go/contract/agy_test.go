package contract_test

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/llcoolkm/rookery/go/core"
)

func TestAGYIdentityVectors(t *testing.T) {
	b, err := os.ReadFile("testdata/contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name   string                     `json:"name"`
			Params map[string]json.RawMessage `json:"params"`
			Actor  string                     `json:"actor"`
			Error  string                     `json:"error"`
		} `json:"agy_identity"`
	}
	if json.Unmarshal(b, &fixture) != nil || len(fixture.Cases) < 10 {
		t.Fatal("missing AGY identity vectors")
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var meta core.CallMetadata
			if json.Unmarshal(c.Params["_meta"], &meta) != nil {
				meta = nil
			}
			actor, err := core.AGYMetadataIdentity(meta)
			if c.Error == "" {
				if err != nil || actor != c.Actor {
					t.Fatal(actor, err)
				}
				return
			}
			var invalid core.InvalidParams
			if err == nil || err.Error() != c.Error || !errors.As(err, &invalid) {
				t.Fatal(actor, err)
			}
		})
	}
}
