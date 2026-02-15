package app

import (
	"fmt"
	"io"
)

func chatHelp(w io.Writer) {
	fmt.Fprintln(w, "Обычный текст — вопрос по профилям.\n/top [cpu|heap|allocs|goroutine|block|mutex] — функции и стеки\n/profiles — файлы и время замеров\n/source ПУТЬ — подключить исходники соответствующей версии\n/code — показать подключённые фрагменты\n/report — повторить разбор\n/save — сохранить отчёт и переписку\n/help profiling — включение pprof, block и mutex\n/exit — выйти")
}

func profilingHelp(w io.Writer) {
	fmt.Fprintln(w, "В приложении должен быть включён net/http/pprof и запущен HTTP-сервер\nс его обработчиками, например отдельный сервер на localhost:6060.\n\nBlock и mutex включаются в самом приложении, например:\n    runtime.SetBlockProfileRate(10_000_000) // выборка ожиданий, 10 ms\n    runtime.SetMutexProfileFraction(10)    // примерно 1 из 10 событий\n\nПустые block/mutex не позволяют узнать, был ли сбор выключен.\nОбычный HTTP endpoint pprof не включает эти режимы удалённо.\n\nCPU — сэмплы работы процессора, а не время HTTP-запроса.\nHeap — данные с учётом последней завершённой сборки мусора; GC не вызывается.\nAllocations — выделения, включая уже освобождённые объекты.\nИзменения между снимками не доказывают утечку.\nПример сервиса с pprof: go run ./cmd/demo")
}

func help(w io.Writer) {
	fmt.Fprintf(w, "ProfMate %s — профили Go с объяснениями в консоли\n\n  profmate inspect URL [--duration 30s] [--goal \"что тормозит\"]\n      [--source ПАПКА] [--out ./profmate-runs] [--no-chat]\n      [--model ИМЯ] [--ai-url http://localhost:11434] [--offline]\n  profmate chat ПАПКА_СЕССИИ [--model ИМЯ] [--offline]\n  profmate report ПАПКА_СЕССИИ\n  profmate version\n\nПо умолчанию: CPU, heap (in-use и allocations), goroutine, block, mutex.\nСнимки берутся до и после CPU-интервала. Частичные ошибки сохраняются.\nВопросы задаются после замера в строке profmate>.\n\nИИ через Ollama: PROFMATE_MODEL, PROFMATE_AI_URL, PROFMATE_AI_KEY.\nБез модели работает локальный разбор и справка, помеченные «без ИИ».\nPROFMATE_TARGET_AUTH — значение заголовка Authorization для pprof;\nоно используется при запросах и не сохраняется в сессию.\n", Version)
}
