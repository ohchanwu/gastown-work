package cmd

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

func TestNewListenerPIDsProvesPackageExitBaseline(t *testing.T) {
	baseline := []doltserver.DoltListener{{PID: 10, Port: 3307}, {PID: 11, Port: 4400}}
	after := []doltserver.DoltListener{{PID: 10, Port: 3307}, {PID: 12, Port: 4401}, {PID: 12, Port: 4402}}
	if got, want := newListenerPIDs(baseline, after), []int{12}; !reflect.DeepEqual(got, want) {
		t.Fatalf("newListenerPIDs() = %v, want %v", got, want)
	}
}

func TestCleanupCmdTestDoltRootsAttemptsEveryRegisteredRoot(t *testing.T) {
	forced := errors.New("forced cleanup failure")
	var got []string
	err := cleanupCmdTestDoltRoots([]string{"/test/a", "/test/b"}, func(root string) (int, error) {
		got = append(got, root)
		if root == "/test/a" {
			return 0, forced
		}
		return 1, nil
	})
	if !errors.Is(err, forced) {
		t.Fatalf("cleanup error = %v, want forced failure", err)
	}
	if !slices.Equal(got, []string{"/test/a", "/test/b"}) {
		t.Fatalf("cleaned roots = %v", got)
	}
}
