package core

import (
	"bytes"
	"log"
	"testing"
)

func TestHeldLogOnlyAcceptsRedactedCodes(t *testing.T) {
	var output bytes.Buffer
	oldWriter, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	defer func() { log.SetOutput(oldWriter); log.SetFlags(oldFlags); log.SetPrefix(oldPrefix) }()
	log.SetOutput(&output)
	log.SetFlags(0)
	log.SetPrefix("")
	logHeld("record_unsafe")
	logHeld("pin_mismatch")
	logHeld("not_idle")
	logHeld("synthetic private path/session/token\nforged")
	want := "native delivery held reason=record_unsafe\nnative delivery held reason=pin_mismatch\nnative delivery held reason=not_idle\nnative delivery held reason=not_ready\n"
	if output.String() != want {
		t.Fatalf("unexpected local log: %q", output.String())
	}
}
