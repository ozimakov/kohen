package git

import (
	"context"
	"net"
	"testing"
)

func TestParseURL(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		allowLocal bool
		wantKind   transportKind
		wantHost   string
		wantErr    bool
	}{
		{name: "https", raw: "https://github.com/acme/repo.git", wantKind: transportHTTPS, wantHost: "github.com"},
		{name: "https with port", raw: "https://git.example.com:8443/x.git", wantKind: transportHTTPS, wantHost: "git.example.com"},
		{name: "ssh scheme", raw: "ssh://git@github.com/acme/repo.git", wantKind: transportSSH, wantHost: "github.com"},
		{name: "scp-like ssh", raw: "git@github.com:acme/repo.git", wantKind: transportSSH, wantHost: "github.com"},
		{name: "http rejected", raw: "http://github.com/x.git", wantErr: true},
		{name: "git scheme rejected", raw: "git://github.com/x.git", wantErr: true},
		{name: "file rejected without allow", raw: "file:///tmp/x", wantErr: true},
		{name: "file allowed", raw: "file:///tmp/x", allowLocal: true, wantKind: transportLocal},
		{name: "local path allowed", raw: "/tmp/repo", allowLocal: true, wantKind: transportLocal},
		{name: "local path rejected", raw: "/tmp/repo", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parseURL(tc.raw, tc.allowLocal)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", p)
				}
				if _, ok := ReasonOf(err); !ok {
					t.Fatalf("expected a *git.Error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.kind != tc.wantKind {
				t.Fatalf("kind = %v, want %v", p.kind, tc.wantKind)
			}
			if p.host != tc.wantHost {
				t.Fatalf("host = %q, want %q", p.host, tc.wantHost)
			}
		})
	}
}

func TestGuardHostIPLiteral(t *testing.T) {
	c := NewClient(Options{})
	blocked := []string{
		"https://127.0.0.1/x.git",
		"https://169.254.169.254/x.git", // cloud metadata (link-local)
		"https://[::1]/x.git",           // ipv6 loopback
		"https://0.0.0.0/x.git",         // unspecified
	}
	for _, u := range blocked {
		p, err := parseURL(u, false)
		if err != nil {
			t.Fatalf("parseURL(%q): %v", u, err)
		}
		err = c.validate(context.Background(), p)
		if err == nil {
			t.Fatalf("expected %q to be blocked", u)
		}
		if r, _ := ReasonOf(err); r != ReasonSourceNotAllowed {
			t.Fatalf("reason for %q = %v, want SourceNotAllowed", u, r)
		}
	}

	// Private RFC1918 addresses are allowed (in-cluster git servers).
	for _, u := range []string{"https://10.0.0.5/x.git", "https://192.168.1.10/x.git"} {
		p, _ := parseURL(u, false)
		if err := c.validate(context.Background(), p); err != nil {
			t.Fatalf("expected %q to be allowed, got %v", u, err)
		}
	}
}

type fakeResolver struct {
	ips []net.IPAddr
	err error
}

func (f fakeResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return f.ips, f.err
}

func TestGuardHostDNS(t *testing.T) {
	// A hostname resolving to a link-local metadata IP must be blocked.
	c := NewClient(Options{Resolver: fakeResolver{ips: []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}}})
	p, _ := parseURL("https://metadata.internal/x.git", false)
	err := c.validate(context.Background(), p)
	if r, _ := ReasonOf(err); r != ReasonSourceNotAllowed {
		t.Fatalf("expected SourceNotAllowed, got %v (err %v)", r, err)
	}

	// A hostname resolving to a public IP is allowed.
	c2 := NewClient(Options{Resolver: fakeResolver{ips: []net.IPAddr{{IP: net.ParseIP("140.82.121.3")}}}})
	p2, _ := parseURL("https://github.com/x.git", false)
	if err := c2.validate(context.Background(), p2); err != nil {
		t.Fatalf("expected allowed, got %v", err)
	}
}

func TestCheckAllowList(t *testing.T) {
	c := NewClient(Options{AllowList: []string{"github.com", "https://gitlab.example.com/team/"}})
	allowed := []string{
		"https://github.com/acme/repo.git",
		"https://api.github.com/acme/repo.git", // subdomain
		"https://gitlab.example.com/team/repo.git",
	}
	for _, u := range allowed {
		p, _ := parseURL(u, false)
		if err := c.validate(context.Background(), p); err != nil {
			t.Fatalf("expected %q allowed, got %v", u, err)
		}
	}
	denied := []string{
		"https://evil.com/x.git",
		"https://notgithub.com/x.git",
		"https://gitlab.example.com/other/repo.git", // wrong prefix path
	}
	for _, u := range denied {
		p, _ := parseURL(u, false)
		err := c.validate(context.Background(), p)
		if r, _ := ReasonOf(err); r != ReasonSourceNotAllowed {
			t.Fatalf("expected %q denied with SourceNotAllowed, got %v (err %v)", u, r, err)
		}
	}
}

func TestEmptyAllowListPermits(t *testing.T) {
	c := NewClient(Options{})
	p, _ := parseURL("https://anything.example.org/x.git", false)
	if err := c.validate(context.Background(), p); err != nil {
		t.Fatalf("empty allow-list should permit any host, got %v", err)
	}
}
