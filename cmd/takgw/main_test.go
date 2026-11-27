package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun checks the dispatcher's exit codes and which stream each message goes to.
func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no command", args: nil, wantCode: 2, wantStderr: "usage: takgw"},
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "usage: takgw"},
		{name: "serve not implemented", args: []string{"serve"}, wantCode: 1, wantStderr: "not implemented yet"},
		{name: "preflight dispatches", args: []string{"preflight"}, wantCode: 0, wantStderr: "0 entities checked"},
		{name: "readback not implemented", args: []string{"readback"}, wantCode: 1, wantStderr: "not implemented yet"},
		{name: "unknown command", args: []string{"bogus"}, wantCode: 2, wantStderr: `unknown command "bogus"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(tt.args, strings.NewReader(""), &stdout, &stderr); got != tt.wantCode {
				t.Errorf("exit code = %d, want %d", got, tt.wantCode)
			}
			if tt.wantStdout != "" && !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}
