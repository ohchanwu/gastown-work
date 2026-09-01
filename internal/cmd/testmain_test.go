//go:build !integration

package cmd

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

func TestMain(m *testing.M) {
	BuiltProperly = "1"
	if runSessionBrokerReexecHelper() {
		os.Exit(Execute())
	}
	if code, requested := runSessionBrokerRawClientHelper(); requested {
		os.Exit(code)
	}
	if os.Getenv("GT_TEST_CMD_EXECUTE_HELPER") == "1" {
		os.Exit(Execute())
	}
	baseline, baselineErr := doltserver.FindAllDoltListenersWithError()
	code := m.Run()
	current, currentErr := doltserver.FindAllDoltListenersWithError()
	code, leaked, inventoryErr := cmdTestMainResult(code, baseline, baselineErr, current, currentErr)
	if inventoryErr != nil {
		fmt.Fprintf(os.Stderr, "cmd TestMain: %v\n", inventoryErr)
	}
	if len(leaked) > 0 {
		fmt.Fprintf(os.Stderr, "cmd TestMain: new Dolt listener PIDs remained at package exit: %v\n", leaked)
	}
	os.Exit(code)
}

func cmdTestMainResult(testCode int, baseline []doltserver.DoltListener, baselineErr error, current []doltserver.DoltListener, currentErr error) (int, []int, error) {
	var inventoryErr error
	if baselineErr != nil {
		inventoryErr = fmt.Errorf("capturing baseline Dolt listeners: %w", baselineErr)
	}
	if currentErr != nil {
		inventoryErr = errors.Join(inventoryErr, fmt.Errorf("capturing final Dolt listeners: %w", currentErr))
	}

	var leaked []int
	if inventoryErr == nil {
		leaked = newListenerPIDs(baseline, current)
	}
	if testCode == 0 && (inventoryErr != nil || len(leaked) > 0) {
		testCode = 1
	}
	return testCode, leaked, inventoryErr
}

func TestCmdTestMainResultFailsClosedOnInventoryErrors(t *testing.T) {
	listener := doltserver.DoltListener{PID: 42, Port: 3307}
	tests := []struct {
		name        string
		testCode    int
		baseline    []doltserver.DoltListener
		baselineErr error
		current     []doltserver.DoltListener
		currentErr  error
		wantCode    int
		wantLeaked  int
		wantErr     bool
	}{
		{name: "baseline inventory failure", baselineErr: errors.New("baseline failed"), wantCode: 1, wantErr: true},
		{name: "post inventory failure", currentErr: errors.New("post failed"), wantCode: 1, wantErr: true},
		{name: "preserve original test failure", testCode: 7, currentErr: errors.New("post failed"), wantCode: 7, wantErr: true},
		{name: "new listener", current: []doltserver.DoltListener{listener}, wantCode: 1, wantLeaked: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, leaked, err := cmdTestMainResult(tt.testCode, tt.baseline, tt.baselineErr, tt.current, tt.currentErr)
			if code != tt.wantCode || len(leaked) != tt.wantLeaked || (err != nil) != tt.wantErr {
				t.Fatalf("cmdTestMainResult() = code %d, leaked %v, err %v; want code %d, %d leaked, error %v", code, leaked, err, tt.wantCode, tt.wantLeaked, tt.wantErr)
			}
		})
	}
}
