package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
)

func runPeers(args []string) int {
	fs := flag.NewFlagSet("peers", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	nicks, err := activePeers()
	if err != nil {
		fmt.Fprintf(os.Stderr, "peers: %v\n", err)
		return 1
	}
	bw := bufio.NewWriter(os.Stdout)
	for _, n := range nicks {
		fmt.Fprintln(bw, n)
	}
	bw.Flush()
	if n, err := resolveNick(""); err == nil {
		maybeWarnListener(os.Stderr, n)
	}
	return 0
}

// activePeers scans log.jsonl and returns the sorted set of nicks with a
// joined event and no subsequent quit. A missing log is not an error.
func activePeers() ([]string, error) {
	f, err := os.Open(logPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	active := map[string]struct{}{}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		var r Record
		if err := json.Unmarshal(s.Bytes(), &r); err != nil {
			continue
		}
		switch r.Event {
		case "joined":
			if r.From != "" {
				active[r.From] = struct{}{}
			}
		case "quit":
			if r.From != "" {
				delete(active, r.From)
			}
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}

	nicks := make([]string, 0, len(active))
	for n := range active {
		nicks = append(nicks, n)
	}
	sort.Strings(nicks)
	return nicks, nil
}
