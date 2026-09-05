package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

const (
	dnsLabelMaxLength = 63
	hashSuffixLength  = 8 // hex chars, i.e. 32 bits — collision-resistant enough for a handful of concurrently colliding branch names
)

var invalidDNSLabelChars = regexp.MustCompile(`[^a-z0-9-]+`)

// DeriveResourceName normalizes branch into a DNS-1123 label suitable for
// Kubernetes object and hostname use (design.md 374行, Requirement 11.2). When
// the normalized name collides with one already in use (as reported by taken),
// a deterministic hash suffix derived from branch is appended so different
// branches that normalize to the same label still get distinct, stable names.
func DeriveResourceName(branch string, taken func(name string) bool) (string, error) {
	base := sanitizeDNSLabel(branch)
	if base == "" {
		base = "ws"
	}
	if !taken(base) {
		return base, nil
	}

	suffix := "-" + shortHash(branch)
	truncated := truncateDNSLabel(base, dnsLabelMaxLength-len(suffix))
	candidate := truncated + suffix
	if taken(candidate) {
		return "", fmt.Errorf("devplatform: could not derive a free resource name for branch %q (base %q, candidate %q already taken)", branch, base, candidate)
	}
	return candidate, nil
}

func sanitizeDNSLabel(s string) string {
	lower := strings.ToLower(s)
	replaced := invalidDNSLabelChars.ReplaceAllString(lower, "-")
	collapsed := regexp.MustCompile(`-+`).ReplaceAllString(replaced, "-")
	trimmed := strings.Trim(collapsed, "-")
	return truncateDNSLabel(trimmed, dnsLabelMaxLength)
}

// truncateDNSLabel truncates to at most max characters and re-trims any
// trailing '-' the cut may have exposed, so the result is always a valid
// (non-'-'-terminated) DNS label.
func truncateDNSLabel(s string, max int) string {
	if max < 0 {
		max = 0
	}
	if len(s) > max {
		s = s[:max]
	}
	return strings.TrimRight(s, "-")
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:hashSuffixLength]
}
