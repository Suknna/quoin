// Package report owns the deployment helper's verification-report.json: the
// generic command envelope written atomically on every path (OPS-HELPER-003).
// It records stages, executed commands with raw exit codes, structured
// checks and the next retryable action; secrets and attached-stdin content
// never enter the report.
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

const schemaVersion = 1

type Command struct {
	Argv     []string `json:"argv"`
	ExitCode int      `json:"exitCode"`
	Duration string   `json:"duration"`
	LogPath  string   `json:"logPath,omitempty"`
}

type Stage struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Detail     string    `json:"detail,omitempty"`
	StartedAt  string    `json:"startedAt"`
	FinishedAt string    `json:"finishedAt"`
	Commands   []Command `json:"commands,omitempty"`
}

type Check struct {
	ID       string `json:"id"`
	Result   string `json:"result"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
	Code     string `json:"code,omitempty"`
	Recovery string `json:"recovery,omitempty"`
}

type Failure struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	NextAction string `json:"nextAction"`
}

type Report struct {
	SchemaVersion         int      `json:"schemaVersion"`
	Backend               string   `json:"backend"`
	Architecture          string   `json:"architecture"`
	Command               string   `json:"command"`
	Release               string   `json:"release"`
	ImageMode             string   `json:"imageMode,omitempty"`
	SourceCommit          string   `json:"sourceCommit"`
	ReleaseManifestDigest string   `json:"releaseManifestDigest,omitempty"`
	ConfigPath            string   `json:"configPath"`
	ConfigDigest          string   `json:"configDigest"`
	StartedAt             string   `json:"startedAt"`
	FinishedAt            string   `json:"finishedAt"`
	Stages                []Stage  `json:"stages"`
	Checks                []Check  `json:"checks"`
	ExitCode              int      `json:"exitCode"`
	Failure               *Failure `json:"failure,omitempty"`
}

func New(backend, architecture, command, configPath, configDigest string) *Report {
	return &Report{
		SchemaVersion: schemaVersion, Backend: backend, Architecture: architecture, Command: command,
		ConfigPath: configPath, ConfigDigest: configDigest, SourceCommit: sourceCommit(),
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// sourceCommit reads the VCS revision the helper binary was built from
// (Go buildvcs); released helpers keep their stamped commit, local test
// builds may report none.
func sourceCommit() string {
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range build.Settings {
			if setting.Key == "vcs.revision" && strings.TrimSpace(setting.Value) != "" {
				return setting.Value
			}
		}
	}
	return ""
}

// BeginStage appends a running stage and returns its index for Complete/fail.
func (report *Report) BeginStage(name string) int {
	report.Stages = append(report.Stages, Stage{Name: name, Status: "running", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	return len(report.Stages) - 1
}

func (report *Report) CompleteStage(index int, detail string) {
	report.Stages[index].Status = "completed"
	report.Stages[index].Detail = detail
	report.Stages[index].FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
}

func (report *Report) FailStage(index int, detail string) {
	report.Stages[index].Status = "failed"
	report.Stages[index].Detail = detail
	report.Stages[index].FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
}

func (report *Report) RecordCommand(index int, command Command) {
	report.Stages[index].Commands = append(report.Stages[index].Commands, command)
}

func (report *Report) RecordCheck(check Check) {
	report.Checks = append(report.Checks, check)
}

func (report *Report) MarkFailed(code, message, nextAction string) {
	report.ExitCode = 1
	report.Failure = &Failure{Code: code, Message: message, NextAction: nextAction}
}

func (report *Report) MarkSucceeded() {
	report.ExitCode = 0
}

// Finish stamps the end time and writes the report atomically (tmp file,
// fsync, rename, parent fsync) so a concurrent reader never sees a partial
// document.
func (report *Report) Finish(path string) error {
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	remove = false
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open report directory for fsync: %w", err)
	}
	defer directory.Close()
	return directory.Sync()
}
