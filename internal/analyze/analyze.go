package analyze

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/pprof/profile"
	"github.com/stokotlet/profmate/internal/model"
)

const maxProfileBytes = 64 << 20

type loaded struct {
	capture model.Capture
	profile *profile.Profile
}

type weightedStack struct {
	frames []model.Frame
	value  int64
}

type measure struct {
	unit   string
	total  int64
	stacks map[string]*weightedStack
}

func Build(dir string, captures []model.Capture) model.Analysis {
	a := model.Analysis{}
	profiles := make([]loaded, 0, len(captures))
	for _, c := range captures {
		if c.Error != "" {
			a.Notes = append(a.Notes, fmt.Sprintf("%s/%s: %s", c.Kind, c.Phase, c.Error))
			continue
		}
		if c.File == "" || c.Kind == "goroutine-debug" || c.Kind == "goroutine-debug2" || c.Kind == "goroutine_text" || strings.HasSuffix(c.File, ".txt") {
			continue
		}
		p, err := readProfile(dir, c.File)
		if err != nil {
			a.Notes = append(a.Notes, fmt.Sprintf("%s/%s: не удалось прочитать профиль: %v", c.Kind, c.Phase, err))
			continue
		}
		profiles = append(profiles, loaded{c, p})
	}
	find := func(kind, phase string) *loaded {
		for i := len(profiles) - 1; i >= 0; i-- {
			p := &profiles[i]
			if p.capture.Kind == kind && (phase == "" || p.capture.Phase == phase) {
				return p
			}
		}
		return nil
	}
	if cpu := find("cpu", ""); cpu != nil {
		if m, err := extract(cpu.profile, "cpu", "nanoseconds"); err != nil {
			a.Notes = append(a.Notes, "CPU: "+err.Error())
		} else {
			note := "Выборка процессорного времени всех потоков. Не показывает время ответа запроса или длительность ожидания."
			if cpu.profile.DurationNanos > 0 {
				note += " Длительность профиля: " + time.Duration(cpu.profile.DurationNanos).String() + "."
			}
			metric := render("cpu", "cpu", "interval", m, nil, note)
			a.Metrics = append(a.Metrics, metric)
			if m.total == 0 {
				a.Notes = append(a.Notes, "CPU: сэмплы не записаны. Повторите замер во время воспроизведения нагрузки.")
			} else if !hasSymbols(metric) {
				a.Notes = append(a.Notes, "CPU: в сэмплах нет имён функций. Для символизации исходного профиля нужен бинарный файл той же сборки.")
			}
		}
	}

	heapAfter, heapBefore := find("heap", "after"), find("heap", "before")
	if heapAfter != nil {
		for _, spec := range []struct{ sample, unit string }{{"inuse_space", "bytes"}, {"inuse_objects", "count"}} {
			appendSnapshot(&a, "heap", spec.sample, spec.unit, heapAfter, heapBefore,
				"Выборка удерживаемых выделений по последнему завершённому циклу GC; принудительный GC не запускался. Два снимка не доказывают утечку.")
		}
	}
	allocAfter, allocBefore := find("allocs", "after"), find("allocs", "before")
	if allocAfter == nil {
		allocAfter, allocBefore = heapAfter, heapBefore
	}
	if allocAfter != nil {
		for _, spec := range []struct{ sample, unit string }{{"alloc_space", "bytes"}, {"alloc_objects", "count"}} {
			appendCounter(&a, "allocs", spec.sample, spec.unit, allocAfter, allocBefore,
				"Выборка накопленных выделений, включая освобождённые объекты. Данные могут обновляться с задержкой до GC; разница снимков — оценка выделений, а не точное значение на запрос.")
		}
	}
	if after := find("goroutine", "after"); after != nil {
		appendSnapshot(&a, "goroutine", "goroutine", "count", after, find("goroutine", "before"),
			"Количество горутин и повторяющиеся стеки отражают момент замера. Рост сам по себе не доказывает утечку; нужно проверить состояния и завершение работы горутин.")
	}
	for _, kind := range []string{"block", "mutex"} {
		if after := find(kind, "after"); after != nil {
			for _, spec := range []struct{ sample, unit string }{{"delay", "nanoseconds"}, {"contentions", "count"}} {
				note := "Выборка ожидания синхронизации. Сумма может превышать длительность замера, когда несколько горутин ожидают одновременно."
				if kind == "mutex" {
					note = "Ожидание mutex относится к завершению критической секции владельца блокировки (часто Unlock), а не к ожидающему вызову Lock. Сумма может превышать длительность замера."
				}
				appendCounter(&a, kind, spec.sample, spec.unit, after, find(kind, "before"), note)
			}
		}
	}
	if len(a.Metrics) > 0 {
		a.Notes = append(a.Notes, "Снимки сняты последовательно до и после CPU-профиля. Точные интервалы указаны во времени начала и окончания каждого сбора.")
	}
	a.Findings = findings(a.Metrics)
	return a
}

func readProfile(dir, name string) (*profile.Profile, error) {
	if !filepath.IsLocal(name) {
		return nil, fmt.Errorf("путь профиля должен находиться внутри папки сессии")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("профиль должен быть обычным файлом")
	}
	if info.Size() > maxProfileBytes {
		return nil, fmt.Errorf("профиль превышает ограничение %d MiB", maxProfileBytes>>20)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxProfileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxProfileBytes {
		return nil, fmt.Errorf("профиль превышает ограничение %d MiB", maxProfileBytes>>20)
	}
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		z, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		decoded, readErr := io.ReadAll(io.LimitReader(z, maxProfileBytes+1))
		closeErr := z.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(decoded) > maxProfileBytes {
			return nil, fmt.Errorf("распакованный профиль превышает ограничение %d MiB", maxProfileBytes>>20)
		}
		b = decoded
	}
	// ParseData would gunzip again without a size limit.
	p, err := profile.ParseUncompressed(b)
	if err != nil {
		return nil, fmt.Errorf("ошибка формата профиля: %w", err)
	}
	if err := p.CheckValid(); err != nil {
		return nil, fmt.Errorf("некорректный профиль: %w", err)
	}
	return p, nil
}

func appendSnapshot(a *model.Analysis, kind, sample, unit string, after, before *loaded, note string) {
	m, err := extract(after.profile, sample, unit)
	if err != nil {
		a.Notes = append(a.Notes, kind+": "+err.Error())
		return
	}
	var totalBefore *int64
	if before != nil {
		if reason := compatible(before.profile, after.profile); reason != "" {
			note += " Начальный снимок не учитывается: " + reason + "."
		} else if bm, err := extract(before.profile, sample, unit); err == nil {
			v := bm.total
			totalBefore = &v
		} else {
			note += " Начальный снимок не учитывается: " + err.Error() + "."
		}
	}
	a.Metrics = append(a.Metrics, render(kind, sample, "snapshot", m, totalBefore, note))
}

func appendCounter(a *model.Analysis, kind, sample, unit string, after, before *loaded, note string) {
	m, err := extract(after.profile, sample, unit)
	if err != nil {
		a.Notes = append(a.Notes, kind+": "+err.Error())
		return
	}
	scope := "cumulative"
	reason := "начальный снимок недоступен"
	if before != nil {
		reason = compatible(before.profile, after.profile)
		if reason == "" {
			bm, err := extract(before.profile, sample, unit)
			if err != nil {
				reason = err.Error()
			} else {
				var dm measure
				dm, reason = subtract(bm, m)
				if reason == "" {
					note += " Накопленное значение в начале: " + FormatValue(bm.total, bm.unit) + "."
					m = dm
					scope = "delta"
				}
			}
		}
	}
	if scope == "cumulative" {
		note += " Показаны накопленные значения за время работы процесса: " + reason + "."
	} else {
		note += " Разница снимков. Проверены совместимость сборки и неубывание счётчиков, но pprof не сообщает идентификатор запуска процесса."
	}
	if m.total == 0 && (kind == "block" || kind == "mutex") {
		note += " События не записаны; сбор мог быть выключен либо событий не было."
	}
	// before is a snapshot total; don't draw a delta as baseline → interval
	a.Metrics = append(a.Metrics, render(kind, sample, scope, m, nil, note))
}

func compatible(before, after *profile.Profile) string {
	if before.TimeNanos != 0 && after.TimeNanos != 0 && after.TimeNanos < before.TimeNanos {
		return "время конечного профиля предшествует начальному"
	}
	bids := make(map[string]string)
	allBefore, allAfter := make(map[string]bool), make(map[string]bool)
	for _, m := range before.Mapping {
		if m.BuildID != "" {
			bids[m.File] = m.BuildID
			allBefore[m.BuildID] = true
		}
	}
	for _, m := range after.Mapping {
		if m.BuildID != "" {
			allAfter[m.BuildID] = true
			if bid, ok := bids[m.File]; ok && bid != m.BuildID {
				return "изменился build ID бинарного файла"
			}
		}
	}
	if len(allBefore) > 0 && len(allAfter) > 0 {
		shared := false
		for bid := range allBefore {
			shared = shared || allAfter[bid]
		}
		if !shared {
			return "build ID бинарных файлов не совпадают"
		}
	}
	return ""
}

func extract(p *profile.Profile, sample, unit string) (measure, error) {
	idx := -1
	for i, t := range p.SampleType {
		if t.Type == sample {
			if t.Unit != unit {
				return measure{}, fmt.Errorf("%s: единица измерения %q, ожидалась %q", sample, t.Unit, unit)
			}
			idx = i
			break
		}
	}
	if idx < 0 {
		return measure{}, fmt.Errorf("тип сэмпла %s недоступен", sample)
	}
	m := measure{unit: unit, stacks: make(map[string]*weightedStack)}
	for _, s := range p.Sample {
		if idx >= len(s.Value) {
			return measure{}, fmt.Errorf("%s: в сэмпле отсутствуют значения", sample)
		}
		v := s.Value[idx]
		if v < 0 {
			return measure{}, fmt.Errorf("%s: отрицательные сэмплы; ожидался накопительный снимок", sample)
		}
		if v > math.MaxInt64-m.total {
			return measure{}, fmt.Errorf("%s: сумма сэмплов превышает диапазон int64", sample)
		}
		m.total += v
		frames := sampleFrames(s)
		key := stackKey(frames)
		if ws := m.stacks[key]; ws != nil {
			ws.value += v
		} else {
			m.stacks[key] = &weightedStack{frames: frames, value: v}
		}
	}
	return m, nil
}

func sampleFrames(s *profile.Sample) []model.Frame {
	var frames []model.Frame
	for _, loc := range s.Location {
		if len(loc.Line) == 0 {
			address := loc.Address // rebase so ASLR doesn't split the same stack
			file, build := "", ""
			if loc.Mapping != nil {
				file, build = loc.Mapping.File, loc.Mapping.BuildID
				if address >= loc.Mapping.Start {
					address = address - loc.Mapping.Start + loc.Mapping.Offset
				}
			}
			frames = append(frames, model.Frame{Name: fmt.Sprintf("[unknown %s@0x%x]", build, address), File: file})
			continue
		}
		for _, line := range loc.Line {
			if line.Function == nil {
				continue
			}
			name := line.Function.Name
			if name == "" {
				name = line.Function.SystemName
			}
			if name == "" {
				name = "[unknown]"
			}
			frames = append(frames, model.Frame{Name: name, File: line.Function.Filename, Line: line.Line})
		}
	}
	if len(frames) == 0 {
		frames = []model.Frame{{Name: "[unknown]"}}
	}
	return frames
}

func stackKey(frames []model.Frame) string {
	var b strings.Builder
	for _, f := range frames {
		fmt.Fprintf(&b, "%d:%s%d:%s:%d;", len(f.Name), f.Name, len(f.File), f.File, f.Line) // length prefix; names can contain ':'
	}
	return b.String()
}

func subtract(before, after measure) (measure, string) {
	if before.unit != after.unit {
		return measure{}, "изменились единицы измерения"
	}
	if after.total < before.total {
		return measure{}, "суммарный счётчик уменьшился (возможен перезапуск или несовместимые снимки)"
	}
	for key, bs := range before.stacks {
		as := after.stacks[key]
		if bs.value > 0 && (as == nil || as.value < bs.value) {
			return measure{}, "счётчик стека уменьшился (возможен перезапуск, изменение стеков или несовместимые снимки)"
		}
	}
	m := measure{unit: after.unit, total: after.total - before.total, stacks: make(map[string]*weightedStack)}
	for key, as := range after.stacks {
		v := as.value
		if bs := before.stacks[key]; bs != nil {
			v -= bs.value
		}
		if v > 0 {
			m.stacks[key] = &weightedStack{frames: as.frames, value: v}
		}
	}
	return m, ""
}

func render(kind, sample, scope string, m measure, before *int64, note string) model.Metric {
	metric := model.Metric{ID: kind + "." + sample, Kind: kind, SampleType: sample,
		Unit: m.unit, Scope: scope, Total: m.total, Before: before, Note: note}
	functions := make(map[string]*model.Function)
	var stacks []model.Stack
	for _, ws := range m.stacks {
		if ws.value == 0 {
			continue
		}
		seen := make(map[string]bool)
		for i, frame := range ws.frames {
			key := strconv.Itoa(len(frame.Name)) + ":" + frame.Name + frame.File
			f := functions[key]
			if f == nil {
				f = &model.Function{Frame: frame}
				functions[key] = f
			} else if frame.Line < f.Line {
				f.Line = frame.Line
			}
			if i == 0 {
				f.Flat += ws.value
			}
			if !seen[key] {
				f.Cum += ws.value
				seen[key] = true
			}
		}
		frames := make([]model.Frame, len(ws.frames))
		for i, f := range ws.frames {
			frames[len(frames)-1-i] = f
		}
		stacks = append(stacks, model.Stack{Frames: frames, Value: ws.value})
	}
	var byFlat, byCum []model.Function
	for _, f := range functions {
		byFlat = append(byFlat, *f)
		byCum = append(byCum, *f)
	}
	tieLess := func(x, y model.Function) bool {
		if x.Name != y.Name {
			return x.Name < y.Name
		}
		return x.File < y.File
	}
	sort.Slice(byFlat, func(i, j int) bool {
		x, y := byFlat[i], byFlat[j]
		if x.Flat != y.Flat {
			return x.Flat > y.Flat
		}
		if x.Cum != y.Cum {
			return x.Cum > y.Cum
		}
		return tieLess(x, y)
	})
	sort.Slice(byCum, func(i, j int) bool {
		x, y := byCum[i], byCum[j]
		if x.Cum != y.Cum {
			return x.Cum > y.Cum
		}
		if x.Flat != y.Flat {
			return x.Flat > y.Flat
		}
		return tieLess(x, y)
	})
	// flat leaves first; sorting only by cum hides them behind runtime frames
	selected := make(map[string]bool)
	add := func(f model.Function) {
		key := strconv.Itoa(len(f.Name)) + ":" + f.Name + f.File
		if !selected[key] {
			metric.Top = append(metric.Top, f)
			selected[key] = true
		}
	}
	for _, f := range byFlat {
		if f.Flat <= 0 || len(metric.Top) == 4 {
			break
		}
		add(f)
	}
	for _, f := range byCum {
		if len(metric.Top) == 6 {
			break
		}
		add(f)
	}
	sort.Slice(stacks, func(i, j int) bool {
		if stacks[i].Value != stacks[j].Value {
			return stacks[i].Value > stacks[j].Value
		}
		return stackKey(stacks[i].Frames) < stackKey(stacks[j].Frames)
	})
	if len(stacks) > 3 {
		stacks = stacks[:3]
	}
	metric.Stacks = stacks
	return metric
}

func hasSymbols(m model.Metric) bool {
	for _, s := range m.Stacks {
		for _, f := range s.Frames {
			if !strings.HasPrefix(f.Name, "[unknown") {
				return true
			}
		}
	}
	return false
}

func findings(metrics []model.Metric) []model.Finding {
	var out []model.Finding
	for _, m := range metrics {
		if m.Total <= 0 || len(m.Stacks) == 0 {
			continue
		}
		stack := m.Stacks[0]
		var names []string
		for _, frame := range stack.Frames {
			names = append(names, frame.Name)
		}
		scope := map[string]string{"snapshot": "конечный снимок", "delta": "разница снимков", "interval": "интервал CPU", "cumulative": "накоплено с начала процесса"}[m.Scope]
		if scope == "" {
			scope = m.Scope
		}
		f := model.Finding{ID: "finding." + m.ID, Evidence: []string{
			fmt.Sprintf("%s: %s из %s (%s); %s.", m.ID, FormatValue(stack.Value, m.Unit), FormatValue(m.Total, m.Unit), percent(stack.Value, m.Total), scope),
			"Стек от вызывающей функции к месту сэмпла: " + strings.Join(names, " → "),
		}}
		switch m.ID {
		case "cpu.cpu":
			f.Title = "Начните с наиболее заметного CPU-стека"
			f.Hypothesis = "Работа в этой цепочке может заметно влиять на расход CPU. Высокая доля сэмплов сама по себе не означает ошибку."
			f.NextStep = "Изучите цепочку вызовов и исходники той же сборки. После изменения повторите ту же нагрузку; время ответа запросов сравните отдельно."
		case "heap.inuse_space":
			f.Title = "Проверьте стек с наибольшим объёмом удерживаемых выделений"
			f.Hypothesis = "Объекты из этой цепочки могут храниться в кэше или иметь долгий срок жизни. Место выделения не показывает, какие ссылки удерживают объекты."
			f.NextStep = "Проверьте ожидаемый срок жизни объектов и кэши. Снимите несколько профилей после сопоставимой нагрузки и циклов GC."
		case "allocs.alloc_space":
			f.Title = "Проверьте стек с наибольшим объёмом выделений"
			f.Hypothesis = "В этой цепочке могут создаваться многочисленные временные объекты. Счётчик включает уже освобождённую память и не доказывает утечку."
			f.NextStep = "Изучите временные объекты в цепочке. Измерьте allocs/op на репрезентативном бенчмарке перед оптимизацией."
		case "goroutine.goroutine":
			f.Title = "Изучите крупнейшую группу горутин с общим стеком"
			f.Hypothesis = "Это может быть ожидаемая группа рабочих или ожидающих горутин. Совпадение стеков и рост количества не доказывают утечку."
			f.NextStep = "Проверьте состояния горутин, завершение работы и отмену контекста. Сравните несколько снимков после остановки нагрузки."
		case "block.delay":
			f.Title = "Записано ожидание синхронизации"
			f.Hypothesis = "Производитель данных или освобождение ресурса могут задерживать эту цепочку. Суммарное ожидание нескольких горутин не равно задержке одного запроса."
			f.NextStep = "Изучите канал или операцию синхронизации и соответствующий код отправки данных или освобождения ресурса при той же нагрузке."
		case "mutex.delay":
			f.Title = "Записана конкуренция за mutex"
			f.Hypothesis = "Долгая критическая секция или много конкурирующих горутин могут увеличивать ожидание. Стек указывает на завершение секции владельца, часто Unlock."
			f.NextStep = "Изучите работу под этой блокировкой и число конкурирующих горутин. Проверьте изменение повторным замером при сопоставимой нагрузке."
		default:
			continue
		}
		out = append(out, f)
		if len(out) == 6 {
			break
		}
	}
	return out
}

func percent(v, total int64) string {
	if total <= 0 {
		return "0%"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(v)/float64(total))
}

func FormatValue(v int64, unit string) string {
	switch unit {
	case "nanoseconds":
		return time.Duration(v).String()
	case "bytes":
		x := float64(v)
		units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
		i := 0
		for (x >= 1024 || x <= -1024) && i < len(units)-1 {
			x /= 1024
			i++
		}
		if i == 0 {
			return fmt.Sprintf("%d B", v)
		}
		return fmt.Sprintf("%.2f %s", x, units[i])
	case "count", "":
		return strconv.FormatInt(v, 10)
	default:
		return fmt.Sprintf("%d %s", v, unit)
	}
}
