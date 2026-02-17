package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSearchModesAndBoundedRetention(t *testing.T) {
	for _, optimized := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		a := newDemoApp(ctx, optimized)
		h := a.routes()
		for range 40 {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/search?q=go", nil))
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"query":"go"`) {
				cancel()
				a.workers.Wait()
				t.Fatalf("optimized=%t: status=%d body=%q", optimized, w.Code, w.Body.String())
			}
		}
		var retained int
		for _, b := range a.retained {
			retained += len(b)
		}
		cancel()
		a.workers.Wait()
		if retained != 2<<20 || a.requests != 40 {
			t.Fatalf("retained=%d requests=%d", retained, a.requests)
		}
	}
}

func TestHandlersRejectInvalidRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := newDemoApp(ctx, false)
	defer func() { cancel(); a.workers.Wait() }()
	h := a.routes()
	for _, tc := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodPost, "/search", http.StatusMethodNotAllowed},
		{http.MethodGet, "/search?q=" + strings.Repeat("a", 257), http.StatusBadRequest},
		{http.MethodGet, "/search?q=%3Cscript%3E", http.StatusBadRequest},
		{http.MethodGet, "/debug/pprof/", http.StatusNotFound},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Errorf("%s %s: got %d, want %d", tc.method, tc.path, w.Code, tc.status)
		}
	}
}

func TestCanceledServiceDoesNotHang(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := newDemoApp(ctx, false)
	cancel()
	a.workers.Wait()
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/search?q=go", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d", w.Code)
	}
}
