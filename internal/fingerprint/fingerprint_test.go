package fingerprint

import "testing"

func TestCaptureNonEmpty(t *testing.T) {
	p := Capture()

	if p.OS == "" {
		t.Errorf("expected non-empty OS")
	}
	if p.Arch == "" {
		t.Errorf("expected non-empty Arch")
	}
	if p.GoVersion == "" {
		t.Errorf("expected non-empty GoVersion")
	}
	if p.CapturedAt == "" {
		t.Errorf("expected non-empty CapturedAt")
	}
}
