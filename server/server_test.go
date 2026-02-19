package server

import (
	"strings"
	"testing"
)

func TestExtractHost(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		expected string
	}{
		{
			name:     "standard host header",
			header:   "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n",
			expected: "example.com",
		},
		{
			name:     "host with port",
			header:   "GET / HTTP/1.1\r\nHost: example.com:8080\r\n\r\n",
			expected: "example.com:8080",
		},
		{
			name:     "subdomain host",
			header:   "GET / HTTP/1.1\r\nHost: myapp.tunnel.example.com\r\n\r\n",
			expected: "myapp.tunnel.example.com",
		},
		{
			name:     "uppercase host header name",
			header:   "GET / HTTP/1.1\r\nHOST: example.com\r\n\r\n",
			expected: "example.com",
		},
		{
			name:     "mixed case host value is lowercased",
			header:   "GET / HTTP/1.1\r\nHost: MyApp.Example.COM\r\n\r\n",
			expected: "myapp.example.com",
		},
		{
			name:     "host with extra whitespace",
			header:   "GET / HTTP/1.1\r\nHost:   example.com  \r\n\r\n",
			expected: "example.com",
		},
		{
			name:     "no host header",
			header:   "GET / HTTP/1.1\r\nContent-Type: text/html\r\n\r\n",
			expected: "",
		},
		{
			name:     "empty header",
			header:   "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractHost(tt.header)
			if got != tt.expected {
				t.Errorf("extractHost() = %q, want %q", got, tt.expected)
			}
		})
	}
}

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
