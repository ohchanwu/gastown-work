package cmd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

func TestCaptureSpawnedPolecatBranchReceiptReturnsPartialGenerationOnRevFailure(t *testing.T) {
	previous := resolveSpawnedPolecatBranchOID
	resolveSpawnedPolecatBranchOID = func(_, _ string) (string, error) {
		return "", errors.New("injected rev failure")
	}
	t.Cleanup(func() { resolveSpawnedPolecatBranchOID = previous })

	for _, kind := range []string{"reused", "spawned"} {
		t.Run(kind, func(t *testing.T) {
			want := &SpawnedPolecatInfo{
				RigName: "gastown", PolecatName: "toast", ClonePath: "/tmp/toast",
				Branch: "polecat/toast/work", Incarnation: kind + "-generation",
			}
			got, err := captureSpawnedPolecatBranchReceipt(want, "/tmp/rig", kind)
			if err == nil || err.Error() != fmt.Sprintf("capturing %s polecat branch receipt: injected rev failure", kind) {
				t.Fatalf("capture error = %v", err)
			}
			if got != want {
				t.Fatalf("partial receipt = %p, want original %p", got, want)
			}
			if got.Incarnation == "" || got.Branch == "" || got.ClonePath == "" || got.BranchOID != "" {
				t.Fatalf("partial generation receipt = %+v", got)
			}
		})
	}
}

func TestEffectivePolecatDirCap(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		want       int
	}{
		{"negative uses floor", -1, minPolecatDirsPerRig},
		{"zero uses floor", 0, minPolecatDirsPerRig},
		{"default below floor uses floor", 10, minPolecatDirsPerRig},
		{"one below floor uses floor", minPolecatDirsPerRig - 1, minPolecatDirsPerRig},
		{"floor remains floor", minPolecatDirsPerRig, minPolecatDirsPerRig},
		{"above floor is honored", 45, 45},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectivePolecatDirCap(tt.configured); got != tt.want {
				t.Errorf("effectivePolecatDirCap(%d) = %d, want %d", tt.configured, got, tt.want)
			}
		})
	}
}

func TestPolecatStartupStateCommitCompensatesSecondWriteFailure(t *testing.T) {
	const incarnation = "generation-1"
	agentDescriptionState := "spawning"
	workStatus := "open"
	workAssignee := "gastown/polecats/toast"
	workFailure := errors.New("work state outcome unknown")
	restored := false

	commit := &polecatStartupStateCommit{
		capture: func(got string) (polecat.StartupStateReceipt, error) {
			if got != incarnation {
				t.Fatalf("capture incarnation = %q", got)
			}
			return polecat.StartupStateReceipt{
				Incarnation: incarnation, AgentState: agentDescriptionState,
				WorkIssueID: "gt-work", WorkStatus: workStatus,
				WorkAssignee: workAssignee, WorkStateChange: true,
			}, nil
		},
		setAgent: func(string) error {
			agentDescriptionState = "working"
			return nil
		},
		setWork: func(string) error {
			workStatus = "in_progress"
			return workFailure
		},
		restore: func(receipt polecat.StartupStateReceipt) error {
			restored = true
			if agentDescriptionState != "working" || workStatus != "in_progress" || workAssignee != receipt.WorkAssignee {
				t.Fatalf("pre-compensation state = agent:%s work:%s assignee:%s", agentDescriptionState, workStatus, workAssignee)
			}
			agentDescriptionState = receipt.AgentState
			workStatus = receipt.WorkStatus
			workAssignee = receipt.WorkAssignee
			return nil
		},
	}

	if err := commit.onStarted(incarnation); !errors.Is(err, workFailure) {
		t.Fatalf("onStarted error = %v", err)
	}
	if err := commit.onStartFailed(incarnation); err != nil {
		t.Fatalf("onStartFailed: %v", err)
	}
	if !restored || agentDescriptionState != "spawning" || workStatus != "open" || workAssignee != "gastown/polecats/toast" {
		t.Fatalf("restored=%v agent=%s work=%s assignee=%s", restored, agentDescriptionState, workStatus, workAssignee)
	}
}
