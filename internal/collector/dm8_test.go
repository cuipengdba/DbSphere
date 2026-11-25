package collector

import (
	"strings"
	"testing"
)

func TestDM8RecognizesMissingAndObservedZeroWaits(t *testing.T) {
	const basic = "SQL> DBS8|instance|DM Database Server x64 V8|20260707|OPEN|NORMAL|2026-11-25 12:00:00\n" +
		"SQL> DBS8|sessions|2|1\nDBS8|transactions|1\nDBS8|file|1:0|MAIN|32768|8192\n"
	for _, test := range []struct {
		line, status string
		waits        int
	}{
		{"", "unavailable", 0},
		{"DBS8|wait|none||||\n", "ok", 0},
		{"DBS8|wait|111|123|136277979266392|136277580111056|4001\n", "ok", 1},
	} {
		sample, err := parseDM8(basic + test.line)
		if err != nil || len(sample.Waits) != test.waits || sample.Signals[3].Status != test.status {
			t.Fatalf("wait line %q: waits=%+v signals=%+v err=%v", test.line, sample.Waits, sample.Signals, err)
		}
		if test.waits > 0 && sample.Waits[0].BlockingSession != "136277580111056" {
			t.Fatal("DM8 session identifier lost precision")
		}
	}
}

func TestDM8ConnectFieldsCannotAddClientInstructions(t *testing.T) {
	input := dm8Request{Host: "127.0.0.1", Port: 5236, Username: "DBSPHERE_MON", Password: "fixture%&/@!*"}
	if !input.valid() {
		t.Fatal("ordinary monitoring credential rejected")
	}
	for _, suffix := range []string{"\nEXIT", "\r", "\";", "\x00"} {
		changed := input
		changed.Password += suffix
		if changed.valid() {
			t.Fatalf("invalid credential accepted for suffix %q", suffix)
		}
	}
	var output dm8Output
	if _, err := output.Write([]byte(strings.Repeat("x", 128<<10+1))); err == nil {
		t.Fatal("probe output limit was not applied")
	}
}
