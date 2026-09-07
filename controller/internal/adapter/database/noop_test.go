package database

import "testing"

// TestNoopAdapter_EnsureAndReleaseDoNothing locks in Requirement 3.5's
// disabled path at the adapter level: no client is ever touched, so a
// deployment without a database backend can wire this in without a CNPG
// operator installed at all.
func TestNoopAdapter_EnsureAndReleaseDoNothing(t *testing.T) {
	a := NoopAdapter{}
	target := DatabaseTarget{WorkspaceName: "ws", Namespace: "ns", Name: "ws-abcd", ClusterRef: "some-cluster"}

	ref, err := a.Ensure(nil, target)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if ref != (DatabaseRef{}) {
		t.Errorf("Ensure returned %+v, want the zero DatabaseRef", ref)
	}
	if err := a.Release(nil, target); err != nil {
		t.Fatalf("Release: %v", err)
	}
}
