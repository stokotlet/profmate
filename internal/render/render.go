package render

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/profmate/profmate/internal/analyze"
	"github.com/profmate/profmate/internal/model"
)

func Scope(scope string) string {
	switch scope {
	case "snapshot":
		return "снимок"
	case "delta":
		return "изменение между снимками"
	case "interval":
		return "интервал CPU"
	case "cumulative":
		return "накоплено с запуска"
	default:
		return scope
	}
}

func Text(w io.Writer, a model.Analysis) {
	fmt.Fprintln(w, "\nРезультаты замера")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, m := range a.Metrics {
		value := analyze.FormatValue(m.Total, m.Unit)
		if m.Before != nil {
			value = analyze.FormatValue(*m.Before, m.Unit) + " → " + value
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", m.ID, value, Scope(m.Scope))
	}
	tw.Flush()
	if len(a.Metrics) == 0 {
		fmt.Fprintln(w, "  Нет пригодных для анализа профилей.")
	}
	for _, f := range a.Findings {
		fmt.Fprintf(w, "\n[%s] %s\n", f.ID, f.Title)
		for _, evidence := range f.Evidence {
			fmt.Fprintf(w, "  Факт: %s\n", evidence)
		}
		if f.Hypothesis != "" {
			fmt.Fprintf(w, "  Гипотеза: %s\n", f.Hypothesis)
		}
		fmt.Fprintf(w, "  Проверка: %s\n", f.NextStep)
	}
	notes := append([]string(nil), a.Notes...)
	for _, m := range a.Metrics {
		if m.Note != "" {
			notes = append(notes, m.Note)
		}
	}
	if len(notes) > 0 {
		fmt.Fprintln(w, "\nУсловия и ограничения:")
	}
	seen := map[string]bool{}
	for _, note := range notes {
		if !seen[note] {
			fmt.Fprintf(w, "  • %s\n", note)
			seen[note] = true
		}
	}
}

func Top(w io.Writer, a model.Analysis, kind string) {
	found := false
	for _, m := range a.Metrics {
		if kind != "" && m.Kind != kind {
			continue
		}
		found = true
		fmt.Fprintf(w, "\n%s (%s)\n", m.ID, Scope(m.Scope))
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "Функция\tflat\tcum")
		for _, f := range m.Top {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", f.Name, analyze.FormatValue(f.Flat, m.Unit), analyze.FormatValue(f.Cum, m.Unit))
		}
		tw.Flush()
		for _, s := range m.Stacks {
			names := make([]string, 0, len(s.Frames))
			for _, f := range s.Frames {
				names = append(names, f.Name)
			}
			fmt.Fprintf(w, "  %s: %s\n", analyze.FormatValue(s.Value, m.Unit), strings.Join(names, " → "))
		}
	}
	if !found {
		fmt.Fprintln(w, "Нет метрик для этого типа профиля.")
	}
}

func cell(s string) string { return strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ").Replace(s) }

func Markdown(s *model.Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# ProfMate — отчёт\n\nСессия: `%s`  \nАдрес: `%s`  \nНачало: %s  \nОкончание: %s\n", cell(s.ID), cell(s.Target), s.StartedAt.Format("2006-01-02T15:04:05Z07:00"), s.FinishedAt.Format("2006-01-02T15:04:05Z07:00"))
	if s.Goal != "" {
		fmt.Fprintf(&b, "\nЗадача: %s\n", s.Goal)
	}
	fmt.Fprintln(&b, "\n## Измерения\n\n| Метрика | Значение | Область |\n|---|---:|---|")
	for _, m := range s.Analysis.Metrics {
		value := analyze.FormatValue(m.Total, m.Unit)
		if m.Before != nil {
			value = analyze.FormatValue(*m.Before, m.Unit) + " → " + value
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", cell(m.ID), value, cell(Scope(m.Scope)))
	}
	for _, f := range s.Analysis.Findings {
		fmt.Fprintf(&b, "\n## [%s] %s\n\n", f.ID, f.Title)
		for _, e := range f.Evidence {
			fmt.Fprintf(&b, "- Факт: %s\n", e)
		}
		if f.Hypothesis != "" {
			fmt.Fprintf(&b, "\nГипотеза: %s\n", f.Hypothesis)
		}
		fmt.Fprintf(&b, "\nСледующая проверка: %s\n", f.NextStep)
	}
	fmt.Fprintln(&b, "\n## Функции и стеки")
	for _, m := range s.Analysis.Metrics {
		fmt.Fprintf(&b, "\n### %s\n\n%s\n\n| Функция | Файл | flat | cum |\n|---|---|---:|---:|\n", m.ID, m.Note)
		for _, f := range m.Top {
			fmt.Fprintf(&b, "| %s | %s:%d | %s | %s |\n", cell(f.Name), cell(f.File), f.Line, analyze.FormatValue(f.Flat, m.Unit), analyze.FormatValue(f.Cum, m.Unit))
		}
		for _, stack := range m.Stacks {
			names := make([]string, 0, len(stack.Frames))
			for _, frame := range stack.Frames {
				names = append(names, frame.Name)
			}
			fmt.Fprintf(&b, "\n- %s: %s\n", analyze.FormatValue(stack.Value, m.Unit), strings.Join(names, " → "))
		}
	}
	fmt.Fprint(&b, "\n## Условия и ограничения\n\n")
	for _, n := range s.Analysis.Notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	fmt.Fprintln(&b, "\n## Файлы профилей\n\n| Тип | Фаза | Начало | Окончание | Файл / ошибка |\n|---|---|---|---|---|")
	for _, c := range s.Captures {
		result := c.File
		if c.Error != "" {
			result = c.Error
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", c.Kind, c.Phase, c.StartedAt.Format("15:04:05.000Z07:00"), c.FinishedAt.Format("15:04:05.000Z07:00"), cell(result))
	}
	return b.String()
}

func Chat(s *model.Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# ProfMate — переписка %s\n", s.ID)
	for _, m := range s.Messages {
		name := "Пользователь"
		if m.Role == "assistant" {
			name = "ProfMate"
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", name, m.Content)
	}
	return b.String()
}
