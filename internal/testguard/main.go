package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/workspace"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: testguard snapshot|cleanup RECEIPT LAUNCHER_PID OWNER_ROOT")
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "snapshot":
		if len(os.Args) != 5 {
			err = fmt.Errorf("usage: testguard snapshot RECEIPT LAUNCHER_PID OWNER_ROOT")
			break
		}
		launcherPID, parseErr := positiveInt(os.Args[3], "launcher PID")
		if parseErr != nil {
			err = parseErr
			break
		}
		var baseline []doltserver.DoltListener
		var townRoot string
		townRoot, err = workspace.FindFromCwdOrError()
		if err == nil {
			baseline, err = localDoltListenersWith(townRoot, doltserver.InventoryLocalDoltServersWithError)
		}
		if err == nil {
			_, err = requiredBaselineListener(baseline, launcherPID)
		}
		if err == nil {
			err = writeBaseline(os.Args[2], baseline)
		}
	case "cleanup":
		if len(os.Args) != 5 {
			err = fmt.Errorf("usage: testguard cleanup RECEIPT LAUNCHER_PID OWNER_ROOT")
			break
		}
		var launcherPID int
		launcherPID, err = positiveInt(os.Args[3], "launcher PID")
		if err == nil {
			err = cleanupSinceBaseline(os.Args[2], launcherPID, os.Args[4])
		}
	case "identity":
		if len(os.Args) != 5 {
			err = fmt.Errorf("usage: testguard identity LAUNCHER_PID PORT OWNER_ROOT")
			break
		}
		var selection doltserver.TestLeakSelection
		selection, err = launcherSelection(os.Args[2], os.Args[3], os.Args[4])
		if err == nil {
			fmt.Fprintln(os.Stdout, selection.OwnershipToken)
		}
	case "custody":
		if len(os.Args) != 4 {
			err = fmt.Errorf("usage: testguard custody LAUNCHER_PID PARENT_PID")
			break
		}
		var pid, parentPID int
		pid, err = positiveInt(os.Args[2], "launcher PID")
		if err == nil {
			parentPID, err = positiveInt(os.Args[3], "parent PID")
		}
		if err == nil {
			var custody doltserver.TestProcessCustody
			custody, err = doltserver.CaptureTestProcessCustody(pid, parentPID)
			if err == nil {
				fmt.Fprintln(os.Stdout, custody.OwnershipToken)
			}
		}
	case "stop-custody":
		if len(os.Args) != 5 {
			err = fmt.Errorf("usage: testguard stop-custody LAUNCHER_PID PARENT_PID OWNERSHIP_TOKEN")
			break
		}
		var pid, parentPID int
		pid, err = positiveInt(os.Args[2], "launcher PID")
		if err == nil {
			parentPID, err = positiveInt(os.Args[3], "parent PID")
		}
		if err == nil {
			err = doltserver.TerminateTestProcessCustody(doltserver.TestProcessCustody{
				PID: pid, ParentPID: parentPID, OwnershipToken: os.Args[4],
			})
		}
	case "stop":
		if len(os.Args) != 6 {
			err = fmt.Errorf("usage: testguard stop LAUNCHER_PID PORT OWNER_ROOT OWNERSHIP_TOKEN")
			break
		}
		var selection doltserver.TestLeakSelection
		selection, err = launcherSelection(os.Args[2], os.Args[3], os.Args[4])
		if err == nil && selection.OwnershipToken != os.Args[5] {
			err = fmt.Errorf("launcher process identity changed")
		}
		if err == nil {
			var townRoot string
			townRoot, err = workspace.FindFromCwdOrError()
			if err == nil {
				_, err = doltserver.RemediatePreviewedTestLeaks(townRoot, []doltserver.TestLeakSelection{selection}, true)
			}
		}
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "testguard: %v\n", err)
		os.Exit(1)
	}
}

func positiveInt(value, name string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return n, nil
}

func launcherSelection(pidValue, portValue, ownerRoot string) (doltserver.TestLeakSelection, error) {
	pid, err := positiveInt(pidValue, "launcher PID")
	if err != nil {
		return doltserver.TestLeakSelection{}, err
	}
	port, err := positiveInt(portValue, "launcher port")
	if err != nil || port > 65535 {
		return doltserver.TestLeakSelection{}, fmt.Errorf("launcher port must be in the TCP port range")
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return doltserver.TestLeakSelection{}, fmt.Errorf("find Gas Town workspace: %w", err)
	}
	inventory, err := doltserver.InventoryLocalDoltServersWithError(townRoot)
	if err != nil {
		return doltserver.TestLeakSelection{}, err
	}
	for _, server := range inventory {
		if server.PID == pid && server.Port == port && pathWithinOrEqual(ownerRoot, server.OwnerPath) {
			selections := doltserver.TestLeakSelections([]doltserver.LocalDoltServer{server})
			if len(selections) == 1 {
				return selections[0], nil
			}
		}
	}
	return doltserver.TestLeakSelection{}, fmt.Errorf("launcher listener identity is not positively owned")
}

func pathWithinOrEqual(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func writeBaseline(path string, baseline []doltserver.DoltListener) error {
	data, err := json.Marshal(baseline)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create private baseline receipt: %w", err)
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write baseline receipt: %w", err)
	}
	return file.Close()
}

func requiredBaselineListener(baseline []doltserver.DoltListener, launcherPID int) (doltserver.DoltListener, error) {
	for _, listener := range baseline {
		if listener.PID == launcherPID {
			return listener, nil
		}
	}
	return doltserver.DoltListener{}, fmt.Errorf("launcher PID %d missing from listener inventory", launcherPID)
}

func localDoltListenersWith(townRoot string, inventory func(string) ([]doltserver.LocalDoltServer, error)) ([]doltserver.DoltListener, error) {
	servers, err := inventory(townRoot)
	if err != nil {
		return nil, err
	}
	listeners := make([]doltserver.DoltListener, len(servers))
	for i, server := range servers {
		listeners[i] = server.DoltListener
	}
	return listeners, nil
}

func cleanupSinceBaseline(path string, launcherPID int, ownerRoot string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat baseline receipt: %w", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("baseline receipt must be private")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read baseline receipt: %w", err)
	}
	var baseline []doltserver.DoltListener
	if err := json.Unmarshal(data, &baseline); err != nil {
		return fmt.Errorf("decode baseline receipt: %w", err)
	}
	required, err := requiredBaselineListener(baseline, launcherPID)
	if err != nil {
		return err
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("find Gas Town workspace: %w", err)
	}
	current, err := localDoltListenersWith(townRoot, doltserver.InventoryLocalDoltServersWithError)
	if err != nil {
		return err
	}
	found := false
	for _, listener := range current {
		if listener == required {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("launcher listener PID %d port %d missing during cleanup", required.PID, required.Port)
	}
	return doltserver.CleanupOwnedLocalTestLeaks(townRoot, baseline, ownerRoot)
}
