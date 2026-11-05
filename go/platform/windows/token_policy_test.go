//go:build windows

package windows

import (
	"errors"
	"testing"

	"github.com/llcoolkm/rookery/go/platform"

	win "golang.org/x/sys/windows"
)

// Exercise elevation independently of socket ownership, including on elevated
// CI runners; the refusal must identify the token elevation check.
func TestTokenElevationPolicy(t *testing.T) {
	current, err := win.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if err := tokenAllowed(current, current, false); err != nil {
		t.Fatal("same-token baseline refused", err)
	}
	if err := tokenAllowed(current, current, true); current.IsElevated() {
		if !errors.Is(err, platform.ErrControlEndpointElevated) {
			t.Fatal("expected control_endpoint_elevated")
		}
	} else if err != nil {
		t.Fatal("non-elevated token refused")
	}
	if current.IsElevated() {
		// When UAC supplies a linked limited token, verify both directions:
		// either elevated endpoint must refuse, and two limited peers pass.
		limited, err := current.GetLinkedToken()
		if err != nil {
			return // UAC-disabled hosts have no linked token.
		}
		defer limited.Close()
		if limited.IsElevated() {
			t.Fatal("linked token is elevated")
		}
		if err := tokenAllowed(limited, limited, true); err != nil {
			t.Fatal("limited-token peer refused", err)
		}
		if err := tokenAllowed(current, limited, true); !errors.Is(err, platform.ErrControlEndpointElevated) {
			t.Fatal("elevated peer accepted")
		}
		if err := tokenAllowed(limited, current, true); err == nil {
			t.Fatal("elevated current token accepted")
		}
	}
}
