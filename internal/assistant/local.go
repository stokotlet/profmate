package assistant

import (
	"fmt"
	"strings"

	"github.com/stokotlet/profmate/internal/model"
)

func LocalAnswer(a model.Analysis, question string) string {
	prefix := "Офлайн-разбор по правилам (без ИИ).\n\n"
	q := strings.ToLower(question)
	switch {
	case contains(q, "flat", "cum", "флэт", "кум"):
		return prefix + "Flat — измеренный вклад самой функции. Cum — вклад этой функции вместе с её вызовами. Высокий cum помогает найти путь к дорогой операции; суммировать cum разных строк нельзя: стеки пересекаются.\n\n" + metricEvidence(a, "cpu")
	case contains(q, "утеч", "памят", "memory", "heap", "alloc", "мегабайт", "байт", "gc"):
		return prefix + "Наблюдение: inuse_space/inuse_objects описывают удерживаемые байты/объекты в heap-снимке; alloc_space/alloc_objects — выделения, включая уже освобождённые объекты. Heap зависит от последней завершённой сборки мусора.\n\n" + memoryEvidence(a) + "\n\nГипотеза: частые временные выделения могут создавать нагрузку на GC. Рост между двумя снимками сам по себе не доказывает утечку.\n\nСледующий эксперимент: повторить одинаковую нагрузку несколько раз и сравнить удерживаемую память после завершения нагрузки и GC. Одинаковые условия замера обязательны."
	case contains(q, "горутин", "goroutine", "goroutines"):
		return prefix + "Наблюдение: профиль goroutine показывает число горутин и их стеки в момент снимка.\n\n" + metricEvidence(a, "goroutine") + "\n\nГипотеза: повторяющийся стек ожидания может быть как нормальным пулом работников, так и застрявшими горутинами. Один снимок не доказывает утечку.\n\nСледующий эксперимент: сравнить число и стеки при одинаковой нагрузке и после её завершения; проверить, возвращается ли число к исходному уровню."
	case contains(q, "mutex", "block", "мьютекс", "блокиров", "ожидан", "канал"):
		return prefix + "Наблюдение: block показывает записанные ожидания синхронизации, mutex — конкуренцию за mutex с указанием стеков владельцев при освобождении. Эти показатели не являются задержкой HTTP-запроса.\n\n" + metricEvidence(a, "block") + "\n" + metricEvidence(a, "mutex") + "\n\nПустой профиль может означать, что сбор выключен. Он включается в приложении через runtime.SetBlockProfileRate и runtime.SetMutexProfileFraction; стандартный HTTP endpoint не включает его удалённо.\n\nСледующий эксперимент: включить выборочный сбор, повторить нагрузку и проверить критические секции в самых частых стеках."
	case contains(q, "cpu", "процессор", "regexp", "тормоз", "медлен", "задерж", "latency"):
		return prefix + "Наблюдение: CPU-профиль содержит сэмплы выполнения на процессоре. Он не измеряет полную задержку запроса и не показывает всё время ожидания сети.\n\n" + metricEvidence(a, "cpu") + "\n\nГипотеза: функции с высоким flat выполняют дорогую работу; высокий cum помогает найти вызывающий код. Без исходников и контекста нельзя выбрать конкретное исправление.\n\nСледующий эксперимент: проверить самый заметный путь вызовов, сделать одно изменение и повторить ту же нагрузку. Задержку и throughput сравнить отдельно по нагрузочному тесту."
	default:
		var out strings.Builder
		out.WriteString(prefix)
		if len(a.Findings) > 0 {
			out.WriteString("С чего начать — сохранённые находки анализатора:\n")
			for _, f := range a.Findings[:min(len(a.Findings), 3)] {
				fmt.Fprintf(&out, "\n[%s] %s\n", f.ID, f.Title)
				for _, evidence := range f.Evidence[:min(len(f.Evidence), 3)] {
					fmt.Fprintf(&out, "  Наблюдение: %s\n", evidence)
				}
				if f.Hypothesis != "" {
					fmt.Fprintf(&out, "  Гипотеза: %s\n", f.Hypothesis)
				}
				fmt.Fprintf(&out, "  Следующий эксперимент: %s\n", f.NextStep)
			}
		} else {
			out.WriteString("Готовых находок пока нет. Начните с CPU, удерживаемой памяти и числа горутин:\n\n")
			out.WriteString(metricEvidence(a, "cpu") + "\n" + memoryEvidence(a) + "\n" + metricEvidence(a, "goroutine") + "\n")
		}
		out.WriteString("\nМожно спросить про CPU, память/утечку, goroutines, block/mutex или flat/cum. Для свободного диалога подключите Ollama и задайте PROFMATE_MODEL.")
		return out.String()
	}
}

func contains(q string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(q, needle) {
			return true
		}
	}
	return false
}

func memoryEvidence(a model.Analysis) string {
	var lines []string
	for _, m := range a.Metrics {
		if contains(m.SampleType, "inuse_", "alloc_") || m.Kind == "heap" || m.Kind == "allocs" {
			lines = append(lines, describeMetric(m))
			if len(lines) == 6 {
				break
			}
		}
	}
	if len(lines) == 0 {
		return "Доступных метрик памяти нет; сначала нужен успешный сбор профиля."
	}
	return strings.Join(lines, "\n")
}

func metricEvidence(a model.Analysis, kind string) string {
	var lines []string
	for _, m := range a.Metrics {
		if m.Kind == kind || (kind == "goroutine" && m.SampleType == "goroutine") {
			lines = append(lines, describeMetric(m))
			if len(lines) == 3 {
				break
			}
		}
	}
	if len(lines) == 0 {
		return fmt.Sprintf("Доступных метрик %s нет; отсутствие данных не означает отсутствие проблемы.", kind)
	}
	return strings.Join(lines, "\n")
}

func describeMetric(m model.Metric) string {
	var out strings.Builder
	fmt.Fprintf(&out, "[%s] %s/%s: %d %s (область: %s)", m.ID, m.Kind, m.SampleType, m.Total, m.Unit, m.Scope)
	if m.Before != nil {
		fmt.Fprintf(&out, "; начальный снимок: %d %s", *m.Before, m.Unit)
	}
	if len(m.Top) > 0 {
		out.WriteString(". Функции:")
		for _, f := range m.Top[:min(len(m.Top), 3)] {
			fmt.Fprintf(&out, " %s (flat=%d, cum=%d %s);", f.Name, f.Flat, f.Cum, m.Unit)
		}
	}
	if m.Note != "" {
		out.WriteString(". " + m.Note)
	}
	return out.String()
}
