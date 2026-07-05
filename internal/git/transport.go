package git

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	transportclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// maxRedirects caps HTTP redirects go-git will follow before failing closed.
const maxRedirects = 10

var installOnce sync.Once

// installSafeHTTPTransport installs a process-wide HTTPS transport for go-git
// that re-applies the SSRF host guard on every redirect hop (R-AUTH.7 / TM5).
//
// go-git otherwise follows redirects with Go's default policy and never
// re-screens the target host, so a repository on an allowed/public host that
// 30x-redirects to a link-local or cloud-metadata endpoint (e.g.
// 169.254.169.254) would bypass the initial-URL guard entirely. Blocking such
// targets at redirect time closes that hole. The block is universal; the
// operator source allow-list is still enforced on the initial URL by validate().
//
// It is installed once per process (go-git protocol handlers are global). The
// operator constructs a single Client, so the first resolver wins; tests
// exercise the redirect decision through guardRedirect directly.
func installSafeHTTPTransport(resolver Resolver) {
	installOnce.Do(func() {
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		hc := &http.Client{
			Timeout: 5 * time.Minute,
			// go-git requires an *http.Transport so it can apply per-fetch TLS
			// options (e.g. InsecureSkipTLS); a nil transport is rejected.
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return fmt.Errorf("stopped after %d redirects", maxRedirects)
				}
				return guardRedirect(req.Context(), resolver, req.URL.Scheme, req.URL.Hostname())
			},
		}
		transportclient.InstallProtocol("https", githttp.NewClient(hc))
	})
}

// guardRedirect fails closed when a redirect target uses a non-https scheme or
// resolves to a blocked (loopback/link-local/metadata/multicast) address.
func guardRedirect(ctx context.Context, resolver Resolver, scheme, host string) error {
	if strings.ToLower(scheme) != "https" {
		return newError(ReasonSourceNotAllowed,
			fmt.Sprintf("redirect to non-https scheme %q blocked", scheme))
	}
	return guardResolvedHost(ctx, resolver, host)
}
