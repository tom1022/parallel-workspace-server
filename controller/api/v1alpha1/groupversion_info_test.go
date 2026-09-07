package v1alpha1

import "testing"

// The API group must not carry the maintainer's private domain into the OSS
// package (fickledev.com is operated privately, not part of the public
// distribution).
func TestGroupVersionUsesPublicDomain(t *testing.T) {
	const want = "workspace.tom1022.github.io"
	if GroupVersion.Group != want {
		t.Errorf("GroupVersion.Group = %q, want %q", GroupVersion.Group, want)
	}
}
