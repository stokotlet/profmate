package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/profmate/profmate/internal/analyze"
	"github.com/profmate/profmate/internal/assistant"
	"github.com/profmate/profmate/internal/collector"
	"github.com/profmate/profmate/internal/model"
	"github.com/profmate/profmate/internal/render"
	"github.com/profmate/profmate/internal/source"
	"github.com/profmate/profmate/internal/store"
)

const Version = "0.1.0"

type runtimeOptions struct {
	client   *http.Client
	aiClient *http.Client
}

func Run(ctx context.Context, args []string, in io.Reader, out, errout io.Writer) error {
	return run(ctx, args, in, out, errout, runtimeOptions{})
}

func run(ctx context.Context, args []string, in io.Reader, out, errout io.Writer, opts runtimeOptions) error {
	if len(args) == 0 {
		help(out)
		return nil
	}
	switch args[0] {
	case "help", "--help", "-h":
		help(out)
		return nil
	case "version", "--version":
		fmt.Fprintln(out, "ProfMate", Version)
		return nil
	case "inspect":
		return inspect(ctx, args[1:], in, out, errout, opts)
	case "chat":
		return resume(ctx, args[1:], in, out, errout)
	case "report":
		if len(args) != 2 {
			return errors.New("использование: profmate report ПАПКА_СЕССИИ")
		}
		s, err := store.Load(args[1])
		if err != nil {
			return err
		}
		render.Text(out, s.Analysis)
		return nil
	default:
		return fmt.Errorf("неизвестная команда %q; profmate --help", args[0])
	}
}

type aiFlags struct {
	model, url string
	offline    bool
}

func (a *aiFlags) register(fs *flag.FlagSet) {
	c := assistant.FromEnv()
	fs.StringVar(&a.model, "model", c.Model, "имя установленной модели Ollama")
	fs.StringVar(&a.url, "ai-url", c.URL, "базовый адрес Ollama")
	fs.BoolVar(&a.offline, "offline", false, "разбор по правилам без ИИ")
}
func (a aiFlags) config() assistant.Config {
	c := assistant.FromEnv()
	c.Model = a.model
	c.URL = a.url
	if a.offline {
		c.Model = ""
	}
	return c
}

func inspect(ctx context.Context, args []string, in io.Reader, out, errout io.Writer, opts runtimeOptions) (returnErr error) {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(errout)
	var target, base, goal, sourceRoot string
	var duration time.Duration
	var noChat bool
	fs.DurationVar(&duration, "duration", 30*time.Second, "CPU-интервал: целое число секунд от 1 до 300")
	fs.StringVar(&base, "out", "./profmate-runs", "папка для сессий")
	fs.StringVar(&goal, "goal", "", "что исследуем")
	fs.StringVar(&sourceRoot, "source", "", "папка с исходниками соответствующей версии")
	fs.BoolVar(&noChat, "no-chat", false, "сохранить отчёт без диалога")
	var ai aiFlags
	ai.register(fs)
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if target == "" && fs.NArg() == 1 {
		target = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return errors.New("укажи один адрес pprof; profmate inspect URL --duration 30s")
	}
	if target == "" {
		return errors.New("укажи адрес: profmate inspect http://localhost:6060")
	}
	if duration < time.Second || duration > 300*time.Second || duration%time.Second != 0 {
		return errors.New("--duration: целое число секунд от 1 до 300")
	}
	normalized, err := collector.NormalizeURL(target)
	if err != nil {
		return err
	}
	if len(goal) > 8192 {
		return errors.New("слишком длинное описание задачи (максимум 8 KiB)")
	}
	input := newInput(in)
	if goal == "" && !noChat {
		fmt.Fprint(out, "Что исследуем? (Enter — общий разбор)\n> ")
		line, err := input.line(ctx)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		goal = strings.TrimSpace(line)
	}
	if len(goal) > 8192 {
		return errors.New("слишком длинное описание задачи (максимум 8 KiB)")
	}
	dir, err := newSessionDir(base)
	if err != nil {
		return err
	}
	s := &model.Session{Version: 1, ID: filepath.Base(dir), Target: redactURL(normalized), Goal: goal, StartedAt: time.Now().UTC(), Duration: duration}
	defer func() {
		if s.FinishedAt.IsZero() {
			s.FinishedAt = time.Now().UTC()
		}
		if err := store.Save(dir, s); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("сохранение сессии: %w", err))
		}
	}()
	fmt.Fprintf(out, "\nАдрес: %s\nСессия: %s\n", s.Target, dir)
	fmt.Fprintf(out, "Снимаю профили; CPU-интервал %s. Воспроизведи проблему во время замера.\n", duration)
	headers := map[string]string{}
	if auth := os.Getenv("PROFMATE_TARGET_AUTH"); auth != "" {
		headers["Authorization"] = auth
	}
	progress := strings.NewReplacer("Collecting ", "  Снимаю ", "(before)", "(начальный снимок)", "(after)", "(конечный снимок)", "(window)", "(интервал)")
	captures, collectErr := collector.Collect(ctx, dir, collector.Options{URL: normalized, Duration: duration, Headers: headers, Client: opts.client, Progress: func(s string) { fmt.Fprintln(out, progress.Replace(s)) }})
	s.Captures = captures
	s.FinishedAt = time.Now().UTC()
	s.Analysis = analyze.Build(dir, captures)
	if err := store.Save(dir, s); err != nil {
		return err
	}
	render.Text(out, s.Analysis)
	if collectErr != nil {
		return fmt.Errorf("сбор завершён с ошибкой; доступные данные в %s: %w", dir, collectErr)
	}
	if len(s.Analysis.Metrics) == 0 {
		return fmt.Errorf("нет профилей с подходящими метриками; подробности в %s", dir)
	}
	var excerpts string
	if sourceRoot != "" {
		excerpts = attachSource(out, s, sourceRoot)
	}
	cfg := ai.config()
	cfg.Client = opts.aiClient
	if cfg.Enabled() {
		question := "Объясни три главные находки. Отделяй факты от гипотез и предложи следующий эксперимент. Задача пользователя: " + goal
		if err := answer(ctx, out, s, cfg, excerpts, question); err != nil {
			fmt.Fprintln(errout, err)
		}
	} else {
		fmt.Fprintln(out, "\nИИ не подключён. Доступны измерения и справка по правилам. Для ИИ задай PROFMATE_MODEL (Ollama).")
	}
	if err := store.Save(dir, s); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nОтчёт: %s\n", filepath.Join(dir, "report.md"))
	if noChat {
		return ctx.Err()
	}
	return chat(ctx, input, out, errout, dir, s, cfg, excerpts)
}

func resume(ctx context.Context, args []string, in io.Reader, out, errout io.Writer) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(errout)
	var ai aiFlags
	ai.register(fs)
	var dir string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		dir = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if dir == "" && fs.NArg() == 1 {
		dir = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return errors.New("укажи одну папку сессии")
	}
	if dir == "" {
		return errors.New("использование: profmate chat ПАПКА_СЕССИИ")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	dir = abs
	s, err := store.Load(dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Сессия %s\n", s.ID)
	render.Text(out, s.Analysis)
	var excerpts string
	if s.SourceRoot != "" {
		excerpts = attachSource(out, s, s.SourceRoot)
	}
	return chat(ctx, newInput(in), out, errout, dir, s, ai.config(), excerpts)
}

func chat(ctx context.Context, input *consoleInput, out, errout io.Writer, dir string, s *model.Session, cfg assistant.Config, excerpts string) (returnErr error) {
	defer func() {
		if err := store.Save(dir, s); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	fmt.Fprintln(out, "\nПиши вопросы обычным текстом. Команды: /help, /top, /source ПУТЬ, /code, /save, /exit.")
	if !cfg.Enabled() {
		fmt.Fprintln(out, "Режим: офлайн-справка по правилам, без ИИ.")
	}
	for {
		fmt.Fprint(out, "\nprofmate> ")
		line, err := input.line(ctx)
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(out)
			return nil
		}
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		command, arg, _ := strings.Cut(line, " ")
		arg = strings.TrimSpace(arg)
		switch command {
		case "/exit", "/quit":
			fmt.Fprintln(out, "Сессия сохранена.")
			return nil
		case "/help":
			if arg == "profiling" {
				profilingHelp(out)
			} else {
				chatHelp(out)
			}
		case "/top":
			render.Top(out, s.Analysis, arg)
		case "/report":
			render.Text(out, s.Analysis)
		case "/profiles":
			for _, c := range s.Captures {
				status := c.File
				if c.Error != "" {
					status = "ошибка: " + c.Error
				}
				fmt.Fprintf(out, "%s/%s [%s — %s] %s\n", c.Kind, c.Phase, c.StartedAt.Format(time.RFC3339), c.FinishedAt.Format(time.RFC3339), status)
			}
		case "/source":
			if arg == "" {
				fmt.Fprintln(out, "Использование: /source ПУТЬ")
				continue
			}
			candidate := attachSource(out, s, strings.Trim(arg, "\"'"))
			if candidate != "" {
				excerpts = candidate
			}
		case "/code":
			if excerpts == "" {
				fmt.Fprintln(out, "Исходники не подключены. Используй /source ПУТЬ.")
			} else {
				fmt.Fprintln(out, excerpts)
			}
		case "/save":
			if err := store.Save(dir, s); err != nil {
				return err
			}
			fmt.Fprintf(out, "Сохранены: %s, report.md, chat.md и profiles/\n", filepath.Join(dir, "session.json"))
		default:
			if strings.HasPrefix(command, "/") {
				fmt.Fprintln(out, "Неизвестная команда. Используй /help.")
				continue
			}
			if err := answer(ctx, out, s, cfg, excerpts, line); err != nil {
				fmt.Fprintln(errout, err)
			}
		}
		if err := store.Save(dir, s); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func answer(ctx context.Context, out io.Writer, s *model.Session, cfg assistant.Config, excerpts, question string) error {
	var result string
	var askErr error
	if cfg.Enabled() {
		fmt.Fprintln(out, "\nИИ разбирает измерения...")
		result, askErr = cfg.Ask(ctx, s.Analysis, excerpts, s.Messages, question)
		if askErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			result = "ИИ недоступен. " + assistant.LocalAnswer(s.Analysis, question)
		}
	} else {
		result = assistant.LocalAnswer(s.Analysis, question)
	}
	fmt.Fprintln(out, "\n"+result)
	s.Messages = append(s.Messages, model.Message{Role: "user", Content: question}, model.Message{Role: "assistant", Content: result})
	return askErr
}

func attachSource(out io.Writer, s *model.Session, path string) string {
	excerpts, err := source.Extract(path, s.Analysis)
	if err != nil {
		fmt.Fprintln(out, "Исходники:", err)
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintln(out, "Исходники:", err)
		return ""
	}
	s.SourceRoot = abs
	fmt.Fprintln(out, "Подключены фрагменты исходников из", abs)
	fmt.Fprintln(out, "Соответствие сборке автоматически не проверено. При вопросах фрагменты передаются настроенной модели. /code — посмотреть их.")
	return excerpts
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[адрес скрыт]"
	}
	q := u.Query()
	for k := range q {
		q.Set(k, "REDACTED")
	}
	u.RawQuery = q.Encode()
	u.User = nil
	return u.String()
}

func newSessionDir(base string) (string, error) {
	abs, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return "", err
	}
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(abs, time.Now().UTC().Format("20060102T150405Z")+"-"+hex.EncodeToString(suffix[:]))
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

type consoleInput struct {
	scanner *bufio.Scanner
	done    bool
}

func newInput(r io.Reader) *consoleInput {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 64<<10)
	return &consoleInput{scanner: s}
}
func (i *consoleInput) line(ctx context.Context) (string, error) {
	if i.done {
		return "", io.EOF
	}
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		if i.scanner.Scan() {
			ch <- result{line: i.scanner.Text()}
		} else {
			err := i.scanner.Err()
			if err == nil {
				err = io.EOF
			}
			ch <- result{err: err}
		}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		if r.err != nil {
			i.done = true
		}
		return r.line, r.err
	}
}
