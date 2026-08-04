package httpcore

import (
	"bytes"
	"io"
	"net/http"
)

// roundTripFunc adapts a function to http.RoundTripper so tests drive a Client
// entirely in process. An httptest.Server is deliberately not used anywhere in
// this repo's provider tests: the conformance kit's NoGoroutineLeak case
// snapshots goroutines and a live server's accept goroutine does not survive it.
type roundTripFunc func(req *http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// fakeClient returns an *http.Client whose transport is f.
func fakeClient(f roundTripFunc) *http.Client { return &http.Client{Transport: f} }

// recordingBody is a ReadCloser that records whether it was closed, so tests
// can prove Do drains and closes every response.
type recordingBody struct {
	io.Reader
	closed bool
}

// Close marks the body closed.
func (b *recordingBody) Close() error {
	b.closed = true
	return nil
}

// newResponse builds an *http.Response with a recording body.
func newResponse(status int, body []byte, header http.Header) (*http.Response, *recordingBody) {
	rb := &recordingBody{Reader: bytes.NewReader(body)}
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       rb,
	}, rb
}
