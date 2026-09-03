package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/mail"
)

func TestMailSendJSONReturnsRecipientSourceIDs(t *testing.T) {
	cmd := &cobra.Command{}
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)

	receipts := []mailSendReceipt{
		{Recipient: "gastown/witness", MessageID: "hq-wisp-a", SourceID: "msg-0123456789abcdef"},
		{Recipient: "gastown/refinery", MessageID: "hq-wisp-b", SourceID: "msg-fedcba9876543210"},
	}
	if err := writeMailSendResult(cmd, true, "@rig/gastown", "Status", receipts, nil, mail.TypeNotification); err != nil {
		t.Fatalf("writeMailSendResult: %v", err)
	}

	var got struct {
		Deliveries []mailSendReceipt `json:"deliveries"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("JSON output: %v\n%s", err, stdout.String())
	}
	if len(got.Deliveries) != 2 || got.Deliveries[0] != receipts[0] || got.Deliveries[1] != receipts[1] {
		t.Fatalf("deliveries = %#v, want %#v", got.Deliveries, receipts)
	}
}

func TestHasReplyPrefix(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"Re: Status", true},
		{"re: status", true},
		{"RE:status", true},
		{"  Re: leading ws", true},
		{"Reply: not a Re prefix", false},
		{"Status", false},
		{"", false},
		{"Re", false},
		{":empty", false},
	}
	for _, c := range cases {
		if got := hasReplyPrefix(c.in); got != c.want {
			t.Errorf("hasReplyPrefix(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNormalizeReplySubject(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Re: Hello", "hello"},
		{"re:hello", "hello"},
		{"Re: Re: Re: nested", "nested"},
		{"  Re:  spaced  ", "spaced"},
		{"plain subject", "plain subject"},
		{"", ""},
		{"Re: ", ""},
	}
	for _, c := range cases {
		if got := normalizeReplySubject(c.in); got != c.want {
			t.Errorf("normalizeReplySubject(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeAddress(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"mayor/", "mayor"},
		{"Mayor", "mayor"},
		{"  gastown/Toast/  ", "gastown/toast"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeAddress(c.in); got != c.want {
			t.Errorf("normalizeAddress(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPickReplyTo(t *testing.T) {
	msg := func(id, from, subj string) *mail.Message {
		return &mail.Message{ID: id, From: from, Subject: subj}
	}

	t.Run("exact match returns id", func(t *testing.T) {
		msgs := []*mail.Message{
			msg("bd-1", "deacon/", "Build broken"),
			msg("bd-2", "witness/", "Build broken"),
		}
		if got := pickReplyTo(msgs, "deacon/", "Re: Build broken"); got != "bd-1" {
			t.Errorf("got %q, want bd-1", got)
		}
	})

	t.Run("address normalization", func(t *testing.T) {
		msgs := []*mail.Message{msg("bd-3", "Mayor", "[HIGH] alert")}
		if got := pickReplyTo(msgs, "mayor/", "Re: [HIGH] alert"); got != "bd-3" {
			t.Errorf("got %q, want bd-3", got)
		}
	})

	t.Run("nested Re prefixes normalize", func(t *testing.T) {
		msgs := []*mail.Message{msg("bd-4", "deacon/", "Re: original")}
		if got := pickReplyTo(msgs, "deacon/", "Re: Re: original"); got != "bd-4" {
			t.Errorf("got %q, want bd-4", got)
		}
	})

	t.Run("ambiguous match returns empty", func(t *testing.T) {
		msgs := []*mail.Message{
			msg("bd-5", "deacon/", "stuck"),
			msg("bd-6", "deacon/", "stuck"),
		}
		if got := pickReplyTo(msgs, "deacon/", "Re: stuck"); got != "" {
			t.Errorf("got %q, want empty (ambiguous)", got)
		}
	})

	t.Run("wrong sender returns empty", func(t *testing.T) {
		msgs := []*mail.Message{msg("bd-7", "witness/", "important")}
		if got := pickReplyTo(msgs, "deacon/", "Re: important"); got != "" {
			t.Errorf("got %q, want empty (wrong sender)", got)
		}
	})

	t.Run("wrong subject returns empty", func(t *testing.T) {
		msgs := []*mail.Message{msg("bd-8", "deacon/", "alpha")}
		if got := pickReplyTo(msgs, "deacon/", "Re: beta"); got != "" {
			t.Errorf("got %q, want empty (wrong subject)", got)
		}
	})

	t.Run("empty subject after strip returns empty", func(t *testing.T) {
		msgs := []*mail.Message{msg("bd-9", "deacon/", "")}
		if got := pickReplyTo(msgs, "deacon/", "Re: "); got != "" {
			t.Errorf("got %q, want empty (degenerate)", got)
		}
	})

	t.Run("empty message list returns empty", func(t *testing.T) {
		if got := pickReplyTo(nil, "deacon/", "Re: anything"); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestBindReviewMetadataInheritsOnlyFromExactRequest(t *testing.T) {
	const exactSHA = "0123456789abcdef0123456789abcdef01234567"
	request := &mail.Message{
		ID: "hq-review-request", From: "mayor/", To: "gastown/witness", Subject: "review",
		Type: mail.TypeTask,
		Review: &mail.ReviewMetadata{
			Lineage: "notification-convergence", Generation: 5, ExactSHA: exactSHA,
		},
	}
	reply := &mail.Message{
		ID: "msg-review-reply", From: "gastown/witness", To: "mayor/", Subject: "verdict",
		Type: mail.TypeReply, ReplyTo: request.ID,
	}
	if err := bindReviewMetadata(reply, request, "", 0, "", "approved"); err != nil {
		t.Fatalf("bindReviewMetadata: %v", err)
	}
	if reply.Review == nil || reply.Review.Lineage != request.Review.Lineage ||
		reply.Review.Generation != 5 || reply.Review.ExactSHA != exactSHA ||
		reply.Review.Verdict != mail.ReviewApproved {
		t.Fatalf("bound review = %#v", reply.Review)
	}

	wrongSHA := *reply
	wrongSHA.Review = nil
	if err := bindReviewMetadata(&wrongSHA, request, "", 0,
		"fedcba9876543210fedcba9876543210fedcba98", "approved"); err == nil {
		t.Fatal("bindReviewMetadata accepted a verdict SHA that disagrees with its exact request")
	}
	if err := bindReviewMetadata(&wrongSHA, nil, "", 0, "", "approved"); err == nil {
		t.Fatal("bindReviewMetadata accepted a verdict without its exact reply target")
	}
}
