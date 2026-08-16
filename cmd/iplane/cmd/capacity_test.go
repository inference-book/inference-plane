package cmd

import (
	"bytes"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// The display half of the rule the fabric package enforces in code. A host
// that reported nothing must not render as a host that reported zero, because
// an operator scanning a column of numbers would read "0" as a measurement and
// rank it against real ones.
func TestRenderCandidatesNeverPrintsUnknownFabricAsZero(t *testing.T) {
	var buf bytes.Buffer
	err := renderCandidates(&buf, answersFor("vast"), []*provisioners.Candidate{{
		HostId: "9871", OfferId: "2204551", Sku: "A100_PCIE", GpuCount: 4,
		PriceUsdPerHour: 1.44,
		FabricScope:     provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED,
		FabricSource:    provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN,
	}}, nil, "table")
	if err != nil {
		t.Fatalf("renderCandidates: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "unknown") {
		t.Errorf("output does not say the reading is unknown:\n%s", out)
	}
	if strings.Contains(out, "0 Gb") {
		t.Errorf("output renders an absent reading as a zero measurement:\n%s", out)
	}
}

// Measured and declared are both real verdicts and the difference decides
// whether a candidate can be trusted on that axis, so the provenance has to
// survive into the column an operator actually reads.
func TestRenderCandidatesDistinguishesMeasuredFromDeclared(t *testing.T) {
	var buf bytes.Buffer
	err := renderCandidates(&buf, answersFor("vast"), []*provisioners.Candidate{
		{HostId: "1", Sku: "A100_PCIE",
			FabricScope:  provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
			FabricSource: provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED,
			FabricGbps:   2400},
		{HostId: "2", Sku: "A100_SXM4",
			FabricScope:  provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
			FabricSource: provisionerv1.FabricSource_FABRIC_SOURCE_DECLARED,
			FabricGbps:   4800},
	}, nil, "table")
	if err != nil {
		t.Fatalf("renderCandidates: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "measured") || !strings.Contains(out, "declared") {
		t.Errorf("output collapses provenance:\n%s", out)
	}
}

// An empty result is the one place the wording matters most: it is the moment
// an operator decides whether to widen their requirements or go somewhere
// else, and "no candidates" alone does not tell them which.
func TestRenderCandidatesEmptySaysTheProviderWasAsked(t *testing.T) {
	var buf bytes.Buffer
	if err := renderCandidates(&buf, answersFor("vast"), nil, nil, "table"); err != nil {
		t.Fatalf("renderCandidates: %v", err)
	}

	// The per-provider line is what carries the distinction now: "answered,
	// no capacity" against the "cannot answer" wording a provider without the
	// capability gets. Both must name the provider, because with several in
	// play a bare summary line cannot say which one had nothing.
	out := buf.String()
	if !strings.Contains(out, "answered") {
		t.Errorf("empty output does not say the provider was asked and answered:\n%s", out)
	}
	if !strings.Contains(out, "vast") {
		t.Errorf("empty output does not name which provider had nothing:\n%s", out)
	}
}

// buildReadOnlyService deliberately opens the store without LockForLifetime,
// so `iplane capacity` answers while a daemon is up. That is the moment an
// operator is most likely to be asking whether there is capacity to scale
// onto, and a command that refused exactly then would be useless.
//
// This pins the invariant the command depends on: opening a store whose lock
// another process holds must succeed, and only taking the lock must fail.
func TestReadOnlyStoreOpenSucceedsWhileLockHeld(t *testing.T) {
	dir := t.TempDir()

	daemon, err := file.Open(dir, "daemon")
	if err != nil {
		t.Fatalf("open store for the holder: %v", err)
	}
	if _, err := daemon.LockForLifetime(); err != nil {
		t.Fatalf("holder could not take the lock: %v", err)
	}

	// What buildReadOnlyService does: open, never lock.
	reader, err := file.Open(dir, "reader")
	if err != nil {
		t.Fatalf("read-only open failed while the lock was held: %v", err)
	}

	// And the contrast, so this test fails if opening ever starts locking:
	// the write path must still be refused.
	if _, err := reader.LockForLifetime(); err == nil {
		t.Error("a second LockForLifetime succeeded; the lock is not actually exclusive, so this test proves nothing")
	}
}

// The reassurance is the point of the command, so it should not be possible to
// read the output and wonder whether something got rented.
func TestRenderCandidatesSaysNothingWasRented(t *testing.T) {
	var buf bytes.Buffer
	err := renderCandidates(&buf, answersFor("vast"), []*provisioners.Candidate{
		{HostId: "1", Sku: "A100_PCIE", PriceUsdPerHour: 1.10},
	}, nil, "table")
	if err != nil {
		t.Fatalf("renderCandidates: %v", err)
	}

	if !strings.Contains(strings.ToLower(buf.String()), "nothing was rented") {
		t.Errorf("output does not state that nothing was rented:\n%s", buf.String())
	}
}

// answersFor builds the single-provider ProviderAnswer slice these rendering
// tests need, so each one keeps stating only the thing it is about.
func answersFor(provider string, cands ...*provisioners.Candidate) []*provisioners.ProviderAnswer {
	outcome := provisionerv1.AnswerOutcome_ANSWER_OUTCOME_ANSWERED
	if len(cands) == 0 {
		outcome = provisionerv1.AnswerOutcome_ANSWER_OUTCOME_NO_CAPACITY
	}
	return []*provisioners.ProviderAnswer{{Provider: provider, Outcome: outcome, Candidates: cands}}
}

// A typo'd --reclaim must not fall through to "no preference". That would hand
// an operator the full-price tier they were explicitly trying to avoid, which
// is precisely the failure #288 named.
func TestParseReclaimPolicyRejectsTypos(t *testing.T) {
	for _, in := range []string{"ye", "spot", "interruptible-please", "1"} {
		if _, err := parseReclaimPolicy(in); err == nil {
			t.Errorf("parseReclaimPolicy(%q) accepted a value it does not understand", in)
		}
	}
}

// The three understood spellings, and the empty default that keeps the Ch 6-10
// behaviour free.
func TestParseReclaimPolicyAcceptsTheDocumentedForms(t *testing.T) {
	cases := map[string]provisionerv1.ReclaimPolicy{
		"":          provisionerv1.ReclaimPolicy_RECLAIM_POLICY_UNSPECIFIED,
		"yes":       provisionerv1.ReclaimPolicy_RECLAIM_POLICY_PREFERRED,
		"no":        provisionerv1.ReclaimPolicy_RECLAIM_POLICY_NEVER,
		"on-demand": provisionerv1.ReclaimPolicy_RECLAIM_POLICY_NEVER,
	}
	for in, want := range cases {
		got, err := parseReclaimPolicy(in)
		if err != nil {
			t.Errorf("parseReclaimPolicy(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseReclaimPolicy(%q) = %v, want %v", in, got, want)
		}
	}
}

// The tier belongs in a column, not a footnote, because it changes what the
// number beside it means: an hourly rate on capacity that can be taken back is
// not the same kind of number as one that cannot.
func TestRenderCandidatesNamesTheTier(t *testing.T) {
	var buf bytes.Buffer
	err := renderCandidates(&buf, answersFor("vast"), []*provisioners.Candidate{
		{Provider: "vast", Sku: "A100_SXM4", PriceUsdPerHour: 0.13, Reclaimable: true},
		{Provider: "vast", Sku: "A100_SXM4", PriceUsdPerHour: 0.83},
	}, nil, "table")
	if err != nil {
		t.Fatalf("renderCandidates: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "reclaimable") || !strings.Contains(out, "on-demand") {
		t.Errorf("output does not distinguish the two tiers:\n%s", out)
	}
}
