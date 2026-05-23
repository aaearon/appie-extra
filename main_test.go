package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// captureStdout swaps os.Stdout for a pipe and returns whatever was written.
// Closes both pipe ends and restores stdout even if fn panics, so the copy
// goroutine can finish and the test framework gets a clean failure.
func captureStdout(t *testing.T, fn func()) []byte {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	done := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.Bytes()
	}()

	defer func() {
		os.Stdout = old
		_ = r.Close()
	}()

	// Run fn in an inner func so w.Close() runs even if fn panics — that
	// unblocks the copy goroutine and lets the panic propagate cleanly.
	func() {
		defer func() { _ = w.Close() }()
		fn()
	}()

	return <-done
}

// withFakeExit swaps exitFn for the duration of fn; returns the captured code
// (or -1 if exitFn was never called).
func withFakeExit(t *testing.T, fn func()) int {
	t.Helper()
	code := -1
	old := exitFn
	exitFn = func(c int) { code = c }
	defer func() { exitFn = old }()
	fn()
	return code
}

func TestEmitSuccessShape(t *testing.T) {
	out := captureStdout(t, func() {
		emitSuccess(map[string]any{"foo": "bar"}, nil, nil)
	})

	var env map[string]any
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, out)
	}

	if env["ok"] != true {
		t.Errorf("ok = %v, want true", env["ok"])
	}
	if _, ok := env["meta"]; ok {
		t.Errorf("meta should be omitted when nil; got %v", env["meta"])
	}
	if _, ok := env["warnings"]; ok {
		t.Errorf("warnings should be omitted when empty; got %v", env["warnings"])
	}
	if _, ok := env["error"]; ok {
		t.Errorf("error should be omitted on success; got %v", env["error"])
	}
	data, ok := env["data"].(map[string]any)
	if !ok || data["foo"] != "bar" {
		t.Errorf("data = %v, want {foo:bar}", env["data"])
	}
}

func TestEmitSuccessIncludesMetaAndWarnings(t *testing.T) {
	out := captureStdout(t, func() {
		emitSuccess(
			[]int{1, 2, 3},
			map[string]any{"total": 3},
			[]string{"slow upstream"},
		)
	})

	var env map[string]any
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env["meta"].(map[string]any)["total"].(float64) != 3 {
		t.Errorf("meta.total wrong: %v", env["meta"])
	}
	warns := env["warnings"].([]any)
	if len(warns) != 1 || warns[0] != "slow upstream" {
		t.Errorf("warnings wrong: %v", warns)
	}
}

func TestEmitErrorShape(t *testing.T) {
	var out []byte
	gotCode := withFakeExit(t, func() {
		out = captureStdout(t, func() {
			emitError("bad_args", "boom", exitUserError)
		})
	})

	if gotCode != exitUserError {
		t.Errorf("exit code = %d, want %d", gotCode, exitUserError)
	}

	var env map[string]any
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, out)
	}
	if env["ok"] != false {
		t.Errorf("ok = %v, want false", env["ok"])
	}
	errMap, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("error missing or wrong type: %v", env["error"])
	}
	if errMap["code"] != "bad_args" {
		t.Errorf("error.code = %v, want bad_args", errMap["code"])
	}
	if errMap["message"] != "boom" {
		t.Errorf("error.message = %v, want boom", errMap["message"])
	}
	if _, hasData := env["data"]; hasData {
		t.Errorf("data should be omitted on error; got %v", env["data"])
	}
}

func TestParsePositiveInt(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   int
		wantOK bool
	}{
		{"valid", "5", 5, true},
		{"zero", "0", 0, false},
		{"negative", "-3", 0, false},
		{"alpha", "abc", 0, false},
		{"empty", "", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got int
			var stdoutBytes []byte
			gotCode := withFakeExit(t, func() {
				stdoutBytes = captureStdout(t, func() {
					got = parsePositiveInt(c.in, "v")
				})
			})

			if c.wantOK {
				if gotCode != -1 {
					t.Errorf("unexpected exit (code %d) for valid input %q", gotCode, c.in)
				}
				if got != c.want {
					t.Errorf("got %d, want %d", got, c.want)
				}
				if len(stdoutBytes) != 0 {
					t.Errorf("unexpected stdout for valid input: %s", stdoutBytes)
				}
			} else {
				if gotCode != exitUserError {
					t.Errorf("exit = %d, want %d", gotCode, exitUserError)
				}
				if !bytes.Contains(stdoutBytes, []byte(`"invalid_int"`)) {
					t.Errorf("expected invalid_int error envelope; got: %s", stdoutBytes)
				}
			}
		})
	}
}

func TestStripImagesFromRaw(t *testing.T) {
	input := []byte(`{
		"products": [
			{"id": 1, "title": "a", "images": [{"url": "x"}]},
			{"id": 2, "title": "b", "images": []}
		],
		"nested": {
			"item": {"images": "bad", "name": "ok"}
		},
		"empty_list": []
	}`)

	out := stripImagesFromRaw(input)
	s := string(out)

	if strings.Contains(s, `"images"`) {
		t.Errorf("images key still present: %s", s)
	}
	for _, want := range []string{`"title"`, `"name"`, `"id"`, `"nested"`, `"empty_list"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing key %s in: %s", want, s)
		}
	}

	// Round-trip must still be valid JSON.
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, s)
	}
}

func TestParseGlobalFlagsStrips(t *testing.T) {
	gf, rest := parseGlobalFlags([]string{"--no-images", "previously-bought", "5"})
	if !gf.noImages {
		t.Errorf("expected noImages=true")
	}
	if len(rest) != 2 || rest[0] != "previously-bought" || rest[1] != "5" {
		t.Errorf("rest = %v", rest)
	}
}

// Regression: a global flag preceding the command must be absorbed, not
// treated as the command itself (which would fall through to unknown_command).
func TestParseGlobalFlagsFlagBeforeCommand(t *testing.T) {
	for _, args := range [][]string{
		{"--no-images", "bonus-products", "5"},
		{"--verbose", "--no-images", "member"},
		{"bonus-products", "--no-images", "5"},
		{"bonus-products", "5", "--no-images"},
	} {
		gf, rest := parseGlobalFlags(args)
		if !gf.noImages {
			t.Errorf("%v: expected noImages=true", args)
		}
		if len(rest) == 0 || rest[0] == "--no-images" || rest[0] == "--verbose" {
			t.Errorf("%v: global flag leaked into rest: %v", args, rest)
		}
	}
}

func TestPickPeriodContaining(t *testing.T) {
	// Mirrors a real holiday-shifted AH response: the "current" period
	// is extended through Whit Monday (May 25), and the next period
	// starts on a Tuesday.
	periods := []bonusPeriod{
		{Start: "2026-05-18", End: "2026-05-25"},
		{Start: "2026-05-26", End: "2026-05-31"},
	}
	cases := []struct {
		name, in, wantS, wantE string
		wantOK                 bool
	}{
		{"start of current", "2026-05-18", "2026-05-18", "2026-05-25", true},
		{"holiday boundary day", "2026-05-25", "2026-05-18", "2026-05-25", true},
		{"start of next", "2026-05-26", "2026-05-26", "2026-05-31", true},
		{"end of next", "2026-05-31", "2026-05-26", "2026-05-31", true},
		{"before all", "2026-05-01", "", "", false},
		{"after all", "2026-06-15", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, ok := pickPeriodContaining(periods, c.in)
			if ok != c.wantOK || p.Start != c.wantS || p.End != c.wantE {
				t.Errorf("got (%q, %q, %v); want (%q, %q, %v)", p.Start, p.End, ok, c.wantS, c.wantE, c.wantOK)
			}
		})
	}
}

func TestPickNextPeriod(t *testing.T) {
	periods := []bonusPeriod{
		{Start: "2026-05-18", End: "2026-05-25"},
		{Start: "2026-05-26", End: "2026-05-31"},
	}
	if p, ok := pickNextPeriod(periods, "2026-05-23"); !ok || p.Start != "2026-05-26" || p.End != "2026-05-31" {
		t.Errorf("next from 2026-05-23: got (%q, %q, %v); want (2026-05-26, 2026-05-31, true)", p.Start, p.End, ok)
	}
	if _, ok := pickNextPeriod(periods, "2026-05-26"); ok {
		t.Errorf("next from start of last published period should report no next; metadata likely needs to advance first")
	}
	if _, ok := pickNextPeriod(nil, "2026-05-23"); ok {
		t.Errorf("next on empty periods should not find a match")
	}
}

func TestSelectPeriods(t *testing.T) {
	periods := []bonusPeriod{
		{Start: "2026-05-18", End: "2026-05-25", NextPeriodVisibleFrom: "2026-05-22T00:00:00Z"},
		{Start: "2026-05-26", End: "2026-05-31", NextPeriodVisibleFrom: "2026-05-29T00:00:00Z"},
	}
	const today = "2026-05-23"

	t.Run("all returns full list as array", func(t *testing.T) {
		items, single, code, _ := selectPeriods(periods, "all", today)
		if code != "" {
			t.Fatalf("unexpected error: %s", code)
		}
		if single {
			t.Errorf("all should emit as array, not single")
		}
		if len(items) != 2 {
			t.Errorf("got %d items, want 2", len(items))
		}
	})

	t.Run("current picks period containing today", func(t *testing.T) {
		items, single, code, _ := selectPeriods(periods, "current", today)
		if code != "" {
			t.Fatalf("unexpected error: %s", code)
		}
		if !single || len(items) != 1 || items[0].Start != "2026-05-18" {
			t.Errorf("got items=%v single=%v; want current period as single", items, single)
		}
		if items[0].NextPeriodVisibleFrom == "" {
			t.Errorf("expected NextPeriodVisibleFrom to flow through; got empty")
		}
	})

	t.Run("next picks earliest period after today", func(t *testing.T) {
		items, single, code, _ := selectPeriods(periods, "next", today)
		if code != "" {
			t.Fatalf("unexpected error: %s", code)
		}
		if !single || len(items) != 1 || items[0].Start != "2026-05-26" {
			t.Errorf("got items=%v single=%v; want next period as single", items, single)
		}
	})

	t.Run("explicit date picks containing period", func(t *testing.T) {
		items, single, code, _ := selectPeriods(periods, "2026-05-28", today)
		if code != "" {
			t.Fatalf("unexpected error: %s", code)
		}
		if !single || len(items) != 1 || items[0].Start != "2026-05-26" {
			t.Errorf("got items=%v single=%v; want 2026-05-26..31 as single", items, single)
		}
	})

	t.Run("unknown arg rejected", func(t *testing.T) {
		items, _, code, _ := selectPeriods(periods, "bogus", today)
		if code != "unexpected_arg" {
			t.Errorf("got code=%q, want unexpected_arg", code)
		}
		if items != nil {
			t.Errorf("got items=%v, want nil", items)
		}
	})

	t.Run("date with no matching period reports no_period", func(t *testing.T) {
		_, _, code, _ := selectPeriods(periods, "2026-06-15", today)
		if code != "no_period" {
			t.Errorf("got code=%q, want no_period", code)
		}
	})
}

// today() must compute the calendar date in Europe/Amsterdam, not the
// process-local TZ — AH bonus periods are keyed to Dutch local dates and
// the CLI runs from arbitrary timezones.
func TestTodayUsesAmsterdamTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	want := time.Now().In(loc).Format("2006-01-02")
	if got := today(); got != want {
		t.Errorf("today() = %q, want %q (Europe/Amsterdam)", got, want)
	}
}

func TestParseGlobalFlagsRejectsUnknown(t *testing.T) {
	var out []byte
	gotCode := withFakeExit(t, func() {
		out = captureStdout(t, func() {
			_, _ = parseGlobalFlags([]string{"--bogus"})
		})
	})
	if gotCode != exitUserError {
		t.Errorf("exit = %d, want %d", gotCode, exitUserError)
	}
	if !bytes.Contains(out, []byte(`"bad_args"`)) {
		t.Errorf("expected bad_args envelope; got: %s", out)
	}
}

func TestParseBonusDateArg(t *testing.T) {
	fixed := func() string { return "2026-05-22" }

	t.Run("empty uses default", func(t *testing.T) {
		date, rest := parseBonusDateArg(nil, fixed)
		if date != "2026-05-22" {
			t.Errorf("date = %q, want default", date)
		}
		if len(rest) != 0 {
			t.Errorf("rest = %v, want empty", rest)
		}
	})

	t.Run("next consumes arg", func(t *testing.T) {
		date, rest := parseBonusDateArg([]string{"next"}, fixed)
		got, err := time.Parse("2006-01-02", date)
		if err != nil {
			t.Fatalf("returned date %q not parseable: %v", date, err)
		}
		// "next" is just a hint that resolves to ~7 days out; the
		// caller pairs it with AH metadata to land on the real next
		// bonus period (holidays shift the week boundary).
		diff := int(time.Until(got).Hours() / 24)
		if diff < 6 || diff > 8 {
			t.Errorf("next should be ~7 days from now, got %d days (%s)", diff, date)
		}
		if len(rest) != 0 {
			t.Errorf("rest = %v, want empty", rest)
		}
	})

	t.Run("explicit date consumes arg", func(t *testing.T) {
		date, rest := parseBonusDateArg([]string{"2026-05-25", "25"}, fixed)
		if date != "2026-05-25" {
			t.Errorf("date = %q, want 2026-05-25", date)
		}
		if len(rest) != 1 || rest[0] != "25" {
			t.Errorf("rest = %v, want [25]", rest)
		}
	})

	t.Run("invalid date-shaped arg rejects", func(t *testing.T) {
		var out []byte
		gotCode := withFakeExit(t, func() {
			out = captureStdout(t, func() {
				_, _ = parseBonusDateArg([]string{"2026-13-99"}, fixed)
			})
		})
		if gotCode != exitUserError {
			t.Errorf("exit = %d, want %d", gotCode, exitUserError)
		}
		if !bytes.Contains(out, []byte(`"bad_args"`)) {
			t.Errorf("expected bad_args envelope; got: %s", out)
		}
	})

	t.Run("numeric limit is not consumed", func(t *testing.T) {
		date, rest := parseBonusDateArg([]string{"25"}, fixed)
		if date != "2026-05-22" {
			t.Errorf("date = %q, want default", date)
		}
		if len(rest) != 1 || rest[0] != "25" {
			t.Errorf("rest = %v, want [25]", rest)
		}
	})

	t.Run("mixed next + limit", func(t *testing.T) {
		date, rest := parseBonusDateArg([]string{"next", "25"}, fixed)
		if _, err := time.Parse("2006-01-02", date); err != nil {
			t.Fatalf("date = %q, want parseable date", date)
		}
		if len(rest) != 1 || rest[0] != "25" {
			t.Errorf("rest = %v, want [25]", rest)
		}
	})
}
