package main

import (
	"encoding/json"
	"os"
	"syscall"
)

// witnessObservation is the machine record one witness run produces.
// Errno 0 with Success true is the clean path; any non-zero errno is the
// fault the storage-fault cells assert on.
type witnessObservation struct {
	Operation string `json:"operation"`
	Path      string `json:"path"`
	Errno     int    `json:"errno"`
	Success   bool   `json:"success"`
	ErrorText string `json:"errorText,omitempty"`
}

// witness performs one operation and returns the raw observation. Each
// operation uses the same syscall sequence a persistence layer would:
// create/write for writes, write+fsync for fsync, create+rename for
// rename — so the observed errno is the one the product path would see.
func witness(operation, path string) witnessObservation {
	observation := witnessObservation{Operation: operation, Path: path}
	switch operation {
	case "write":
		errno, text := witnessWrite(path)
		observation.Errno, observation.ErrorText, observation.Success = errno, text, errno == 0
	case "fsync":
		errno, text := witnessFsync(path)
		observation.Errno, observation.ErrorText, observation.Success = errno, text, errno == 0
	case "rename":
		errno, text := witnessRename(path)
		observation.Errno, observation.ErrorText, observation.Success = errno, text, errno == 0
	}
	return observation
}

func witnessWrite(path string) (int, string) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return errnoOf(err), err.Error()
	}
	defer file.Close()
	if _, err := file.WriteString("quoin-faultfs witness payload"); err != nil {
		return errnoOf(err), err.Error()
	}
	return 0, ""
}

func witnessFsync(path string) (int, string) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return errnoOf(err), err.Error()
	}
	defer file.Close()
	if _, err := file.WriteString("quoin-faultfs witness payload"); err != nil {
		return errnoOf(err), err.Error()
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_FSYNC, file.Fd(), 0, 0); errno != 0 {
		return int(errno), errno.Error()
	}
	return 0, ""
}

func witnessRename(path string) (int, string) {
	source := path + ".witness-source"
	if err := os.WriteFile(source, []byte("quoin-faultfs witness payload"), 0o644); err != nil {
		return errnoOf(err), err.Error()
	}
	if err := os.Rename(source, path); err != nil {
		return errnoOf(err), err.Error()
	}
	return 0, ""
}

// errnoOf extracts the syscall errno of an *os.PathError or LinkError.
func errnoOf(err error) int {
	switch typed := err.(type) {
	case nil:
		return 0
	case *os.PathError:
		if errno, ok := typed.Err.(syscall.Errno); ok {
			return int(errno)
		}
	case *os.LinkError:
		if errno, ok := typed.Err.(syscall.Errno); ok {
			return int(errno)
		}
	case syscall.Errno:
		return int(typed)
	}
	if errno, ok := err.(syscall.Errno); ok {
		return int(errno)
	}
	return -1
}

func jsonMarshalIndent(value any) ([]byte, error) {
	return json.MarshalIndent(value, "", "  ")
}
