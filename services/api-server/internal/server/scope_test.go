package server

import "testing"

func TestScopedNamespace(t *testing.T) {
	if got := scopedNamespace("", "ns"); got != "ns" {
		t.Fatalf("expected ns, got %q", got)
	}
	if got := scopedNamespace("proj_a", "ns"); got != "proj_a:ns" {
		t.Fatalf("expected proj_a:ns, got %q", got)
	}
}

func TestUnscopedNamespace(t *testing.T) {
	if got := unscopedNamespace("", "ns"); got != "ns" {
		t.Fatalf("expected ns, got %q", got)
	}
	if got := unscopedNamespace("proj_a", "proj_a:ns"); got != "ns" {
		t.Fatalf("expected ns, got %q", got)
	}
	if got := unscopedNamespace("proj_a", "proj_b:ns"); got != "proj_b:ns" {
		t.Fatalf("expected unchanged namespace, got %q", got)
	}
}
