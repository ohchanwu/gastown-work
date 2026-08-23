package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

func TestMailHelpUsesTownRootMessagingConfig(t *testing.T) {
	const want = "<town-root>/config/messaging.json"

	for _, command := range []struct {
		name string
		long string
	}{
		{name: "send", long: mailSendCmd.Long},
		{name: "announces", long: mailAnnouncesCmd.Long},
	} {
		t.Run(command.name, func(t *testing.T) {
			if !strings.Contains(command.long, want) {
				t.Fatalf("help should document %q:\n%s", want, command.long)
			}
			if strings.Contains(command.long, "~/gt/config/messaging.json") {
				t.Fatalf("help should not document the obsolete home-relative path:\n%s", command.long)
			}
		})
	}
}

// TestClaimPatternMatching tests claim pattern matching via the beads package.
// This verifies that the pattern matching used for queue eligibility works correctly.
func TestClaimPatternMatching(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		caller  string
		want    bool
	}{
		// Exact matches
		{
			name:    "exact match",
			pattern: "gastown/polecats/capable",
			caller:  "gastown/polecats/capable",
			want:    true,
		},
		{
			name:    "exact match with different name",
			pattern: "gastown/polecats/toast",
			caller:  "gastown/polecats/capable",
			want:    false,
		},

		// Wildcard at end
		{
			name:    "wildcard matches polecat",
			pattern: "gastown/polecats/*",
			caller:  "gastown/polecats/capable",
			want:    true,
		},
		{
			name:    "wildcard matches different polecat",
			pattern: "gastown/polecats/*",
			caller:  "gastown/polecats/toast",
			want:    true,
		},
		{
			name:    "wildcard doesn't match wrong rig",
			pattern: "gastown/polecats/*",
			caller:  "beads/polecats/capable",
			want:    false,
		},
		{
			name:    "wildcard doesn't match nested path",
			pattern: "gastown/polecats/*",
			caller:  "gastown/polecats/sub/capable",
			want:    false,
		},

		// Universal wildcard
		{
			name:    "universal wildcard matches anything",
			pattern: "*",
			caller:  "anything",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := beads.MatchClaimPattern(tt.pattern, tt.caller)
			if got != tt.want {
				t.Errorf("MatchClaimPattern(%q, %q) = %v, want %v",
					tt.pattern, tt.caller, got, tt.want)
			}
		})
	}
}

// TestMailAnnounces tests the announces command functionality.
func TestMailAnnounces(t *testing.T) {
	t.Run("listAnnounceChannels with nil config", func(t *testing.T) {
		// Test with nil announces map
		cfg := &config.MessagingConfig{
			Announces: nil,
		}

		// Reset flag to default
		mailAnnouncesJSON = false

		// This should not panic and should handle nil gracefully
		// We can't easily capture stdout in unit tests, but we can verify no panic
		err := listAnnounceChannels(cfg)
		if err != nil {
			t.Errorf("listAnnounceChannels with nil announces should not error: %v", err)
		}
	})

	t.Run("listAnnounceChannels with empty config", func(t *testing.T) {
		cfg := &config.MessagingConfig{
			Announces: make(map[string]config.AnnounceConfig),
		}

		mailAnnouncesJSON = false
		err := listAnnounceChannels(cfg)
		if err != nil {
			t.Errorf("listAnnounceChannels with empty announces should not error: %v", err)
		}
	})

	t.Run("readAnnounceChannel validates channel exists", func(t *testing.T) {
		cfg := &config.MessagingConfig{
			Announces: map[string]config.AnnounceConfig{
				"alerts": {
					Readers:     []string{"@town"},
					RetainCount: 100,
				},
			},
		}

		// Test with unknown channel
		err := readAnnounceChannel("/tmp", cfg, "nonexistent")
		if err == nil {
			t.Error("readAnnounceChannel should error for unknown channel")
		}
		if !strings.Contains(err.Error(), "unknown announce channel") {
			t.Errorf("error should mention 'unknown announce channel', got: %v", err)
		}
	})

	t.Run("readAnnounceChannel errors on nil announces", func(t *testing.T) {
		cfg := &config.MessagingConfig{
			Announces: nil,
		}

		err := readAnnounceChannel("/tmp", cfg, "alerts")
		if err == nil {
			t.Error("readAnnounceChannel should error for nil announces")
		}
		if !strings.Contains(err.Error(), "no announce channels configured") {
			t.Errorf("error should mention 'no announce channels configured', got: %v", err)
		}
	})
}

// TestAnnounceMessageParsing tests parsing of announce messages from beads output.
func TestAnnounceMessageParsing(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{
			name:   "extracts from label",
			labels: []string{"from:mayor/", "announce_channel:alerts"},
			want:   "mayor/",
		},
		{
			name:   "extracts from with rig path",
			labels: []string{"announce_channel:alerts", "from:gastown/witness"},
			want:   "gastown/witness",
		},
		{
			name:   "no from label",
			labels: []string{"announce_channel:alerts"},
			want:   "",
		},
		{
			name:   "empty labels",
			labels: []string{},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the label extraction logic from listAnnounceMessages
			var from string
			for _, label := range tt.labels {
				if strings.HasPrefix(label, "from:") {
					from = strings.TrimPrefix(label, "from:")
					break
				}
			}
			if from != tt.want {
				t.Errorf("extracting from label: got %q, want %q", from, tt.want)
			}
		})
	}
}
