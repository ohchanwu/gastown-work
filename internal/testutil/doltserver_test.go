//go:build !windows

package testutil

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestIsDockerUnavailableErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "rootless", err: errors.New("testcontainers docker unavailable: rootless Docker not found"), want: true},
		{name: "daemon", err: errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock"), want: true},
		{name: "ordinary", err: errors.New("pulling image failed"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDockerUnavailableErr(tt.err); got != tt.want {
				t.Fatalf("isDockerUnavailableErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestSetSharedDoltEnvOverridesInheritedServerRoute(t *testing.T) {
	for _, key := range []string{
		"GT_DOLT_HOST",
		"GT_DOLT_PORT",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_PORT",
		"BEADS_DOLT_AUTO_START",
	} {
		t.Setenv(key, "production")
	}

	setSharedDoltEnv("4407")

	want := map[string]string{
		"GT_DOLT_HOST":           "127.0.0.1",
		"GT_DOLT_PORT":           "4407",
		"BEADS_DOLT_SERVER_HOST": "127.0.0.1",
		"BEADS_DOLT_SERVER_PORT": "4407",
		"BEADS_DOLT_PORT":        "4407",
		"BEADS_DOLT_AUTO_START":  "0",
		"GT_TEST_EXTERNAL_DOLT":  "1",
	}
	for key, value := range want {
		if got := os.Getenv(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
}

func TestEnsureDoltContainerForTestMainPoisonsRouteWhenDockerUnavailable(t *testing.T) {
	for _, key := range []string{
		"GT_DOLT_HOST",
		"GT_DOLT_PORT",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_PORT",
		"BEADS_DOLT_AUTO_START",
		"GT_TEST_EXTERNAL_DOLT",
	} {
		t.Setenv(key, "production")
	}

	err := ensureDoltContainerForTestMain(func() bool { return false })
	if err == nil {
		t.Fatal("EnsureDoltContainerForTestMain error = nil, want Docker unavailable")
	}
	for _, key := range []string{"GT_DOLT_PORT", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"} {
		if got := os.Getenv(key); got != "1" {
			t.Errorf("%s = %q, want poisoned port 1", key, got)
		}
	}
	if got := os.Getenv("GT_TEST_DOLT_UNAVAILABLE"); got != "1" {
		t.Errorf("GT_TEST_DOLT_UNAVAILABLE = %q, want 1", got)
	}
	if got := os.Getenv("GT_TEST_EXTERNAL_DOLT"); got != "" {
		t.Errorf("GT_TEST_EXTERNAL_DOLT = %q, want unset", got)
	}
}

func TestTerminateDoltContainerOnCleanupFailsResponsibleTest(t *testing.T) {
	want := errors.New("container runtime refused termination")
	recorder := &cleanupRecorder{}

	terminateDoltContainerOnCleanup(recorder, func() error { return want })

	if len(recorder.errors) != 1 || !strings.Contains(recorder.errors[0], want.Error()) {
		t.Fatalf("cleanup errors = %v, want preserved cause %q", recorder.errors, want)
	}
}

func TestTerminateDoltContainerOnCleanupAcceptsSuccess(t *testing.T) {
	recorder := &cleanupRecorder{}

	terminateDoltContainerOnCleanup(recorder, func() error { return nil })

	if len(recorder.errors) != 0 {
		t.Fatalf("cleanup errors = %v, want none", recorder.errors)
	}
}
