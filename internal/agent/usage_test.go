package agent

import (
	"bytes"
	"strings"
	"testing"
)

func TestStatusLine(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	in := `{"session_id":"s1","model":{"id":"claude-opus-5-5"},"cost":{"total_cost_usd":1.25},` +
		`"context_window":{"context_window_size":1000000,"used_percentage":14.5},` +
		`"rate_limits":{"five_hour":{"used_percentage":19,"resets_at":1791531600},` +
		`"seven_day":{"used_percentage":32,"resets_at":"2026-10-14T14:00:00Z"}}}`
	var out bytes.Buffer
	if err := StatusLine(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != in {
		t.Fatalf("the JSON must pass through unchanged, got %q", out.String())
	}
	l, ok := ReadLive("s1")
	if !ok || l.CtxPct != 14.5 || l.CtxSize != 1000000 || l.Cost != 1.25 || l.Model != "claude-opus-5-5" {
		t.Errorf("live = %+v, %v", l, ok)
	}
	lim, ok := ReadLimits()
	if !ok || lim.FiveHour == nil || lim.FiveHour.Used != 19 || lim.FiveHour.ResetsAt != 1791531600 ||
		lim.SevenDay == nil || lim.SevenDay.ResetsAt != 1791986400 {
		t.Errorf("limits = %+v %+v, %v", lim.FiveHour, lim.SevenDay, ok)
	}
}

func TestStatusLineNotJSON(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	var out bytes.Buffer
	if err := StatusLine(strings.NewReader("not json"), &out); err != nil || out.String() != "not json" {
		t.Errorf("got %q, %v", out.String(), err)
	}
	if _, ok := ReadLimits(); ok {
		t.Error("no limits should be written")
	}
}

func TestStamp(t *testing.T) {
	for raw, want := range map[string]int64{`1791531600`: 1791531600, `1791531600000`: 1791531600, `"2026-10-14T14:00:00Z"`: 1791986400, `"1791531600"`: 1791531600, `null`: 0} {
		if got := stamp([]byte(raw)); got != want {
			t.Errorf("stamp(%s) = %d, want %d", raw, got, want)
		}
	}
}
