package testutil

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type cleanupRecorder struct {
	cleanup func()
	errors  []string
	logs    []string
}

func (*cleanupRecorder) Helper()             {}
func (r *cleanupRecorder) Cleanup(fn func()) { r.cleanup = fn }
func (r *cleanupRecorder) Errorf(f string, a ...any) {
	r.errors = append(r.errors, fmt.Sprintf(f, a...))
}
func (r *cleanupRecorder) Logf(f string, a ...any) { r.logs = append(r.logs, fmt.Sprintf(f, a...)) }

func TestReapOwnedDoltOnCleanupFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		stopped    int
		reapErr    error
		wantErrors int
		wantLog    string
	}{
		{name: "cleanup failure", reapErr: errors.New("inventory failed"), wantErrors: 1},
		{name: "nothing stopped"},
		{name: "exact stopped count", stopped: 2, wantLog: "stopped 2 owned Dolt sql-server process(es)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &cleanupRecorder{}
			reapOwnedDoltOnCleanup(recorder, t.TempDir(), func(string) (int, error) {
				return tt.stopped, tt.reapErr
			})
			if recorder.cleanup == nil {
				t.Fatal("cleanup callback was not registered")
			}
			recorder.cleanup()
			if len(recorder.errors) != tt.wantErrors {
				t.Fatalf("cleanup errors = %v, want %d", recorder.errors, tt.wantErrors)
			}
			if tt.reapErr != nil && !strings.Contains(strings.Join(recorder.errors, "\n"), tt.reapErr.Error()) {
				t.Fatalf("cleanup error did not preserve cause: %v", recorder.errors)
			}
			if tt.wantLog != "" && !strings.Contains(strings.Join(recorder.logs, "\n"), tt.wantLog) {
				t.Fatalf("cleanup logs = %v, want %q", recorder.logs, tt.wantLog)
			}
		})
	}
}

func TestDoltTestMainExitCodeFailsOnLifecycleError(t *testing.T) {
	tests := []struct {
		name                 string
		testCode             int
		setupErr, cleanupErr error
		want                 int
	}{
		{name: "success", want: 0},
		{name: "preserve test failure", testCode: 2, want: 2},
		{name: "setup failure", setupErr: errors.New("setup failed"), want: 1},
		{name: "cleanup failure", cleanupErr: errors.New("cleanup failed"), want: 1},
		{name: "cleanup preserves test failure", testCode: 2, cleanupErr: errors.New("cleanup failed"), want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DoltTestMainExitCode(tt.testCode, tt.setupErr, tt.cleanupErr); got != tt.want {
				t.Fatalf("DoltTestMainExitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}
