package main

import (
	"reflect"
	"testing"
)

func TestRestoreBackupArgumentRemovesOnlyRestoreFlag(t *testing.T) {
	backup, remaining := restoreBackupArgument([]string{"--config", "/tmp/component.yaml", "--backup", "42"})
	if backup != "42" || !reflect.DeepEqual(remaining, []string{"--config", "/tmp/component.yaml"}) {
		t.Fatalf("backup=%q remaining=%q", backup, remaining)
	}
	backup, remaining = restoreBackupArgument([]string{"--backup=43", "--config", "/tmp/component.yaml"})
	if backup != "43" || !reflect.DeepEqual(remaining, []string{"--config", "/tmp/component.yaml"}) {
		t.Fatalf("backup=%q remaining=%q", backup, remaining)
	}
}

func TestRestoreBackupArgumentRejectsMissingValue(t *testing.T) {
	backup, remaining := restoreBackupArgument([]string{"--config", "/tmp/component.yaml", "--backup"})
	if backup != "" || remaining != nil {
		t.Fatalf("backup=%q remaining=%q", backup, remaining)
	}
}

func TestTrimTerminalPasswordRemovesOnlyEnterSequence(t *testing.T) {
	if got := trimTerminalPassword([]byte("  secret value  \r\n")); got != "  secret value  " {
		t.Fatalf("trimTerminalPassword=%q", got)
	}
}
