package httpmock

import (
	"errors"
	"net/http"
	"net/http/httptest"
)

// RoundTripFunc allows tests to stub an http.RoundTripper inline.
type RoundTripFunc func(*http.Request) (*http.Response, error)

func (f RoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	if f == nil {
		return nil, errors.New("httpmock: nil round trip func")
	}
	return f(req)
}

// HandlerTransport serves requests directly through an http.Handler without opening a listener.
type HandlerTransport struct {
	Handler http.Handler
}

func (t HandlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.Handler == nil {
		return nil, errors.New("httpmock: nil handler")
	}
	rr := httptest.NewRecorder()
	clone := req.Clone(req.Context())
	if clone.Body == nil {
		clone.Body = http.NoBody
	}
	if clone.Host == "" && clone.URL != nil {
		clone.Host = clone.URL.Host
	}
	t.Handler.ServeHTTP(rr, clone)
	return rr.Result(), nil
}

// NewClient returns an http.Client backed by HandlerTransport.
func NewClient(handler http.Handler) *http.Client {
	return &http.Client{Transport: HandlerTransport{Handler: handler}}
}
