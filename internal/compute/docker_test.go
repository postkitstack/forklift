package compute

import "testing"

func TestPublishAddressDefaultsToLoopback(t *testing.T) {
	d := &Docker{}
	if got, want := d.publishAddress(15500), "127.0.0.1:15500:5432"; got != want {
		t.Fatalf("publishAddress: want %q, got %q", want, got)
	}
}

func TestPublishAddressSupportsExplicitLoopback(t *testing.T) {
	d := &Docker{BindHost: "::1"}
	if got, want := d.publishAddress(15500), "[::1]:15500:5432"; got != want {
		t.Fatalf("publishAddress: want %q, got %q", want, got)
	}
}
