//go:build windows

package integration_test

import "testing"

func TestPythonGoHelpers(t *testing.T) {
	t.Skip("phase 2 integration requires the Linux platform; native Windows helper acceptance is phase 3")
}
