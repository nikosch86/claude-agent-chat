package main

import (
	"strings"
	"testing"
)

const (
	lineJoinedAlice = `{"ts":1000.000,"from":"alice","event":"joined"}`
	lineJoinedBob   = `{"ts":1001.000,"from":"bob","event":"joined"}`
	lineQuitAlice   = `{"ts":1002.000,"from":"alice","event":"quit"}`
	lineRejoinAlice = `{"ts":1003.000,"from":"alice","event":"joined"}`
)

func TestPeersListsJoined(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, lineJoinedAlice, lineJoinedBob)
	out, rc := captureStdout(t, func() int { return run([]string{"peers"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	got := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Errorf("got %v, want [alice bob]", got)
	}
}

func TestPeersQuitRemoves(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, lineJoinedAlice, lineJoinedBob, lineQuitAlice)
	out, rc := captureStdout(t, func() int { return run([]string{"peers"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	got := strings.TrimRight(out, "\n")
	if got != "bob" {
		t.Errorf("got %q, want %q", got, "bob")
	}
}

func TestPeersRejoinAfterQuit(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, lineJoinedAlice, lineJoinedBob, lineQuitAlice, lineRejoinAlice)
	out, rc := captureStdout(t, func() int { return run([]string{"peers"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	got := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Errorf("got %v, want [alice bob]", got)
	}
}

func TestPeersOutputSorted(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home,
		`{"ts":1000.000,"from":"zoe","event":"joined"}`,
		`{"ts":1001.000,"from":"alice","event":"joined"}`,
		`{"ts":1002.000,"from":"marvin","event":"joined"}`,
	)
	out, rc := captureStdout(t, func() int { return run([]string{"peers"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	got := strings.Split(strings.TrimRight(out, "\n"), "\n")
	want := []string{"alice", "marvin", "zoe"}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestPeersEmptyLogIsEmptyOutput(t *testing.T) {
	withTempHome(t)
	out, rc := captureStdout(t, func() int { return run([]string{"peers"}) })
	if rc != 0 {
		t.Fatalf("rc = %d, want 0 for missing log", rc)
	}
	if out != "" {
		t.Errorf("want empty output, got %q", out)
	}
}

// Non-event traffic (plain send lines) must not appear as peers.
func TestPeersIgnoresNonEventLines(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, lineAliceBob, lineBroadcast, lineJoinedAlice)
	out, rc := captureStdout(t, func() int { return run([]string{"peers"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	got := strings.TrimRight(out, "\n")
	if got != "alice" {
		t.Errorf("got %q, want %q (carol/bob have no joined event)", got, "alice")
	}
}
