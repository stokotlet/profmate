package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const responseLimit = 1 << 20

func validateTarget(target string) error {
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return errors.New("target must be an HTTP(S) URL without credentials or fragment")
	}
	return nil
}

type loadStats struct {
	ok      atomic.Uint64
	failed  atomic.Uint64
	latency atomic.Int64
}

func doRequest(ctx context.Context, client *http.Client, target string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, responseLimit+1))
	if err != nil {
		return err
	}
	if n > responseLimit {
		return errors.New("response exceeds 1 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func load(ctx context.Context, client *http.Client, target string, workers int) *loadStats {
	stats := &loadStats{}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				start := time.Now()
				err := doRequest(ctx, client, target)
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					stats.failed.Add(1)
					timer := time.NewTimer(100 * time.Millisecond)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
					continue
				}
				stats.ok.Add(1)
				stats.latency.Add(time.Since(start).Nanoseconds())
			}
		}()
	}
	wg.Wait()
	return stats
}

func run() error {
	target := flag.String("target", "http://127.0.0.1:8080/search?q=go", "HTTP(S) URL of a service you own")
	duration := flag.Duration("duration", 45*time.Second, "load duration (up to 10m)")
	workers := flag.Int("workers", 16, "concurrent requests (1 to 128)")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected arguments; use -help")
	}
	if err := validateTarget(*target); err != nil {
		return err
	}
	if *duration <= 0 || *duration > 10*time.Minute {
		return errors.New("duration must be greater than zero and at most 10m")
	}
	if *workers < 1 || *workers > 128 {
		return errors.New("workers must be between 1 and 128")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment, MaxIdleConns: *workers,
		MaxIdleConnsPerHost: *workers, MaxConnsPerHost: *workers,
		IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	fmt.Printf("Load: %d workers for %s; stop with Ctrl+C\n", *workers, *duration)
	started := time.Now()
	stats := load(ctx, client, *target, *workers)
	elapsed := time.Since(started)
	ok := stats.ok.Load()
	fmt.Printf("Completed: %d; failed: %d; elapsed: %s\n", ok, stats.failed.Load(), elapsed.Round(time.Millisecond))
	if ok == 0 {
		return errors.New("no successful requests; check the target and start the demo")
	}
	fmt.Printf("Throughput: %.1f requests/s; mean completed latency: %s\n", float64(ok)/elapsed.Seconds(), (time.Duration(stats.latency.Load()) / time.Duration(ok)).Round(time.Microsecond))
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
