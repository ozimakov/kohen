package git

import (
	"context"
	"net"
	"testing"
)

// mapResolver maps hostnames to fixed IPs for hermetic SSRF tests.
type mapResolver map[string][]net.IPAddr

func (f mapResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if addrs, ok := f[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "not found", Name: host}
}

func ipAddrs(ips ...string) []net.IPAddr {
	out := make([]net.IPAddr, 0, len(ips))
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out
}

func TestGuardRedirect(t *testing.T) {
	res := mapResolver{
		"evil.example.com": ipAddrs("169.254.169.254"),
		"ok.example.com":   ipAddrs("93.184.216.34"),
	}
	ctx := context.Background()

	tests := []struct {
		name    string
		scheme  string
		host    string
		wantErr bool
	}{
		{"https to public host", "https", "ok.example.com", false},
		{"https to metadata IP literal", "https", "169.254.169.254", true},
		{"https to loopback literal", "https", "127.0.0.1", true},
		{"https redirect to metadata via DNS", "https", "evil.example.com", true},
		{"downgrade to http blocked", "http", "ok.example.com", true},
		{"downgrade to file blocked", "file", "ok.example.com", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := guardRedirect(ctx, res, tc.scheme, tc.host)
			if tc.wantErr && err == nil {
				t.Fatalf("expected redirect to be blocked")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected redirect to be allowed, got %v", err)
			}
		})
	}
}

func TestGuardResolvedHostBlocksMetadata(t *testing.T) {
	res := mapResolver{"meta": ipAddrs("169.254.169.254")}
	if err := guardResolvedHost(context.Background(), res, "meta"); err == nil {
		t.Fatal("expected metadata host to be blocked after resolution")
	}
	if err := guardResolvedHost(context.Background(), res, "203.0.113.7"); err != nil {
		t.Fatalf("public IP literal should be allowed: %v", err)
	}
}
