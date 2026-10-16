package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stokotlet/profmate/internal/model"
	"github.com/stokotlet/profmate/internal/testhttp"
)

func TestAskSendsNativeChatWithMeasuredContext(t *testing.T) {
	a := model.Analysis{Metrics: []model.Metric{{ID: "cpu.total", Kind: "cpu", SampleType: "cpu", Unit: "nanoseconds", Total: 42}}}
	client := testhttp.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/api/chat" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected authentication and JSON content type")
		}
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Model != "test-model" || request.Stream {
			t.Errorf("model/stream = %q/%v", request.Model, request.Stream)
		}
		if len(request.Messages) != 5 {
			t.Errorf("messages = %d, want 5", len(request.Messages))
			return
		}
		roles := []string{"system", "user", "user", "assistant", "user"}
		for i, role := range roles {
			if request.Messages[i].Role != role {
				t.Errorf("role %d = %q, want %q", i, request.Messages[i].Role, role)
			}
		}
		if !strings.Contains(request.Messages[0].Content, "недоверенные данные") || !strings.Contains(request.Messages[0].Content, "Гипотеза") {
			t.Error("system instructions lack evidence/data boundaries")
		}
		if !strings.Contains(request.Messages[1].Content, "cpu.total") || !strings.Contains(request.Messages[1].Content, "main.go: func main") {
			t.Error("missing measured context or source")
		}
		if request.Messages[4].Content != "Почему CPU?" {
			t.Error("missing final question")
		}
		io.WriteString(w, `{"message":{"role":"assistant","content":"  Наблюдение: [cpu.total] 42 ns.  "},"done":true}`)
	}))
	c := Config{URL: "http://ollama.example/prefix/", Model: "test-model", APIKey: "test-key", Client: client}
	history := []model.Message{{Role: "system", Content: "untrusted system message"}, {Role: "user", Content: "привет"}, {Role: "assistant", Content: "Здравствуйте"}}
	got, err := c.Ask(context.Background(), a, "main.go: func main", history, "Почему CPU?")
	if err != nil || got != "Наблюдение: [cpu.total] 42 ns." {
		t.Fatalf("Ask() = %q, %v", got, err)
	}
}

func TestAskResponseErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"status", 401, `{"error":"test-secret"}`, "HTTP 401"},
		{"server-error", 200, `{"error":"test-secret"}`, "ошибке генерации"},
		{"no-content", 200, `{"message":{"content":"  "}}`, "пустой ответ"},
		{"malformed", 200, "{broken", "некорректный JSON"},
		{"too-large", 200, strings.Repeat("x", maxResponseBytes+1), "превышает 1 MiB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := testhttp.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			_, err := (Config{URL: "http://ollama.example", Model: "test", APIKey: "test-secret", Client: client}).Ask(context.Background(), model.Analysis{}, "", nil, "вопрос")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "test-secret") {
				t.Error("error disclosed credential")
			}
		})
	}
}

func TestAskDoesNotForwardContextOnRedirect(t *testing.T) {
	requests := 0
	client := testhttp.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Location", "http://elsewhere.example/receive")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	_, err := (Config{URL: "http://ollama.example", Model: "test", Client: client}).Ask(context.Background(), model.Analysis{}, "private source", nil, "question")
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") || requests != 1 {
		t.Fatalf("requests=%d error=%v", requests, err)
	}
}

func TestAskCancellationAndTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })}
	_, err := (Config{Model: "test", Client: client}).Ask(ctx, model.Analysis{}, "", nil, "q")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > requestTimeout || time.Until(deadline) < 80*time.Second {
			t.Errorf("missing bounded timeout: %v, %v", deadline, ok)
		}
		return nil, errors.New("transport echoed test-secret")
	})
	_, err = (Config{Model: "test", APIKey: "test-secret", Client: client}).Ask(context.Background(), model.Analysis{}, "", nil, "q")
	if err == nil || strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("unexpected transport error: %v", err)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	client = testhttp.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	_, err = (Config{URL: "http://ollama.test", Model: "test", Client: client}).Ask(ctx, model.Analysis{}, "", nil, "q")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want caller deadline exceeded", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFromEnvAndDisabled(t *testing.T) {
	t.Setenv("PROFMATE_AI_URL", "")
	t.Setenv("OLLAMA_HOST", "")
	t.Setenv("PROFMATE_MODEL", "")
	t.Setenv("PROFMATE_AI_KEY", "test-key")
	c := FromEnv()
	if c.URL != "http://localhost:11434" || c.Enabled() || c.APIKey != "test-key" {
		t.Fatalf("unexpected default config: enabled=%v URL=%q", c.Enabled(), c.URL)
	}
	if _, err := c.Ask(context.Background(), model.Analysis{}, "", nil, "q"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("error = %v, want ErrDisabled", err)
	}
	t.Setenv("OLLAMA_HOST", "localhost:12345")
	t.Setenv("PROFMATE_MODEL", " llama3.2 ")
	if got := FromEnv(); got.URL != "localhost:12345" || got.Model != "llama3.2" || !got.Enabled() {
		t.Error("OLLAMA_HOST/model not applied")
	}
	t.Setenv("PROFMATE_AI_URL", "https://custom.example")
	if FromEnv().URL != "https://custom.example" {
		t.Error("PROFMATE_AI_URL should take precedence")
	}
}

func TestURLValidation(t *testing.T) {
	for _, base := range []string{"ftp://host", "http://user:secret@host", "http://host?key=secret", "http://host/#secret", "http://"} {
		if _, err := chatURL(base); err == nil {
			t.Errorf("accepted invalid base %q", base)
		}
	}
	if got, err := chatURL("localhost:11434"); err != nil || got != "http://localhost:11434/api/chat" {
		t.Fatalf("host normalization = %q, %v", got, err)
	}
}

func TestBoundedContextPreservesUTF8AndJSON(t *testing.T) {
	text := strings.Repeat("Ж", 100000)
	if got := clip(text, 7); len(got) != 6 || !utf8.ValidString(got) {
		t.Errorf("invalid clip: %q", got)
	}
	if got := clip("a\xffb", 3); got != "ab" {
		t.Errorf("invalid UTF8 not removed: %q", got)
	}
	a := model.Analysis{
		Metrics:  []model.Metric{{ID: "m1", Kind: "heap", Total: 123, Top: []model.Function{{Frame: model.Frame{Name: text, File: text}, Flat: 17}}, Stacks: []model.Stack{{Frames: []model.Frame{{Name: text}}, Value: 1}}}},
		Findings: []model.Finding{{ID: "f1", Title: text, Evidence: []string{text}, NextStep: text}},
		Notes:    []string{text},
	}
	b, err := boundedAnalysis(a)
	if err != nil || len(b) > maxAnalysisBytes || !json.Valid(b) {
		t.Fatalf("bounded analysis len=%d error=%v", len(b), err)
	}
	var got model.Analysis
	if err := json.Unmarshal(b, &got); err != nil || len(got.Metrics) != 1 || got.Metrics[0].Total != 123 {
		t.Fatalf("numeric facts missing: %+v, %v", got, err)
	}
	if len(a.Metrics[0].Top[0].Name) != len(text) || len(a.Metrics[0].Stacks) != 1 || a.Findings[0].Evidence[0] != text {
		t.Error("truncation mutated original analysis")
	}
	history := make([]model.Message, 30)
	for i := range history {
		history[i] = model.Message{Role: "user", Content: strings.Repeat("Щ", 2048)}
	}
	h := boundedHistory(history)
	total := 0
	for _, m := range h {
		total += len(m.Content)
		if !utf8.ValidString(m.Content) {
			t.Error("invalid UTF8 in history")
		}
	}
	if len(h) > 12 || total > maxHistoryBytes {
		t.Errorf("unbounded history: %d messages, %d bytes", len(h), total)
	}
}

func TestAskBoundsSourceAndQuestion(t *testing.T) {
	client := testhttp.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		contextJSON := strings.SplitN(request.Messages[1].Content, "\n", 2)[1]
		var facts struct {
			Source        string `json:"source"`
			SourceClipped bool   `json:"source_clipped"`
		}
		if err := json.Unmarshal([]byte(contextJSON), &facts); err != nil {
			t.Error(err)
		}
		if len(facts.Source) > maxSourceBytes || !facts.SourceClipped || !utf8.ValidString(facts.Source) {
			t.Error("unbounded source")
		}
		if len(request.Messages[len(request.Messages)-1].Content) > maxQuestionBytes {
			t.Error("unbounded question")
		}
		io.WriteString(w, `{"message":{"content":"ok"}}`)
	}))
	if _, err := (Config{URL: "http://ollama.example", Model: "test", Client: client}).Ask(context.Background(), model.Analysis{}, strings.Repeat("я", maxSourceBytes), nil, strings.Repeat("п", maxQuestionBytes)); err != nil {
		t.Fatal(err)
	}
}

func TestLocalAnswersAreHeuristicsAndUseFacts(t *testing.T) {
	a := model.Analysis{
		Metrics: []model.Metric{
			{ID: "cpu-1", Kind: "cpu", SampleType: "cpu", Total: 42, Unit: "nanoseconds", Top: []model.Function{{Frame: model.Frame{Name: "main.Search"}, Flat: 20, Cum: 42}}},
			{ID: "heap-1", Kind: "heap", SampleType: "inuse_space", Total: 128, Unit: "bytes"},
			{ID: "goroutine-1", Kind: "goroutine", SampleType: "goroutine", Total: 10, Unit: "count"},
		},
		Findings: []model.Finding{{ID: "finding-1", Title: "CPU path", Evidence: []string{"cpu-1"}, Hypothesis: "Проверить Search", NextStep: "Повторить нагрузку"}},
	}
	for _, question := range []string{"Что такое flat и cum?", "Есть утечка памяти?", "Почему goroutines растут?", "Что с mutex?", "Почему CPU?", "С чего начать?"} {
		got := LocalAnswer(a, question)
		if !strings.Contains(got, "Офлайн-разбор по правилам (без ИИ)") {
			t.Errorf("not labelled heuristic for %q", question)
		}
	}
	if got := LocalAnswer(a, "CPU"); !strings.Contains(got, "[cpu-1]") || !strings.Contains(got, "main.Search") || !strings.Contains(got, "не измеряет полную задержку") {
		t.Errorf("CPU facts/caveats missing: %s", got)
	}
	if got := LocalAnswer(a, "утечка"); !strings.Contains(got, "[heap-1]") || !strings.Contains(got, "не доказывает утечку") {
		t.Errorf("memory facts/caveats missing: %s", got)
	}
	if got := LocalAnswer(a, "начать"); !strings.Contains(got, "[finding-1]") || !strings.Contains(got, "Гипотеза") {
		t.Errorf("finding evidence missing: %s", got)
	}
}
