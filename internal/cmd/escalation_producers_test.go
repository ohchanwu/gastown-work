package cmd

import "testing"

func TestDogEscalationArgsKeepStableIdentityAcrossDiagnostics(t *testing.T) {
	tests := []struct {
		condition   string
		fingerprint string
	}{
		{condition: "session-start", fingerprint: "dog-dispatch:session-start:bravo"},
		{condition: "work-lookup", fingerprint: "dog-dispatch:work-lookup:bravo"},
		{condition: "work-cleared", fingerprint: "dog-dispatch:work-cleared:bravo"},
	}
	for _, tt := range tests {
		t.Run(tt.condition, func(t *testing.T) {
			first := dogEscalationArgs(tt.condition, "Bravo", "failed after 1.2s: /tmp/first.log")
			second := dogEscalationArgs(tt.condition, " bravo ", "failed after 9.8s: /tmp/second.log")
			assertRecurringEscalationIdentity(t, first, "medium", tt.fingerprint, "dog:bravo")
			assertRecurringEscalationIdentity(t, second, "medium", tt.fingerprint, "dog:bravo")
			if first[len(first)-1] == second[len(second)-1] {
				t.Fatal("diagnostic messages should differ in this regression fixture")
			}
		})
	}
}

func TestCrossRigEscalationArgsKeepStableIdentityAcrossBeads(t *testing.T) {
	first := crossRigEscalationArgs("WalletUI", "HQ", "hq-first")
	second := crossRigEscalationArgs(" walletui ", "hq", "hq-second")
	assertRecurringEscalationIdentity(t, first, "medium", "cross-rig-prefix:walletui:hq", "rig:walletui")
	assertRecurringEscalationIdentity(t, second, "medium", "cross-rig-prefix:walletui:hq", "rig:walletui")
	if first[len(first)-1] == second[len(second)-1] {
		t.Fatal("observation messages should retain the distinct bead IDs")
	}
}

func TestPolecatHookEscalationArgsKeepStableIdentityAcrossDetails(t *testing.T) {
	first := polecatHookUnresolvableEscalationArgs("Gastown/Polecats/Furiosa", "lookup failed after 1.2s")
	second := polecatHookUnresolvableEscalationArgs(" gastown/polecats/furiosa ", "lookup failed after 9.8s")
	assertRecurringEscalationIdentity(t, first, "high", "polecat-hook-unresolvable:gastown/polecats/furiosa", "agent:gastown/polecats/furiosa")
	assertRecurringEscalationIdentity(t, second, "high", "polecat-hook-unresolvable:gastown/polecats/furiosa", "agent:gastown/polecats/furiosa")
	if first[len(first)-1] == second[len(second)-1] {
		t.Fatal("diagnostic messages should differ in this regression fixture")
	}
}

func assertRecurringEscalationIdentity(t *testing.T, args []string, severity, fingerprint, scope string) {
	t.Helper()
	want := map[string]string{
		"--severity":    severity,
		"--fingerprint": fingerprint,
		"--scope":       scope,
	}
	for flag, value := range want {
		found := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag {
				found = args[i+1] == value
				break
			}
		}
		if !found {
			t.Fatalf("args %q missing %s %q", args, flag, value)
		}
	}
}
