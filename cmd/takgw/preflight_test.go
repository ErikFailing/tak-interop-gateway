package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkAt is the -now every test passes, so the expiry in goodLine is two minutes in the future.
const checkAt = "2026-11-27T18:00:00Z"

// goodLine is one complete track entity that breaks no rule at checkAt.
const goodLine = `{"entityId":"e1","isLive":true,"expiryTime":"2026-11-27T18:02:00Z","aliases":{"name":"SIM-SAR-1"},` +
	`"milView":{"disposition":"DISPOSITION_FRIENDLY","environment":"ENVIRONMENT_LAND"},` +
	`"ontology":{"template":"TEMPLATE_TRACK","platformType":"a-f-G-U-C"},` +
	`"location":{"position":{"latitudeDegrees":40.3,"longitudeDegrees":-105.6}},` +
	`"symbology":{"milStd2525C":{"sidc":"SFGPUC---------"}},` +
	`"provenance":{"integrationName":"tak-interop-gateway","dataType":"cot","sourceUpdateTime":"2026-11-27T18:00:00Z"},` +
	`"tracked":{"trackQualityWrapper":2},"dataClassification":{"default":{"level":"CLASSIFICATION_LEVELS_UNCLASSIFIED"}}}`

// variant returns goodLine with one substring replaced, failing the test if it isn't there.
func variant(t *testing.T, old, replacement string) string {
	t.Helper()
	if !strings.Contains(goodLine, old) {
		t.Fatalf("goodLine has no %q", old)
	}
	return strings.Replace(goodLine, old, replacement, 1)
}

// TestPreflightCommand checks the command's output and exit status.
func TestPreflightCommand(t *testing.T) {
	isLiveFalse := variant(t, `"isLive":true`, `"isLive":false`)
	// otherFalse is a different entity, so following goodLine it isn't also an unchanged update of e1.
	otherFalse := strings.Replace(isLiveFalse, `"entityId":"e1"`, `"entityId":"e2"`, 1)
	noSymbology := variant(t, `"symbology":{"milStd2525C":{"sidc":"SFGPUC---------"}},`, "")
	geo := variant(t, `"TEMPLATE_TRACK"`, `"TEMPLATE_GEO"`)
	tests := []struct {
		name       string
		args       []string
		stdin      string
		wantCode   int
		wantStdout []string // substrings stdout must contain
		notStdout  []string // substrings stdout must not contain
		wantStderr string
	}{
		{name: "passing entity", stdin: goodLine + "\n", wantCode: 0, wantStderr: "1 entities checked: 0 errors, 0 warnings, 0 unreadable"},
		{name: "error", stdin: isLiveFalse, wantCode: 1, wantStdout: []string{"stdin:1: error is-live isLive: is false"}},
		{name: "warning passes", stdin: noSymbology, wantCode: 0, wantStdout: []string{"stdin:1: warning symbology-set"}},
		{name: "warning fails with -strict", args: []string{"-strict"}, stdin: noSymbology, wantCode: 1},
		{name: "blank lines keep line numbers", stdin: "\n  \n" + isLiveFalse + "\n", wantCode: 1, wantStdout: []string{"stdin:3: error is-live"}},
		{name: "unreadable line", stdin: goodLine + "\n{bad\n" + otherFalse + "\n", wantCode: 2,
			wantStdout: []string{"stdin:2: preflight: invalid entity JSON", "stdin:3: error is-live"},
			wantStderr: "2 entities checked: 1 errors, 0 warnings, 1 unreadable"},
		{name: "duplicate key", stdin: `{"entityId":"a","entityId":"b"}`, wantCode: 2, wantStdout: []string{"duplicate JSON key: entityId"}},
		{name: "unchanged update time", stdin: goodLine + "\n" + goodLine + "\n", wantCode: 1, wantStdout: []string{"stdin:2: error source-update-time-changes"}},
		{name: "info hidden by default", stdin: geo, wantCode: 0, notStdout: []string{"unchecked-template-rules"}},
		{name: "info shown with -v", args: []string{"-v"}, stdin: geo, wantCode: 0, wantStdout: []string{"stdin:1: info unchecked-template-rules"}},
		{name: "ok line with -v", args: []string{"-v"}, stdin: goodLine, wantCode: 0, wantStdout: []string{"stdin:1: ok"}},
		{name: "rules", args: []string{"-rules"}, wantCode: 0, wantStdout: []string{"# Entity Preflight rules", "`expiry-in-future`"}},
		{name: "bad -now", args: []string{"-now", "tomorrow"}, wantCode: 2, wantStderr: "-now"},
		{name: "unknown flag", args: []string{"-bogus"}, wantCode: 2, wantStderr: "flag provided but not defined"},
		{name: "help", args: []string{"-h"}, wantCode: 0, wantStderr: "usage: takgw preflight"},
		{name: "missing file", args: []string{"no-such-file.jsonl"}, wantCode: 2, wantStderr: "no-such-file.jsonl"},
		{name: "line too long", stdin: strings.Repeat(" ", maxLine+1), wantCode: 2, wantStderr: "token too long"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{"-now", checkAt}, tt.args...)
			if got := runPreflight(args, strings.NewReader(tt.stdin), &stdout, &stderr); got != tt.wantCode {
				t.Errorf("exit code = %d, want %d\nstdout: %s\nstderr: %s", got, tt.wantCode, &stdout, &stderr)
			}
			for _, s := range tt.wantStdout {
				if !strings.Contains(stdout.String(), s) {
					t.Errorf("stdout = %q, want it to contain %q", stdout.String(), s)
				}
			}
			for _, s := range tt.notStdout {
				if strings.Contains(stdout.String(), s) {
					t.Errorf("stdout = %q, want it not to contain %q", stdout.String(), s)
				}
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

// TestPreflightFiles checks file arguments, "-" for stdin, and that one checker spans every input in order.
func TestPreflightFiles(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.jsonl")
	second := filepath.Join(dir, "second.jsonl")
	for _, f := range []string{first, second} {
		if err := os.WriteFile(f, []byte(goodLine+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	code := runPreflight([]string{"-now", checkAt, first, "-", second}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1\nstdout: %s\nstderr: %s", code, &stdout, &stderr)
	}
	// The same entity with the same time in the second file is an update the server would discard.
	if want := second + ":1: error source-update-time-changes"; !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
	}
	if strings.Contains(stdout.String(), first+":") {
		t.Errorf("the first file should pass: %q", stdout.String())
	}
}
