package sign

import "testing"

func TestSignVerifyRoundtrip(t *testing.T) {
	pub, priv, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	_ = pub

	b := Bundle{
		UnitID:      "test-unit",
		Status:      "ok-stub",
		Fingerprint: map[string]string{"os": "linux"},
	}
	s, err := Sign(b, priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	ok, err := Verify(s)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatalf("signature did not verify")
	}
}

func TestVerifyFailsOnTamper(t *testing.T) {
	_, priv, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}

	b := Bundle{
		UnitID:      "test-unit",
		Status:      "ok-stub",
		Fingerprint: map[string]string{"os": "linux"},
	}
	s, err := Sign(b, priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	// Tamper: change the unit_id after signing.
	s.Bundle.UnitID = "tampered-unit"

	ok, err := Verify(s)
	if err != nil {
		t.Fatalf("verify returned error on tampered bundle: %v", err)
	}
	if ok {
		t.Fatalf("expected verification to fail on tampered bundle, but it succeeded")
	}
}
