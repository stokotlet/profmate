package collector

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"github.com/stokotlet/profmate/internal/testhttp"
)

func TestNormalizeURL(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"http://localhost:6060", "http://localhost:6060/debug/pprof/"},
		{"https://example.test/debug/pprof", "https://example.test/debug/pprof/"},
		{"http://[::1]:6060/debug/pprof/heap?token=secret&gc=1&debug=2&seconds=5", "http://[::1]:6060/debug/pprof/?token=secret"},
		{"https://example.test/service/debug/pprof/profile", "https://example.test/service/debug/pprof/"},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := NormalizeURL(test.input)
			if err != nil || got != test.want {
				t.Fatalf("NormalizeURL = %q, %v; want %q", got, err, test.want)
			}
		})
	}
	for _, input := range []string{
		"localhost:6060", "ftp://example.test", "http://", "http://u:secret@example.test",
		"http://example.test/random", "http://example.test/#fragment", "http://example.test:99999",
		"http://example.test/?token=%ZZ", "http://example.test:bad/?token=secret",
	} {
		if _, err := NormalizeURL(input); err == nil {
			t.Errorf("accepted invalid URL %q", input)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("error exposed credentials: %v", err)
		}
	}
}

func TestCollectCapturesWindowAndSavesProfiles(t *testing.T) {
	wire := fixture(t)
	var paths []string
	var mu sync.Mutex
	client := testhttp.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		if r.Header.Get("X-Token") != "header-secret" || r.URL.Query().Get("token") != "query-secret" {
			t.Error("credentials were not passed to endpoint")
		}
		if r.URL.Query().Has("gc") {
			t.Error("collection forced GC")
		}
		if strings.HasSuffix(r.URL.Path, "profile") && r.URL.Query().Get("seconds") != "1" {
			t.Error("CPU profile duration missing")
		}
		if r.URL.Query().Get("debug") == "2" {
			io.WriteString(w, "goroutine 1 [running]:\nmain.work()\n\t/app/main.go:12 +0x1\n")
			return
		}
		w.Write(wire)
	}))
	dir := t.TempDir()
	captures, err := Collect(context.Background(), dir, Options{
		URL:      "http://pprof.test/debug/pprof/heap?token=query-secret&gc=1&debug=1&seconds=99",
		Duration: time.Second,
		Headers:  map[string]string{"X-Token": "header-secret"},
		Client:   client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(captures) != 10 {
		t.Fatalf("got %d captures, want 10", len(captures))
	}
	want := []string{"heap-before", "goroutine-before", "block-before", "mutex-before", "cpu-window", "heap-after", "goroutine-after", "block-after", "mutex-after", "goroutine_text-after"}
	for i, capture := range captures {
		if got := capture.Kind + "-" + capture.Phase; got != want[i] {
			t.Errorf("capture %d = %s; want %s", i, got, want[i])
		}
		if capture.Error != "" || capture.File == "" || capture.FinishedAt.Before(capture.StartedAt) {
			t.Errorf("invalid capture: %+v", capture)
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(capture.File)))
		if err != nil {
			t.Fatal(err)
		}
		if capture.Kind != "goroutine_text" {
			if !bytes.Equal(data, wire) {
				t.Error("gzipped profile bytes were not preserved")
			}
			if _, err := profile.ParseData(data); err != nil {
				t.Fatal(err)
			}
		}
		if i > 0 && capture.StartedAt.Before(captures[i-1].FinishedAt) {
			t.Error("captures overlapped")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 10 {
		t.Errorf("got %d requests", len(paths))
	}
	if strings.Contains(paths[0], "debug=") || !strings.Contains(paths[9], "debug=2") {
		t.Errorf("unexpected debug modes: %v", paths)
	}
}

func TestCollectPartialFailures(t *testing.T) {
	wire := fixture(t)
	client := testhttp.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "profile"):
			http.Error(w, "token=query-secret", http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "block"):
			io.WriteString(w, "<html>login query-secret</html>")
		case strings.HasSuffix(r.URL.Path, "mutex"):
			http.NotFound(w, r)
		case r.URL.Query().Get("debug") == "2":
			io.WriteString(w, "goroutine 1 [running]:\nmain.work()\n")
		default:
			w.Write(wire)
		}
	}))
	captures, err := Collect(context.Background(), t.TempDir(), Options{URL: "http://pprof.test?token=query-secret", Duration: time.Second, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	if len(captures) != 10 {
		t.Fatalf("got %d captures", len(captures))
	}
	if captures[4].Error != "profile endpoint returned HTTP 500" || captures[5].File == "" {
		t.Errorf("CPU failure prevented later captures: %+v", captures)
	}
	for _, capture := range captures {
		if strings.Contains(capture.Error, "secret") || strings.Contains(capture.Error, "http://pprof.test") {
			t.Errorf("capture error leaked request or response data: %s", capture.Error)
		}
	}
}

func TestCollectAllFailures(t *testing.T) {
	client := testhttp.NewClient(http.NotFoundHandler())
	captures, err := Collect(context.Background(), t.TempDir(), Options{URL: "http://pprof.test", Duration: time.Second, Client: client})
	if err == nil || len(captures) != 10 {
		t.Fatalf("captures=%d, err=%v", len(captures), err)
	}
}

func TestCollectRejectsInvalidDurationBeforeRequests(t *testing.T) {
	for _, duration := range []time.Duration{0, -time.Second, 500 * time.Millisecond, time.Second + time.Nanosecond, 301 * time.Second} {
		if _, err := Collect(context.Background(), t.TempDir(), Options{URL: "http://example.test", Duration: duration}); err == nil {
			t.Errorf("accepted duration %v", duration)
		}
	}
}

func TestCollectCancellation(t *testing.T) {
	started := make(chan struct{})
	client := testhttp.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	captures, err := Collect(ctx, t.TempDir(), Options{URL: "http://pprof.test", Duration: time.Second, Client: client})
	if !errors.Is(err, context.Canceled) || len(captures) != 1 || !strings.Contains(captures[0].Error, "canceled") {
		t.Fatalf("captures=%+v, err=%v", captures, err)
	}
}

func TestCollectRejectsRedirectWithoutForwardingHeaders(t *testing.T) {
	var redirected bool
	client := testhttp.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "destination.test" {
			redirected = true
			return
		}
		http.Redirect(w, r, "http://destination.test/", http.StatusFound)
	}))
	captures, err := Collect(context.Background(), t.TempDir(), Options{URL: "http://pprof.test", Duration: time.Second, Headers: map[string]string{"X-Private": "secret"}, Client: client})
	if err == nil || redirected || len(captures) != 10 || captures[0].Error != "profile endpoint returned HTTP 302" {
		t.Fatalf("redirected=%v, captures=%+v, err=%v", redirected, captures, err)
	}
}

func TestFetchRequestDeadlines(t *testing.T) {
	base, _ := url.Parse("http://pprof.test/debug/pprof/")
	for _, tc := range []struct {
		kind string
		want time.Duration
	}{
		{"heap", 10 * time.Second},
		{"cpu", 16 * time.Second},
		{"goroutine_text", 10 * time.Second},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				remaining := time.Until(deadline)
				if !ok || remaining > tc.want || remaining < tc.want-time.Second {
					t.Errorf("request timeout = %v, exists=%v; want about %v", remaining, ok, tc.want)
				}
				return nil, errors.New("test transport failure")
			})}
			if _, err := fetch(context.Background(), client, base, tc.kind, Options{Duration: time.Second}); err == nil {
				t.Error("transport failure was accepted")
			}
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	client := testhttp.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	if _, err := fetch(ctx, client, base, "heap", Options{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fetch error = %v; want caller deadline exceeded", err)
	}
}

func TestReadBoundedAndCompressedLimit(t *testing.T) {
	if _, err := readBounded(io.LimitReader(zeroReader{}, maxBodyBytes+1)); err == nil {
		t.Error("oversized response accepted")
	}
	var bomb bytes.Buffer
	w := gzip.NewWriter(&bomb)
	if _, err := io.CopyN(w, zeroReader{}, maxBodyBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := validateProfile(bomb.Bytes()); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized decompressed profile accepted: %v", err)
	}
}

func TestValidateUncompressedProfileAndMalformedInputs(t *testing.T) {
	p, err := profile.ParseData(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	if err := p.WriteUncompressed(&raw); err != nil {
		t.Fatal(err)
	}
	encoded, err := validateProfile(raw.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) < 2 || encoded[0] != 0x1f || encoded[1] != 0x8b {
		t.Fatal("uncompressed input was not saved as gzip")
	}
	for _, data := range [][]byte{nil, []byte("<html>login</html>"), {0x1f, 0x8b, 0x00}} {
		if _, err := validateProfile(data); err == nil {
			t.Errorf("accepted invalid input %q", data)
		}
	}
}

func TestFetchTransportErrorsDoNotLeakSecrets(t *testing.T) {
	u, _ := url.Parse("https://example.test/debug/pprof/?token=secret")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("Authorization: secret")
	})}
	_, err := fetch(context.Background(), client, u, "heap", Options{})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unexpected transport error: %v", err)
	}
}

func fixture(t *testing.T) []byte {
	t.Helper()
	f := &profile.Function{ID: 1, Name: "main.work", Filename: "/app/main.go"}
	l := &profile.Location{ID: 1, Line: []profile.Line{{Function: f, Line: 12}}}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
		Sample:     []*profile.Sample{{Location: []*profile.Location{l}, Value: []int64{10, 100000000}}},
		Function:   []*profile.Function{f}, Location: []*profile.Location{l},
		DurationNanos: int64(time.Second),
	}
	var out bytes.Buffer
	if err := p.Write(&out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
