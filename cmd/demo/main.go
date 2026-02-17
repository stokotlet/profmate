package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const searchPattern = `^[a-zA-Z0-9 _.-]{1,256}$`

var compiledPattern = regexp.MustCompile(searchPattern)

type searchJob struct {
	ctx    context.Context
	query  string
	result chan int
}

type demoApp struct {
	ctx       context.Context
	optimized bool
	jobs      chan searchJob
	workers   sync.WaitGroup
	mu        sync.Mutex
	retained  [32][]byte // 2 MiB cap, not a leak
	next      int
	requests  uint64
	checksum  uint64
}

func newDemoApp(ctx context.Context, optimized bool) *demoApp {
	a := &demoApp{ctx: ctx, optimized: optimized, jobs: make(chan searchJob, 8)}
	for range 2 {
		a.workers.Add(1)
		go a.worker()
	}
	return a
}

func (a *demoApp) worker() {
	defer a.workers.Done()
	for {
		select {
		case <-a.ctx.Done():
			return
		case job := <-a.jobs:
			if job.ctx.Err() != nil {
				continue
			}
			total := 0
			for i := range 64 {
				record := fmt.Sprintf("%s result-%d %s", job.query, i, strings.Repeat("description ", 16))
				total += len(strings.ToUpper(record))
			}
			job.result <- total
		}
	}
}

func (a *demoApp) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/search", a.search)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ProfMate demo: GET /search?q=go; GET /healthz")
	})
	return mux
}

func (a *demoApp) search(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		q = "go profiling"
	}
	if len(q) > 256 {
		http.Error(w, "q must not exceed 256 bytes", http.StatusBadRequest)
		return
	}
	re := compiledPattern
	if !a.optimized {
		re = regexp.MustCompile(searchPattern) // per request on purpose
	}
	if !re.MatchString(q) {
		http.Error(w, "q contains unsupported characters", http.StatusBadRequest)
		return
	}
	job := searchJob{ctx: r.Context(), query: q, result: make(chan int, 1)}
	select {
	case a.jobs <- job:
	case <-r.Context().Done():
		return
	case <-a.ctx.Done():
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	var processed int
	select {
	case processed = <-job.result:
	case <-r.Context().Done():
		return
	case <-a.ctx.Done():
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	payload := make([]byte, 64<<10)
	for i := range payload {
		payload[i] = byte((i + processed) % 251)
	}
	a.mu.Lock()
	// long on purpose, otherwise the mutex profile stays empty
	var checksum uint64
	for _, b := range payload {
		checksum += uint64(b)
	}
	a.checksum = checksum
	a.retained[a.next] = payload
	a.next = (a.next + 1) % len(a.retained)
	a.requests++
	requests := a.requests
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Query     string `json:"query"`
		Processed int    `json:"processed_bytes"`
		Requests  uint64 `json:"requests"`
	}{q, processed, requests})
}

func profilingRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

func run() error {
	optimized := flag.Bool("optimized", false, "compile the constant regexp once, outside requests")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected arguments; use -help")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	appCtx, cancelApp := context.WithCancel(context.Background())
	defer cancelApp()
	app := newDemoApp(appCtx, *optimized)
	runtime.SetBlockProfileRate(1_000_000)
	runtime.SetMutexProfileFraction(10)
	defer runtime.SetBlockProfileRate(0)
	defer runtime.SetMutexProfileFraction(0)
	application := &http.Server{
		Addr: "127.0.0.1:8080", Handler: app.routes(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	profiles := &http.Server{
		Addr: "127.0.0.1:6060", Handler: profilingRoutes(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	errs := make(chan error, 2)
	for _, srv := range []*http.Server{application, profiles} {
		go func(s *http.Server) { errs <- s.ListenAndServe() }(srv)
	}
	log.Printf("demo: http://127.0.0.1:8080/search?q=go (optimized=%t)", *optimized)
	log.Print("pprof: http://127.0.0.1:6060/debug/pprof/; stop with Ctrl+C")
	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}
	cancelApp()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	for _, srv := range []*http.Server{application, profiles} {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			_ = srv.Close()
			if runErr == nil {
				runErr = err
			}
		}
	}
	app.workers.Wait()
	return runErr
}

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
