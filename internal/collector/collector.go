package collector

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/pprof/profile"
	"github.com/profmate/profmate/internal/model"
)

const maxBodyBytes = 32 << 20

type Options struct {
	URL      string
	Duration time.Duration
	Headers  map[string]string
	Client   *http.Client
	Progress func(string)
}

func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Opaque != "" || u.Hostname() == "" {
		return "", errors.New("invalid pprof URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("pprof URL must use http or https")
	}
	if u.User != nil {
		return "", errors.New("URL credentials are unsupported; use request headers")
	}
	if u.Fragment != "" {
		return "", errors.New("pprof URL must not include a fragment")
	}
	if port := u.Port(); port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return "", errors.New("invalid pprof URL port")
		}
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "" {
		path = "/debug/pprof"
	} else if !strings.HasSuffix(path, "/debug/pprof") {
		parent, name, found := strings.Cut(path, "/debug/pprof/")
		if !found || !isProfileName(name) {
			return "", errors.New("URL must address the server root or /debug/pprof")
		}
		path = parent + "/debug/pprof"
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", errors.New("invalid pprof URL query")
	}
	query.Del("seconds")
	query.Del("debug")
	query.Del("gc")
	u.Path = path + "/"
	u.RawPath = ""
	u.RawQuery = query.Encode()
	u.ForceQuery = false
	return u.String(), nil
}

func isProfileName(name string) bool {
	switch name {
	case "profile", "heap", "allocs", "goroutine", "block", "mutex", "threadcreate", "trace", "cmdline", "symbol":
		return true
	}
	return false
}

func Collect(ctx context.Context, dir string, opts Options) ([]model.Capture, error) {
	if opts.Duration < time.Second || opts.Duration > 300*time.Second || opts.Duration%time.Second != 0 {
		return nil, errors.New("duration must be a whole number of seconds from 1 to 300")
	}
	normalized, err := NormalizeURL(opts.URL)
	if err != nil {
		return nil, err
	}
	base, _ := url.Parse(normalized)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "profiles"), 0700); err != nil {
		return nil, errors.New("cannot create profile directory")
	}
	client := http.Client{}
	if opts.Client != nil {
		client = *opts.Client
	}
	// don't follow redirects: auth headers would go with them
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	type request struct{ kind, phase string }
	requests := []request{
		{"heap", "before"}, {"goroutine", "before"}, {"block", "before"}, {"mutex", "before"},
		{"cpu", "window"},
		{"heap", "after"}, {"goroutine", "after"}, {"block", "after"}, {"mutex", "after"},
		{"goroutine_text", "after"},
	}
	captures := make([]model.Capture, 0, len(requests))
	successes := 0
	for _, r := range requests {
		if err := ctx.Err(); err != nil {
			return captures, err
		}
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf("Collecting %s (%s)", r.kind, r.phase))
		}
		capture := model.Capture{Kind: r.kind, Phase: r.phase, StartedAt: time.Now().UTC()}
		data, err := fetch(ctx, &client, base, r.kind, opts)
		if err == nil {
			ext := ".pprof"
			if r.kind == "goroutine_text" {
				ext = ".txt"
			}
			relative := filepath.Join("profiles", r.kind+"-"+r.phase+ext)
			if err = saveProfile(filepath.Join(dir, relative), data); err == nil {
				capture.File = filepath.ToSlash(relative)
				successes++
			}
		}
		capture.FinishedAt = time.Now().UTC()
		if err != nil {
			capture.Error = err.Error()
		}
		captures = append(captures, capture)
		if err := ctx.Err(); err != nil {
			return captures, err
		}
	}
	if successes == 0 {
		return captures, errors.New("no usable profiles were collected; see capture errors")
	}
	return captures, nil
}

func fetch(ctx context.Context, client *http.Client, base *url.URL, kind string, opts Options) ([]byte, error) {
	target := *base
	query := target.Query()
	name := kind
	timeout := 10 * time.Second
	switch kind {
	case "cpu":
		name = "profile"
		query.Set("seconds", strconv.Itoa(int(opts.Duration/time.Second)))
		timeout = opts.Duration + 15*time.Second
	case "goroutine_text":
		name = "goroutine"
		query.Set("debug", "2")
	}
	target.Path += name
	target.RawQuery = query.Encode()
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, errors.New("cannot construct profile request")
	}
	for key, value := range opts.Headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		if requestCtx.Err() != nil {
			return nil, fmt.Errorf("profile request: %w", requestCtx.Err())
		}
		return nil, errors.New("profile request failed (connection, TLS, or request error)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("profile endpoint returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBodyBytes {
		return nil, errors.New("profile exceeds the 32 MiB size limit")
	}
	data, err := readBounded(resp.Body)
	if err != nil {
		if requestCtx.Err() != nil {
			return nil, fmt.Errorf("profile request: %w", requestCtx.Err())
		}
		return nil, err
	}
	if kind == "goroutine_text" {
		if !goroutineHeader.Match(bytes.TrimSpace(data)) {
			return nil, errors.New("endpoint did not return a goroutine stack dump")
		}
		return data, nil
	}
	return validateProfile(data)
}

var goroutineHeader = regexp.MustCompile(`^goroutine [0-9]+ \[[^\]\r\n]+\]:\r?\n`)

func readBounded(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil {
		return nil, errors.New("could not read complete profile response")
	}
	if len(data) > maxBodyBytes {
		return nil, errors.New("profile exceeds the 32 MiB size limit")
	}
	return data, nil
}

func validateProfile(data []byte) ([]byte, error) {
	decoded := data
	compressed := len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
	if compressed {
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, errors.New("endpoint returned an invalid compressed profile")
		}
		decoded, err = readBounded(reader)
		reader.Close()
		if err != nil {
			return nil, err
		}
	}
	p, err := profile.ParseUncompressed(decoded)
	if err != nil || len(p.SampleType) == 0 {
		return nil, errors.New("endpoint did not return a valid binary pprof profile")
	}
	if err := p.CheckValid(); err != nil {
		return nil, errors.New("endpoint returned a malformed pprof profile")
	}
	if compressed {
		return data, nil
	}
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := w.Write(data); err != nil {
		return nil, errors.New("cannot compress profile")
	}
	if err := w.Close(); err != nil {
		return nil, errors.New("cannot compress profile")
	}
	return out.Bytes(), nil
}

func saveProfile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".capture-*")
	if err != nil {
		return errors.New("cannot create profile file")
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return errors.New("cannot write profile file")
	}
	if err := f.Close(); err != nil {
		return errors.New("cannot finish profile file")
	}
	if err := os.Rename(temporary, path); err != nil {
		return errors.New("cannot save profile file")
	}
	return nil
}
