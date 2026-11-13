package routing

import (
	"net"
	"strings"
	"testing"
)

func TestParseTrustedProxies(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		wantLen int
		wantErr string
	}{
		{
			name:    "no entries trusts nothing",
			entries: nil,
			wantLen: 0,
		},
		{
			name:    "empty list trusts nothing",
			entries: []string{},
			wantLen: 0,
		},
		{
			name:    "ipv4 cidr",
			entries: []string{"10.0.0.0/8"},
			wantLen: 1,
		},
		{
			name:    "ipv6 cidr",
			entries: []string{"2001:db8::/32"},
			wantLen: 1,
		},
		{
			name:    "bare ipv4 is a single host",
			entries: []string{"203.0.113.7"},
			wantLen: 1,
		},
		{
			name:    "bare ipv6 is a single host",
			entries: []string{"2001:db8::1"},
			wantLen: 1,
		},
		{
			name:    "surrounding whitespace is tolerated",
			entries: []string{"  10.0.0.0/8  "},
			wantLen: 1,
		},
		{
			name:    "several entries",
			entries: []string{"127.0.0.1/32", "::1/128", "10.0.0.0/8"},
			wantLen: 3,
		},
		{
			name:    "an empty entry is an error",
			entries: []string{"10.0.0.0/8", "   "},
			wantErr: "empty",
		},
		{
			name:    "garbage is an error",
			entries: []string{"cloudflare"},
			wantErr: "expected CIDR notation",
		},
		{
			name:    "a hostname is an error rather than a resolution",
			entries: []string{"proxy.internal/32"},
			wantErr: "expected CIDR notation",
		},
		{
			name:    "an out-of-range prefix is an error",
			entries: []string{"10.0.0.0/33"},
			wantErr: "expected CIDR notation",
		},
		{
			name:    "every bad entry is reported, not just the first",
			entries: []string{"nope", "10.0.0.0/33", ""},
			wantErr: "trusted proxy #2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tp, err := ParseTrustedProxies(tc.entries)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseTrustedProxies(%q) succeeded, want an error", tc.entries)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				if tp != nil {
					t.Errorf("a failed parse returned a non-nil set: %v", tp)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseTrustedProxies(%q): %v", tc.entries, err)
			}
			if tp.Len() != tc.wantLen {
				t.Errorf("Len() = %d, want %d", tp.Len(), tc.wantLen)
			}
		})
	}
}

// TestParseTrustedProxies_MultipleErrorsAreAllReported pins the aggregate
// behaviour: fixing a trust list one error per restart is exactly the loop
// config.Validate exists to avoid, and this list is a security decision.
func TestParseTrustedProxies_MultipleErrorsAreAllReported(t *testing.T) {
	_, err := ParseTrustedProxies([]string{"nope", "10.0.0.0/33", ""})
	if err == nil {
		t.Fatal("expected an error")
	}

	msg := err.Error()
	for _, want := range []string{"#2", "nope", "10.0.0.0/33"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func TestTrustedProxies_Trusts(t *testing.T) {
	tp, err := ParseTrustedProxies([]string{"127.0.0.1/32", "::1/128", "10.0.0.0/8"})
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}

	tests := []struct {
		name       string
		remoteAddr string
		want       bool
	}{
		{"ipv4 in range", "127.0.0.1:54321", true},
		{"ipv4 in a wider range", "10.1.2.3:80", true},
		{"ipv6 loopback bracketed with a port", "[::1]:54321", true},
		{"ipv6 loopback bracketed without a port", "[::1]", true},
		{"bare address with no port", "127.0.0.1", true},
		{"ipv4-mapped ipv6 loopback", "[::ffff:127.0.0.1]:54321", true},
		{"outside every range", "192.0.2.1:54321", false},
		{"a neighbouring address of a /32", "127.0.0.2:54321", false},
		{"just outside a wider range", "11.0.0.1:80", false},
		{"empty", "", false},
		{"not an address", "somewhere:80", false},
		{"a hostname", "proxy.internal:80", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tp.Trusts(tc.remoteAddr); got != tc.want {
				t.Errorf("Trusts(%q) = %v, want %v", tc.remoteAddr, got, tc.want)
			}
		})
	}
}

// TestTrustedProxies_ZeroValueTrustsNothing pins the safe default. A nil or
// zero-valued set is what a missing configuration produces, and it must fail
// closed rather than open.
func TestTrustedProxies_ZeroValueTrustsNothing(t *testing.T) {
	var nilSet *TrustedProxies
	if nilSet.Trusts("127.0.0.1:1234") {
		t.Error("a nil TrustedProxies trusted a loopback address")
	}
	if nilSet.Contains(net.ParseIP("127.0.0.1")) {
		t.Error("a nil TrustedProxies contained a loopback address")
	}
	if nilSet.Len() != 0 {
		t.Errorf("a nil TrustedProxies reported Len() = %d", nilSet.Len())
	}

	var zero TrustedProxies
	if zero.Trusts("127.0.0.1:1234") {
		t.Error("the zero TrustedProxies trusted a loopback address")
	}
}

// TestRemoteIPString_CanonicalisesIPv4MappedAddresses matters because the
// resolved client IP is written into X-Forwarded-For. If a dual-stack listener
// reported "::ffff:127.0.0.1" for one request and "127.0.0.1" for another, the
// same client would appear as two.
func TestRemoteIPString_CanonicalisesIPv4MappedAddresses(t *testing.T) {
	tests := []struct {
		remoteAddr string
		want       string
	}{
		{"127.0.0.1:1234", "127.0.0.1"},
		{"[::1]:1234", "::1"},
		{"[::ffff:127.0.0.1]:1234", "127.0.0.1"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"", ""},
		{"not-an-address", ""},
	}

	for _, tc := range tests {
		t.Run(tc.remoteAddr, func(t *testing.T) {
			if got := remoteIPString(tc.remoteAddr); got != tc.want {
				t.Errorf("remoteIPString(%q) = %q, want %q", tc.remoteAddr, got, tc.want)
			}
		})
	}
}
