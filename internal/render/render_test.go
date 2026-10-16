package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stokotlet/profmate/internal/model"
)

func TestConsoleShowsEmptyProfileCaveat(t *testing.T) {
	note := "События не записаны; сбор мог быть выключен либо событий не было."
	a := model.Analysis{Metrics: []model.Metric{{ID: "block.delay", Kind: "block", Unit: "nanoseconds", Scope: "delta", Note: note}, {ID: "block.contentions", Kind: "block", Unit: "count", Scope: "delta", Note: note}}}
	var out bytes.Buffer
	Text(&out, a)
	if strings.Count(out.String(), note) != 1 {
		t.Fatal(out.String())
	}
}
