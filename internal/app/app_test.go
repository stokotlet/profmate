package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"github.com/profmate/profmate/internal/model"
	"github.com/profmate/profmate/internal/store"
	"github.com/profmate/profmate/internal/testhttp"
)

func TestInspectChatSaveAndResume(t *testing.T) {
	t.Setenv("PROFMATE_MODEL", "")
	t.Setenv("PROFMATE_TARGET_AUTH", "Bearer auth-secret")
	root := t.TempDir()
	sourceDir := t.TempDir()
	sourceFile := filepath.Join(sourceDir, "search.go")
	if err := os.WriteFile(sourceFile, []byte("package shop\nfunc SearchHandler() {\n println(42)\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer auth-secret" {
			t.Error("auth header missing")
		}
		kind := filepath.Base(r.URL.Path)
		if kind == "goroutine" && r.URL.Query().Get("debug") == "2" {
			fmt.Fprint(w, "goroutine 1 [running]:\nmain.main()\n")
			return
		}
		var types []*profile.ValueType
		var values []int64
		switch kind {
		case "profile":
			types = []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}
			values = []int64{100000000}
		case "heap":
			types = []*profile.ValueType{{Type: "inuse_space", Unit: "bytes"}, {Type: "inuse_objects", Unit: "count"}, {Type: "alloc_space", Unit: "bytes"}, {Type: "alloc_objects", Unit: "count"}}
			values = []int64{int64(requests) * 100, 10, int64(requests) * 1000, int64(requests) * 10}
		case "goroutine":
			types = []*profile.ValueType{{Type: "goroutine", Unit: "count"}}
			values = []int64{10}
		default:
			types = []*profile.ValueType{{Type: "contentions", Unit: "count"}, {Type: "delay", Unit: "nanoseconds"}}
			values = []int64{int64(requests), int64(requests) * 1000}
		}
		f := &profile.Function{ID: 1, Name: "shop.SearchHandler", Filename: sourceFile}
		l := &profile.Location{ID: 1, Line: []profile.Line{{Function: f, Line: 3}}}
		p := &profile.Profile{SampleType: types, Sample: []*profile.Sample{{Location: []*profile.Location{l}, Value: values}}, Function: []*profile.Function{f}, Location: []*profile.Location{l}, DurationNanos: int64(time.Second)}
		if err := p.Write(w); err != nil {
			t.Error(err)
		}
	})
	var out, errout bytes.Buffer
	input := "/top cpu\nЭто утечка памяти?\n/source " + sourceDir + "\n/code\n/save\n/exit\n"
	err := run(context.Background(), []string{"inspect", "http://pprof.test?token=query-secret", "--duration", "1s", "--out", root, "--goal", "Тормозит поиск", "--offline"}, strings.NewReader(input), &out, &errout, runtimeOptions{client: testhttp.NewClient(handler)})
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out.String(), errout.String())
	}
	if requests != 10 {
		t.Fatalf("requests=%d", requests)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatal(entries)
	}
	dir := filepath.Join(root, entries[0].Name())
	s, err := store.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Analysis.Metrics) != 10 || len(s.Messages) != 2 || s.SourceRoot != sourceDir {
		t.Fatalf("bad session: %+v", s)
	}
	for _, name := range []string{"session.json", "report.md", "chat.md"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("query-secret")) || bytes.Contains(data, []byte("auth-secret")) {
			t.Fatal("credential stored")
		}
	}
	if !strings.Contains(out.String(), "profmate>") || !strings.Contains(out.String(), "без ИИ") || !strings.Contains(out.String(), "func SearchHandler") {
		t.Fatal(out.String())
	}
	var resumed bytes.Buffer
	if err := Run(context.Background(), []string{"chat", dir, "--offline"}, strings.NewReader("Что значит flat и cum?\n/exit\n"), &resumed, &errout); err != nil {
		t.Fatal(err)
	}
	s, err = store.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 4 {
		t.Fatal("conversation was not resumed")
	}
}

func TestInspectWithOllama(t *testing.T) {
	t.Setenv("PROFMATE_MODEL", "test-model")
	t.Setenv("PROFMATE_AI_URL", "http://ollama.test")
	var out, errout bytes.Buffer
	called := false
	ai := testhttp.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		var request struct {
			Messages []model.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if len(request.Messages) < 3 || !strings.Contains(request.Messages[1].Content, "cpu.cpu") {
			t.Error("missing profile facts")
		}
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"Наблюдение: CPU-сэмплы получены. Гипотеза требует проверки."}}`)
	}))
	target := testhttp.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/profile") {
			p := &profile.Profile{SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}, Sample: []*profile.Sample{{Value: []int64{1000}}}}
			if err := p.Write(w); err != nil {
				t.Error(err)
			}
		} else {
			http.NotFound(w, r)
		}
	}))
	if err := run(context.Background(), []string{"inspect", "http://pprof.test", "--duration", "1s", "--out", t.TempDir(), "--no-chat"}, strings.NewReader(""), &out, &errout, runtimeOptions{client: target, aiClient: ai}); err != nil {
		t.Fatalf("%v %s", err, errout.String())
	}
	if !called || !strings.Contains(out.String(), "Гипотеза требует") {
		t.Fatal(out.String())
	}
}

func TestInspectRealRuntimeProfiles(t *testing.T) {
	t.Setenv("PROFMATE_MODEL", "")
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	for _, kind := range []string{"heap", "goroutine", "block", "mutex"} {
		mux.Handle("/debug/pprof/"+kind, pprof.Handler(kind))
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				regexp.MustCompile(`^[a-z0-9]+$`).MatchString("hello123")
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	root := t.TempDir()
	var out, errout bytes.Buffer
	err := run(context.Background(), []string{"inspect", "http://runtime.test", "--duration", "1s", "--out", root, "--no-chat", "--offline"}, strings.NewReader(""), &out, &errout, runtimeOptions{client: testhttp.NewClient(mux)})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	entries, _ := os.ReadDir(root)
	s, err := store.Load(filepath.Join(root, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Analysis.Metrics) != 10 {
		t.Fatalf("metrics=%d\n%s", len(s.Analysis.Metrics), out.String())
	}
	for _, c := range s.Captures {
		if c.Error != "" {
			t.Fatal(c.Error)
		}
	}
}

func TestInvalidArgsDoNotCreateSession(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"inspect", "http://localhost:6060", "--duration", "500ms", "--out", root}, strings.NewReader(""), &out, &out); err == nil {
		t.Fatal("expected validation failure")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal(entries)
	}
}
