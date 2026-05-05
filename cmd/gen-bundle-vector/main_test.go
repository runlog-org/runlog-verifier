package main

import (
	"bytes"
	"testing"
)

// TestGenerateVectorDeterministic asserts the canonical bundle bytes,
// signature, and public key are stable across repeated runs. If this test
// ever fails, the frozen vector consumed by runlog/server/tests/fixtures/
// will need to be regenerated — but more likely, something in
// internal/sign drifted in a way that breaks cross-language verification.
func TestGenerateVectorDeterministic(t *testing.T) {
	canonical1, sig1, pub1, err := generateVector()
	if err != nil {
		t.Fatalf("first generateVector: %v", err)
	}
	canonical2, sig2, pub2, err := generateVector()
	if err != nil {
		t.Fatalf("second generateVector: %v", err)
	}

	if !bytes.Equal(canonical1, canonical2) {
		t.Errorf("canonical bytes differ across runs:\n  run1=%q\n  run2=%q",
			canonical1, canonical2)
	}
	if sig1 != sig2 {
		t.Errorf("signature differs across runs:\n  run1=%s\n  run2=%s", sig1, sig2)
	}
	if pub1 != pub2 {
		t.Errorf("public key differs across runs:\n  run1=%s\n  run2=%s", pub1, pub2)
	}
}
