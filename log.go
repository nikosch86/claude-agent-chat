package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type epochMs float64

func (e epochMs) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatFloat(float64(e), 'f', 3, 64)), nil
}

// Record is the JSONL wire schema. Field order here determines field order
// in the encoded line — keep it aligned with the design doc's examples.
type Record struct {
	Ts    epochMs `json:"ts"`
	From  string  `json:"from"`
	To    string  `json:"to,omitempty"`
	Text  string  `json:"text,omitempty"`
	Path  string  `json:"path,omitempty"`
	Event string  `json:"event,omitempty"`
	Note  string  `json:"note,omitempty"`
}

func chatHome() string {
	if v := os.Getenv("AGENT_CHAT_HOME"); v != "" {
		return v
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".agent-chat"
	}
	return filepath.Join(h, ".agent-chat")
}

func logPath() string {
	return filepath.Join(chatHome(), "log.jsonl")
}

func appendRecord(r Record) error {
	p := logPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

func nowEpochMs() epochMs {
	return epochMs(float64(time.Now().UnixMilli()) / 1000.0)
}
