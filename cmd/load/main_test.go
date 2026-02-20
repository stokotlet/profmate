package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestValidateTarget(t *testing.T) {
	for _, target := range []string{"http://localhost:8080/search?q=go", "https://example.test/search"} {
		if err := validateTarget(target); err != nil {
			t.Errorf("%q: %v", target, err)
		}
	}
	for _, target := range []string{"file:///tmp/data", "ftp://example.test", "http://user:pass@localhost", "http://localhost/#secret", "localhost:8080"} {
		if err := validateTarget(target); err == nil {
			t.Errorf("accepted %q", target)
		}
	}
}

func TestRequestResponseBoundAndStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		wantOK bool
	}{
		{http.StatusOK, "ok", true},
		{http.StatusOK, strings.Repeat("x", responseLimit+1), false},
		{http.StatusServiceUnavailable, "unavailable", false},
		{http.StatusFound, "redirect", false},
	} {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
		})}
		err := doRequest(context.Background(), client, "http://example.test/search")
		if (err == nil) != tc.wantOK {
			t.Errorf("status %d, bytes %d: %v", tc.status, len(tc.body), err)
		}
	}
}
