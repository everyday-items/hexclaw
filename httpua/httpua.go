// Package httpua centralises the default browser User-Agent for hexclaw's
// outbound HTTP fetches.
//
// Without an explicit UA, Go's net/http sends "Go-http-client/1.1", which many
// sites (e.g. Baidu hot search) answer with an anti-bot HTML page instead of the
// real payload — breaking downstream json_decode / image decoding with errors
// like `invalid character '<'`. Every outbound fetch that talks to the public
// web should default to a realistic browser UA via Set.
package httpua

import "net/http"

// Default is a realistic desktop-Chrome User-Agent.
const Default = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// Set applies the default User-Agent to req, but only if the caller has not
// already set one — so an explicit per-request UA (e.g. a Starlark script's
// headers, or a platform API's required UA) always wins.
func Set(req *http.Request) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", Default)
	}
}
