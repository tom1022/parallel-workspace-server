package controller

import (
	"strings"
	"testing"
)

func notTaken(string) bool { return false }

func TestDeriveResourceName_SanitizesToDNSLabel(t *testing.T) {
	cases := []struct {
		branch string
		want   string
	}{
		{"feature/foo", "feature-foo"},
		{"Feature/Foo", "feature-foo"},
		{"feature/foo_bar.baz", "feature-foo-bar-baz"},
		{"--leading-and-trailing--", "leading-and-trailing"},
		{"UPPER_CASE", "upper-case"},
	}
	for _, c := range cases {
		got, err := DeriveResourceName(c.branch, notTaken)
		if err != nil {
			t.Fatalf("DeriveResourceName(%q) error: %v", c.branch, err)
		}
		if got != c.want {
			t.Errorf("DeriveResourceName(%q) = %q, want %q", c.branch, got, c.want)
		}
	}
}

func TestDeriveResourceName_IsValidDNSLabel(t *testing.T) {
	long := strings.Repeat("a", 200)
	got, err := DeriveResourceName(long, notTaken)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) > 63 {
		t.Errorf("name %q exceeds 63 chars (%d)", got, len(got))
	}
	if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
		t.Errorf("name %q must not start or end with '-'", got)
	}
}

func TestDeriveResourceName_EmptyAfterSanitizationFallsBack(t *testing.T) {
	got, err := DeriveResourceName("___", notTaken)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "" {
		t.Fatal("expected a non-empty fallback name")
	}
}

func TestDeriveResourceName_IsDeterministic(t *testing.T) {
	a, err := DeriveResourceName("feature/foo", notTaken)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DeriveResourceName("feature/foo", notTaken)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("DeriveResourceName is not deterministic: %q != %q", a, b)
	}
}

func TestDeriveResourceName_AppendsHashSuffixOnCollision(t *testing.T) {
	taken := func(name string) bool { return name == "feature-foo" }

	got, err := DeriveResourceName("feature/foo", taken)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "feature-foo" {
		t.Fatalf("expected a disambiguating suffix when base name is taken, got %q", got)
	}
	if !strings.HasPrefix(got, "feature-foo-") {
		t.Errorf("expected suffixed name to retain base prefix, got %q", got)
	}

	// Deterministic: colliding again with the same branch produces the same suffix.
	got2, err := DeriveResourceName("feature/foo", taken)
	if err != nil {
		t.Fatal(err)
	}
	if got != got2 {
		t.Errorf("hash suffix is not deterministic: %q != %q", got, got2)
	}
}

func TestDeriveResourceName_DifferentBranchesCollidingOnBaseGetDifferentNames(t *testing.T) {
	// "Feature_Foo" and "feature/foo" both normalize to base "feature-foo".
	claimed := map[string]bool{"feature-foo": true}
	taken := func(name string) bool { return claimed[name] }

	first, err := DeriveResourceName("feature/foo", taken)
	if err != nil {
		t.Fatal(err)
	}
	claimed[first] = true

	second, err := DeriveResourceName("Feature_Foo", taken)
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Errorf("expected distinct names for colliding branches, both got %q", first)
	}
}

func TestDeriveResourceName_ReturnsErrorWhenUnresolvable(t *testing.T) {
	alwaysTaken := func(string) bool { return true }
	if _, err := DeriveResourceName("feature/foo", alwaysTaken); err == nil {
		t.Fatal("expected an error when no candidate name is free")
	}
}

// Branches longer than a DNS label share their truncated base, so the
// disambiguating suffix has to fit inside the label rather than extend past it.
func TestDeriveResourceName_CollisionSuffixKeepsAValidDNSLabel(t *testing.T) {
	base := strings.Repeat("a", 200)
	claimed := map[string]bool{}
	taken := func(name string) bool { return claimed[name] }

	first, err := DeriveResourceName(base, taken)
	if err != nil {
		t.Fatal(err)
	}
	claimed[first] = true

	second, err := DeriveResourceName(base+"/tail", taken)
	if err != nil {
		t.Fatal(err)
	}
	claimed[second] = true
	third, err := DeriveResourceName(base+"/other", taken)
	if err != nil {
		t.Fatal(err)
	}

	for _, got := range []string{second, third} {
		if len(got) > 63 {
			t.Errorf("name %q exceeds 63 chars (%d)", got, len(got))
		}
		if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
			t.Errorf("name %q must not start or end with '-'", got)
		}
		if got == first {
			t.Errorf("name %q collides with the already claimed %q", got, first)
		}
	}
	if second == third {
		t.Errorf("distinct branches sharing a truncated base both got %q", second)
	}
}
