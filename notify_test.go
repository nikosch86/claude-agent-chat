package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func encode(t *testing.T, r Record) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestNotifyLinePassesSmallRecordThrough(t *testing.T) {
	r := Record{Ts: 1783922833.455, From: "alice", To: "@bob", Text: "a short reply"}
	line := encode(t, r)
	if got := notifyLine(line, r); string(got) != string(line) {
		t.Errorf("small record should pass through untouched:\n got %s\nwant %s", got, line)
	}
}

func TestNotifyLineReplacesOversizedRecordWithNotice(t *testing.T) {
	body := "the first sentence explains the problem. " + strings.Repeat("filler ", 500)
	r := Record{Ts: 1783922833.455, From: "alice", To: "@bob", Text: body}
	out := notifyLine(encode(t, r), r)

	if len(out) > listenNotifyMaxBytes {
		t.Fatalf("notice is %d bytes, over the %d cap that gets clipped: %s", len(out), listenNotifyMaxBytes, out)
	}

	var n notice
	if err := json.Unmarshal(out, &n); err != nil {
		t.Fatalf("notice is not valid JSON: %v: %s", err, out)
	}
	if !n.Clipped {
		t.Error("notice should be marked clipped so the peer knows it holds a fragment")
	}
	if n.Bytes != len(body) {
		t.Errorf("bytes = %d, want %d", n.Bytes, len(body))
	}
	if !strings.HasPrefix(n.Preview, "the first sentence explains the problem.") {
		t.Errorf("preview should open with the body, got %q", n.Preview)
	}
	if !strings.Contains(n.Full, "history --id "+r.Ts.String()) {
		t.Errorf("full should name the command that fetches the whole body, got %q", n.Full)
	}
	// The full text must never ride along under a key a peer could mistake for
	// the complete message.
	if strings.Contains(string(out), `"text"`) {
		t.Errorf("notice must not carry a text key: %s", out)
	}
}

// A body of nothing but JSON-escaped characters inflates ~6x when encoded. The
// notice must still come in under the cap — this is the case a fixed rune budget
// would silently blow.
func TestNotifyLineFitsCapWhenPreviewEscapesWide(t *testing.T) {
	body := strings.Repeat("<&>", 2000)
	r := Record{Ts: 1783922833.455, From: "a-very-long-agent-nickname", To: "@another-long-nickname", Text: body}
	out := notifyLine(encode(t, r), r)
	if len(out) > listenNotifyMaxBytes {
		t.Fatalf("escape-heavy notice is %d bytes, over the %d cap: %s", len(out), listenNotifyMaxBytes, out)
	}
	var n notice
	if err := json.Unmarshal(out, &n); err != nil {
		t.Fatalf("notice is not valid JSON: %v: %s", err, out)
	}
	if n.Full == "" {
		t.Error("notice must always carry the fetch command, even when the preview is squeezed out")
	}
}

func TestNotifyLineKeepsSharePathReachable(t *testing.T) {
	r := Record{
		Ts:   1783922833.455,
		From: "alice",
		To:   "@bob",
		Path: "/home/x/.agent-chat/artifacts/alice/1783922833455-abc123-report.md",
		Note: strings.Repeat("a long note about the attached report ", 20),
	}
	out := notifyLine(encode(t, r), r)
	var n notice
	if err := json.Unmarshal(out, &n); err != nil {
		t.Fatalf("notice is not valid JSON: %v: %s", err, out)
	}
	if n.Path != r.Path {
		t.Errorf("an oversized share must still deliver its artifact path, got %q", n.Path)
	}
}

// The notice carries an envelope it cannot shrink — two nicks, an artifact path,
// and the fetch command. If that alone approached the cap, the notice meant to
// survive delivery would itself be clipped. Worst case in real use, measured.
func TestNotifyLineFitsCapWithWorstCaseEnvelope(t *testing.T) {
	r := Record{
		Ts:   1783923255.197,
		From: "madrid-temperature-reconciliation",
		To:   "@infrastructure-upgrade-coordinator",
		Path: "/home/eljeffe/.agent-chat/artifacts/madrid-temperature-reconciliation/1783923255197-a1b2c3-incident-report-final.md",
		Note: strings.Repeat("a long explanatory note ", 100),
	}
	out := notifyLine(encode(t, r), r)
	if len(out) > listenNotifyMaxBytes {
		t.Fatalf("worst-case notice is %d bytes, over the %d cap: %s", len(out), listenNotifyMaxBytes, out)
	}
	var n notice
	if err := json.Unmarshal(out, &n); err != nil {
		t.Fatalf("notice is not valid JSON: %v", err)
	}
	if n.Path != r.Path || n.Full == "" {
		t.Errorf("envelope must survive the squeeze intact, got path=%q full=%q", n.Path, n.Full)
	}
}

func TestPreviewOfCollapsesWhitespaceAndCutsOnRuneBoundary(t *testing.T) {
	if got := previewOf("one\n\ntwo   three", 100); got != "one two three" {
		t.Errorf("previewOf = %q, want %q", got, "one two three")
	}
	got := previewOf(strings.Repeat("é", 50), 10)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated preview should be marked, got %q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("preview cut mid-rune: %q", got)
	}
}

// An envelope that alone fills most of the cap drives the shrink loop to its
// floor (budget<=0), where the preview is dropped rather than shaved. The
// lengths are tuned to land there: change them and this path goes untested.
func TestNotifyLineDropsPreviewEntirelyWhenEnvelopeAloneNearlyFillsCap(t *testing.T) {
	r := Record{
		Ts:   1783923255.197,
		From: strings.Repeat("n", 65),
		To:   "@" + strings.Repeat("m", 65),
		Path: "/home/x/.agent-chat/artifacts/" + strings.Repeat("p", 65) + "/file.md",
		Text: strings.Repeat("body ", 200),
	}
	out := notifyLine(encode(t, r), r)
	if len(out) > listenNotifyMaxBytes {
		t.Fatalf("notice is %d bytes, over the %d cap: %s", len(out), listenNotifyMaxBytes, out)
	}
	var n notice
	if err := json.Unmarshal(out, &n); err != nil {
		t.Fatalf("notice is not valid JSON: %v: %s", err, out)
	}
	if n.Preview != "" {
		t.Errorf("expected the preview to be squeezed out entirely, got %q", n.Preview)
	}
	if !n.Clipped || n.Path != r.Path || n.Full == "" {
		t.Errorf("envelope must still survive intact once preview is dropped: %+v", n)
	}
}

// A non-finite Ts (not ruled out by the type) makes every json.Marshal(notice)
// in the shrink loop fail whatever the preview size. notifyLine must then hand
// back the original line: worse delivery, but never a dropped message.
func TestNotifyLineFallsBackToOriginalLineWhenNoticeCannotBeMarshaled(t *testing.T) {
	r := Record{Ts: epochMs(math.NaN()), From: "alice", To: "@bob", Text: strings.Repeat("x", 1000)}
	// r.Ts is NaN, so even encode(t, r) would fail here (Record.MarshalJSON
	// hits the same non-finite float). Build the oversized line by hand instead
	// — its exact shape doesn't matter, only that it is over the cap.
	line := []byte(`{"ts":"NaN","from":"alice","to":"@bob","text":"` + strings.Repeat("x", 1000) + `"}`)
	out := notifyLine(line, r)
	if string(out) != string(line) {
		t.Errorf("unmarshalable notice should fall back to the original line unchanged, got %q", out)
	}
}

// A zero budget is the last rung of the shrink loop, so it must yield no
// preview at all. Returning the body here would hand back the very bytes the
// caller ran out of room for.
func TestPreviewOfWithNoBudgetIsEmpty(t *testing.T) {
	if got := previewOf("one two three four five", 0); got != "" {
		t.Errorf("previewOf with no budget = %q, want empty", got)
	}
}

// Nicks and paths are caller-controlled (`send --as` takes any string, bypassing
// the join path's clamp) and sit in fields the preview loop cannot shrink. If
// lengthening them pushes the notice over the cap, it is silently clipped.
func TestNotifyLineHoldsCapAgainstAbsurdNicksAndPaths(t *testing.T) {
	for _, nickLen := range []int{24, 65, 75, 200, 500} {
		r := Record{
			Ts:   1783923255.197,
			From: strings.Repeat("n", nickLen),
			To:   "@" + strings.Repeat("m", nickLen),
			Path: "/home/x/.agent-chat/artifacts/" + strings.Repeat("p", nickLen) + "/file.md",
			Text: strings.Repeat("body ", 500),
		}
		out := notifyLine(encode(t, r), r)
		if len(out) > listenNotifyMaxBytes {
			t.Errorf("nickLen %d: notice is %d bytes, over the %d cap — it will be clipped: %s",
				nickLen, len(out), listenNotifyMaxBytes, out)
			continue
		}
		var n notice
		if err := json.Unmarshal(out, &n); err != nil {
			t.Errorf("nickLen %d: notice is not valid JSON: %v", nickLen, err)
			continue
		}
		// Whatever was shed to make room, the way back to the whole message must
		// survive — it is the only thing the recipient cannot reconstruct.
		if !n.Clipped || n.Full == "" {
			t.Errorf("nickLen %d: notice lost its fetch command: %+v", nickLen, n)
		}
	}
}
