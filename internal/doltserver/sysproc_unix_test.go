//go:build !windows

package doltserver

import "testing"

func TestProcessStatusIsAliveRejectsZombie(t *testing.T) {
	for _, status := range []string{"Z", "Z+", " Zs "} {
		if processStatusIsAlive(status) {
			t.Fatalf("zombie status %q reported alive", status)
		}
	}
	if !processStatusIsAlive("S+") {
		t.Fatal("sleeping process reported absent")
	}
}
