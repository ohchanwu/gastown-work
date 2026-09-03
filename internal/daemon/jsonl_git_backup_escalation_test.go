package daemon

import "testing"

func TestJSONLBackupEscalationArgsKeepStableIdentityAcrossMeasurements(t *testing.T) {
	tests := []struct {
		condition   string
		fingerprint string
	}{
		{condition: "pollution-survived", fingerprint: "jsonl-backup:pollution-survived"},
		{condition: "export-spike", fingerprint: "jsonl-backup:export-spike"},
		{condition: "push-failure", fingerprint: "jsonl-backup:push-failure"},
	}
	for _, tt := range tests {
		t.Run(tt.condition, func(t *testing.T) {
			first := jsonlBackupEscalationArgs(tt.condition, "3 records; /tmp/first.log")
			second := jsonlBackupEscalationArgs(tt.condition, "19 records; /tmp/second.log")
			assertJSONLBackupEscalationArg(t, first, "-s", "HIGH")
			assertJSONLBackupEscalationArg(t, first, "--fingerprint", tt.fingerprint)
			assertJSONLBackupEscalationArg(t, first, "--scope", "service:jsonl-backup")
			assertJSONLBackupEscalationArg(t, second, "--fingerprint", tt.fingerprint)
			assertJSONLBackupEscalationArg(t, second, "--scope", "service:jsonl-backup")
			if first[len(first)-1] == second[len(second)-1] {
				t.Fatal("measurement messages should differ in this regression fixture")
			}
		})
	}
}

func assertJSONLBackupEscalationArg(t *testing.T, args []string, flag, want string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			if args[i+1] != want {
				t.Fatalf("%s = %q, want %q", flag, args[i+1], want)
			}
			return
		}
	}
	t.Fatalf("args %q missing %s", args, flag)
}
