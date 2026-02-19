package server

import (
	"strings"
	"testing"
)

func TestGenerateSubdomain(t *testing.T) {
	// Test that generated subdomains are unique
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		subdomain := generateSubdomain()

		// Check length (8 chars from 5 bytes base32)
		if len(subdomain) != 8 {
			t.Errorf("generateSubdomain() length = %d, want 8", len(subdomain))
		}

		// Check lowercase (DNS compatible)
		if subdomain != strings.ToLower(subdomain) {
			t.Errorf("generateSubdomain() = %q is not lowercase", subdomain)
		}

		// Check uniqueness
		if seen[subdomain] {
			t.Errorf("generateSubdomain() generated duplicate: %q", subdomain)
		}
		seen[subdomain] = true
	}
}

func TestGenerateSubdomain_DNSCompatible(t *testing.T) {
	for i := 0; i < 100; i++ {
		subdomain := generateSubdomain()

		// Check it only contains valid DNS label characters (alphanumeric)
		for _, c := range subdomain {
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
				t.Errorf("generateSubdomain() = %q contains invalid char %q", subdomain, c)
			}
		}
	}
}
