package analyze

import (
	"bytes"
	"compress/gzip"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/pprof/profile"
	"github.com/stokotlet/profmate/internal/model"
)

func TestFunctionAggregationHandlesRecursionAndInlining(t *testing.T) {
	p := fixture([]*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}, []string{"app.worker", "app.caller"}, []int64{10})
	p.Sample[0].Location = []*profile.Location{p.Location[0], p.Location[1]}
	p.Location[0].Line = []profile.Line{{Function: p.Function[0], Line: 11}, {Function: p.Function[1], Line: 22}}
	p.Location[1].Line = []profile.Line{{Function: p.Function[0], Line: 33}}
	m, err := extract(p, "cpu", "nanoseconds")
	if err != nil {
		t.Fatal(err)
	}
	got := render("cpu", "cpu", "interval", m, nil, "")
	if got.Total != 10 || len(got.Top) != 2 {
		t.Fatalf("unexpected metric: %+v", got)
	}
	for _, fn := range got.Top {
		if fn.Cum != 10 {
			t.Errorf("recursion double counted cumulative %s: %d", fn.Name, fn.Cum)
		}
		wantFlat := int64(0)
		if fn.Name == "app.worker" {
			wantFlat = 10
		}
		if fn.Flat != wantFlat {
			t.Errorf("%s flat=%d want %d", fn.Name, fn.Flat, wantFlat)
		}
	}
	frames := got.Stacks[0].Frames
	if len(frames) != 3 || frames[0].Line != 33 || frames[1].Line != 22 || frames[2].Line != 11 {
		t.Fatalf("frames must be root→leaf, including inline frames: %+v", frames)
	}
}

func TestHeapSeparatesLiveMemoryAndAllocationDeltas(t *testing.T) {
	dir := t.TempDir()
	types := heapTypes()
	before := fixture(types, []string{"app.allocate"}, []int64{100, 1000, 20, 200})
	after := fixture(types, []string{"app.allocate"}, []int64{160, 1800, 15, 150})
	before.TimeNanos, after.TimeNanos = 100, 200
	caps := []model.Capture{writeCapture(t, dir, "heap", "before", before), writeCapture(t, dir, "heap", "after", after)}
	a := Build(dir, caps)
	for _, tc := range []struct {
		id            string
		total, before int64
		scope         string
	}{{"heap.inuse_space", 150, 200, "snapshot"}, {"heap.inuse_objects", 15, 20, "snapshot"},
		{"allocs.alloc_space", 800, 1000, "delta"}, {"allocs.alloc_objects", 60, 100, "delta"}} {
		m := metric(t, a, tc.id)
		beforeOK := m.Before != nil && *m.Before == tc.before
		if tc.scope == "delta" {
			beforeOK = m.Before == nil
		}
		if m.Total != tc.total || !beforeOK || m.Scope != tc.scope {
			t.Errorf("%s: got %+v, want total %d before %d scope %s", tc.id, m, tc.total, tc.before, tc.scope)
		}
	}
	if !strings.Contains(metric(t, a, "heap.inuse_space").Note, "принудительный GC не запускался") {
		t.Error("heap freshness caveat is missing")
	}
}

func TestCounterFallbackDoesNotClampNegativeStacks(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after int64
		moveStack     bool
		changeBuild   bool
		missingBefore bool
		wantReason    string
	}{
		{name: "counter reset", before: 100, after: 10, wantReason: "суммарный счётчик уменьшился"},
		{name: "stack decreased despite rising total", before: 100, after: 150, moveStack: true, wantReason: "счётчик стека уменьшился"},
		{name: "binary changed", before: 100, after: 150, changeBuild: true, wantReason: "изменился build ID"},
		{name: "missing before", before: 0, after: 150, missingBefore: true, wantReason: "начальный снимок недоступен"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			before := fixture(heapTypes(), []string{"app.a"}, []int64{10, tc.before, 1, 1})
			name := "app.a"
			if tc.moveStack {
				name = "app.b"
			}
			after := fixture(heapTypes(), []string{name}, []int64{15, tc.after, 2, 2})
			if tc.changeBuild {
				after.Mapping[0].BuildID = "another-binary"
			}
			var caps []model.Capture
			if !tc.missingBefore {
				caps = append(caps, writeCapture(t, dir, "heap", "before", before))
			}
			caps = append(caps, writeCapture(t, dir, "heap", "after", after))
			m := metric(t, Build(dir, caps), "allocs.alloc_space")
			if m.Scope != "cumulative" || m.Total != tc.after || m.Before != nil || !strings.Contains(m.Note, tc.wantReason) {
				t.Fatalf("unsafe or unlabeled delta: %+v", m)
			}
		})
	}
}

func TestIncompatibleUnitsDoNotBecomeAValidDelta(t *testing.T) {
	dir := t.TempDir()
	before := fixture(heapTypes(), []string{"app.a"}, []int64{1, 10, 1, 1})
	before.SampleType[1].Unit = "kilobytes"
	after := fixture(heapTypes(), []string{"app.a"}, []int64{2, 100, 2, 2})
	a := Build(dir, []model.Capture{writeCapture(t, dir, "heap", "before", before), writeCapture(t, dir, "heap", "after", after)})
	m := metric(t, a, "allocs.alloc_space")
	if m.Scope != "cumulative" || !strings.Contains(m.Note, `единица измерения "kilobytes"`) {
		t.Fatalf("unit mismatch was ignored: %+v", m)
	}
	badCPU := fixture([]*profile.ValueType{{Type: "cpu", Unit: "milliseconds"}}, []string{"app.a"}, []int64{10})
	a = Build(dir, []model.Capture{writeCapture(t, dir, "cpu", "window", badCPU)})
	if len(a.Metrics) != 0 || len(a.Notes) != 1 || !strings.Contains(a.Notes[0], "единица измерения") {
		t.Fatalf("CPU unit was silently mislabeled: %+v", a)
	}
}

func TestBlockingCountersAndGoroutineSnapshots(t *testing.T) {
	dir := t.TempDir()
	var caps []model.Capture
	for _, kind := range []string{"block", "mutex"} {
		types := []*profile.ValueType{{Type: "contentions", Unit: "count"}, {Type: "delay", Unit: "nanoseconds"}}
		caps = append(caps, writeCapture(t, dir, kind, "before", fixture(types, []string{"app.wait"}, []int64{5, 1000})))
		caps = append(caps, writeCapture(t, dir, kind, "after", fixture(types, []string{"app.wait"}, []int64{8, 2500})))
	}
	gt := []*profile.ValueType{{Type: "goroutine", Unit: "count"}}
	caps = append(caps, writeCapture(t, dir, "goroutine", "before", fixture(gt, []string{"app.worker"}, []int64{5})))
	caps = append(caps, writeCapture(t, dir, "goroutine", "after", fixture(gt, []string{"app.worker"}, []int64{7})))
	a := Build(dir, caps)
	for _, kind := range []string{"block", "mutex"} {
		m := metric(t, a, kind+".delay")
		if m.Total != 1500 || m.Scope != "delta" || m.Before != nil {
			t.Fatalf("%s delta incorrect: %+v", kind, m)
		}
		if metric(t, a, kind+".contentions").Total != 3 {
			t.Fatal("wrong contention delta")
		}
	}
	if !strings.Contains(metric(t, a, "mutex.delay").Note, "Unlock") {
		t.Error("mutex attribution caveat missing")
	}
	g := metric(t, a, "goroutine.goroutine")
	if g.Total != 7 || g.Before == nil || *g.Before != 5 || g.Stacks[0].Value != 7 || g.Scope != "snapshot" {
		t.Fatalf("goroutine snapshot treated as delta: %+v", g)
	}
}

func TestEmptyProfilesAndPartialCaptureFailures(t *testing.T) {
	dir := t.TempDir()
	types := []*profile.ValueType{{Type: "contentions", Unit: "count"}, {Type: "delay", Unit: "nanoseconds"}}
	p := fixture(types, []string{"app.a"}, []int64{0, 0})
	p.Sample = nil
	caps := []model.Capture{writeCapture(t, dir, "block", "after", p), {Kind: "cpu", Phase: "window", Error: "target timed out"},
		{Kind: "heap", Phase: "after", File: "missing.pprof"}, {Kind: "goroutine_text", Phase: "after", File: "dump.txt"}}
	a := Build(dir, caps)
	if len(a.Metrics) != 2 || len(a.Findings) != 0 {
		t.Fatalf("empty counters became findings: %+v", a)
	}
	for _, m := range a.Metrics {
		if m.Total != 0 || len(m.Top) != 0 || !strings.Contains(m.Note, "сбор мог быть выключен") {
			t.Fatalf("empty profile mislabeled: %+v", m)
		}
	}
	joined := strings.Join(a.Notes, " ")
	if !strings.Contains(joined, "target timed out") || !strings.Contains(joined, "не удалось прочитать профиль") || strings.Contains(joined, "dump.txt") {
		t.Fatalf("partial errors were not handled: %+v", a.Notes)
	}
}

func TestNormalizedUnsymbolizedAddressesTolerateASLR(t *testing.T) {
	p := fixture([]*profile.ValueType{{Type: "alloc_space", Unit: "bytes"}}, []string{"app.a"}, []int64{100})
	p.Location[0].Line = nil
	p.Location[0].Address = 0x1020
	p.Mapping[0].Start = 0x1000
	b, _ := extract(p, "alloc_space", "bytes")
	p.Location[0].Address = 0x2020
	p.Mapping[0].Start = 0x2000
	p.Sample[0].Value[0] = 150
	a, _ := extract(p, "alloc_space", "bytes")
	delta, reason := subtract(b, a)
	if reason != "" || delta.total != 50 || len(delta.stacks) != 1 {
		t.Fatalf("ASLR changed stack identity: %+v %s", delta, reason)
	}
}

func TestTopIncludesHotLeavesUnderSharedAncestors(t *testing.T) {
	p := fixture([]*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		[]string{"app.hot", "app.other", "app.handler", "runtime.a", "runtime.b", "runtime.c", "runtime.d", "runtime.e", "runtime.f"}, []int64{80})
	p.Sample[0].Location = append([]*profile.Location{p.Location[0]}, p.Location[2:]...)
	p.Sample = append(p.Sample, &profile.Sample{Location: append([]*profile.Location{p.Location[1]}, p.Location[2:]...), Value: []int64{20}})
	m, err := extract(p, "cpu", "nanoseconds")
	if err != nil {
		t.Fatal(err)
	}
	got := render("cpu", "cpu", "interval", m, nil, "")
	if len(got.Top) != 6 || got.Top[0].Name != "app.hot" || got.Top[0].Flat != 80 || got.Top[1].Name != "app.other" {
		t.Fatalf("hot leaves hidden by shared ancestors: %+v", got.Top)
	}
	for i := 0; i < 20; i++ {
		if next := render("cpu", "cpu", "interval", m, nil, ""); !reflect.DeepEqual(got, next) {
			t.Fatalf("unstable map iteration changed metric: %+v\n%+v", got, next)
		}
	}
}

func TestFindingsContainMeasuredEvidenceForAllProfileKinds(t *testing.T) {
	var metrics []model.Metric
	for _, id := range []string{"cpu.cpu", "heap.inuse_space", "allocs.alloc_space", "goroutine.goroutine", "block.delay", "mutex.delay"} {
		metrics = append(metrics, model.Metric{ID: id, Total: 100, Scope: "delta", Unit: "count", Stacks: []model.Stack{
			{Value: 25, Frames: []model.Frame{{Name: "app.handler"}, {Name: "app.worker"}}},
		}})
	}
	got := findings(metrics)
	if len(got) != 6 || got[5].ID != "finding.mutex.delay" {
		t.Fatalf("mutex finding was lost: %+v", got)
	}
	for i, f := range got {
		evidence := strings.Join(f.Evidence, " ")
		for _, want := range []string{metrics[i].ID, "25 из 100 (25.0%)", "app.handler → app.worker"} {
			if !strings.Contains(evidence, want) {
				t.Errorf("finding %s missing evidence %q: %q", f.ID, want, evidence)
			}
		}
	}
}

func TestReadProfileConfinesPathsToSession(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	p := fixture([]*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}, []string{"app.a"}, []int64{10})
	inside := writeCapture(t, dir, "cpu", "window", p)
	other := writeCapture(t, outside, "cpu", "window", p)
	if err := os.Symlink(filepath.Join(outside, other.File), filepath.Join(dir, "outside.pprof")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked-directory")); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(dir, filepath.Join(outside, other.File))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{filepath.Join(dir, inside.File), filepath.Join(outside, other.File), rel, "../escape.pprof", "outside.pprof", "linked-directory/" + other.File, "."} {
		if _, err := readProfile(dir, name); err == nil {
			t.Errorf("accepted unconfined or non-file path %q", name)
		}
	}
	if _, err := readProfile(dir, inside.File); err != nil {
		t.Fatalf("valid session profile was rejected: %v", err)
	}
}

func TestReadProfileRejectsMalformedAndNestedGzip(t *testing.T) {
	dir := t.TempDir()
	p := fixture([]*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}, []string{"app.a"}, []int64{10})
	p.Sample[0].Value = nil
	bad := writeCapture(t, dir, "cpu", "bad", p)
	if _, err := readProfile(dir, bad.File); err == nil {
		t.Fatal("accepted profile with mismatched sample values")
	}
	p.Sample[0].Value = []int64{10}
	var inner, outer bytes.Buffer
	if err := p.Write(&inner); err != nil {
		t.Fatal(err)
	}
	z := gzip.NewWriter(&outer)
	if _, err := z.Write(inner.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested.pprof"), outer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProfile(dir, "nested.pprof"); err == nil {
		t.Fatal("accepted nested gzip instead of protobuf after bounded decode")
	}
}

func TestReadProfileBoundsDecompression(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "large.pprof"))
	if err != nil {
		t.Fatal(err)
	}
	z := gzip.NewWriter(f)
	if _, err := io.CopyN(z, zeroReader{}, maxProfileBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readProfile(dir, "large.pprof"); err == nil || !strings.Contains(err.Error(), "распакованный профиль превышает") {
		t.Fatalf("unbounded or unlabelled decompression error: %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

func TestExtractRejectsTotalOverflow(t *testing.T) {
	p := fixture([]*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}, []string{"app.a"}, []int64{math.MaxInt64})
	p.Sample = append(p.Sample, &profile.Sample{Location: p.Location, Value: []int64{1}})
	if _, err := extract(p, "cpu", "nanoseconds"); err == nil || !strings.Contains(err.Error(), "int64") {
		t.Fatalf("sample arithmetic overflow was accepted: %v", err)
	}
}

func TestFormatValue(t *testing.T) {
	for _, tc := range []struct {
		v          int64
		unit, want string
	}{
		{1024, "bytes", "1.00 KiB"}, {1572864, "bytes", "1.50 MiB"}, {-2048, "bytes", "-2.00 KiB"},
		{1500000, "nanoseconds", "1.5ms"}, {7, "count", "7"}, {9, "widgets", "9 widgets"},
	} {
		if got := FormatValue(tc.v, tc.unit); got != tc.want {
			t.Errorf("FormatValue(%d,%s)=%s want %s", tc.v, tc.unit, got, tc.want)
		}
	}
}

func heapTypes() []*profile.ValueType {
	return []*profile.ValueType{{Type: "alloc_objects", Unit: "count"}, {Type: "alloc_space", Unit: "bytes"},
		{Type: "inuse_objects", Unit: "count"}, {Type: "inuse_space", Unit: "bytes"}}
}

func fixture(types []*profile.ValueType, names []string, values []int64) *profile.Profile {
	p := &profile.Profile{SampleType: types, TimeNanos: 1000, DurationNanos: 1000000000,
		Mapping: []*profile.Mapping{{ID: 1, File: "/app/server", BuildID: "build-a", HasFunctions: true, HasFilenames: true, HasLineNumbers: true}}}
	for i, name := range names {
		fn := &profile.Function{ID: uint64(i + 1), Name: name, Filename: "app.go"}
		loc := &profile.Location{ID: uint64(i + 1), Mapping: p.Mapping[0], Line: []profile.Line{{Function: fn, Line: int64(i + 10)}}}
		p.Function = append(p.Function, fn)
		p.Location = append(p.Location, loc)
	}
	p.Sample = []*profile.Sample{{Location: p.Location, Value: values}}
	return p
}

func writeCapture(t *testing.T, dir, kind, phase string, p *profile.Profile) model.Capture {
	t.Helper()
	name := kind + "-" + phase + ".pprof"
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Write(f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return model.Capture{Kind: kind, Phase: phase, File: name}
}

func metric(t *testing.T, a model.Analysis, id string) model.Metric {
	t.Helper()
	for _, m := range a.Metrics {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("metric %s missing from %+v", id, a)
	return model.Metric{}
}
