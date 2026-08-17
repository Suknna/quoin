package ops

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
)

// logMu keeps concurrent JSON Lines writes whole even with parallel writers.
var logMu sync.Mutex

type logLine struct {
	Time      string `json:"ts"`
	Level     string `json:"level"`
	Component string `json:"component"`
	Release   string `json:"release"`
	Code      string `json:"code"`
	Message   string `json:"msg"`
}

// LogEvent emits the frozen OPS-LOG-001 JSON Lines shape to stdout/stderr.
// Only whitelisted scalar values may reach message; secrets are never logged.
func LogEvent(component, level, code, message string) {
	line := logLine{
		Time: time.Now().UTC().Format(time.RFC3339Nano), Level: level,
		Component: component, Release: buildinfo.Release, Code: code, Message: message,
	}
	// Marshal of this string-only struct cannot fail in practice; dropping
	// an unencodable line is preferable to panicking inside the log path.
	encoded, err := json.Marshal(line)
	if err != nil {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	if level == "error" {
		os.Stderr.Write(append(encoded, '\n'))
	} else {
		os.Stdout.Write(append(encoded, '\n'))
	}
}
