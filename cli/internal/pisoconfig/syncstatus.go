package pisoconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const syncStatusFile = "sync-status.json"

type syncResult struct {
	Pid         string    `json:"pid"`
	LastAttempt time.Time `json:"lastAttempt"`
	LastSuccess time.Time `json:"lastSuccess"`
	LastError   string    `json:"lastError,omitempty"`
}

// RecordSyncResult is written by the daemon, not one-shot CLI invocations.
// PID binds readiness to this instance; a stale status cannot bless a restart.
func RecordSyncResult(syncErr error) error {
	dir, err := DataDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, syncStatusFile)
	var result syncResult
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &result)
	}
	pid := strconv.Itoa(os.Getpid())
	if result.Pid != pid {
		result = syncResult{Pid: pid}
	}
	result.LastAttempt = time.Now()
	result.LastError = ""
	if syncErr != nil {
		result.LastError = syncErr.Error()
	} else {
		result.LastSuccess = result.LastAttempt
	}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return replaceFile(path, b, 0644)
}

func readSyncResult(dir string) syncResult {
	var result syncResult
	if b, err := os.ReadFile(filepath.Join(dir, syncStatusFile)); err == nil {
		_ = json.Unmarshal(b, &result)
	}
	return result
}

func syncResultReady(result syncResult, pid string, now time.Time) bool {
	age := now.Sub(result.LastSuccess)
	return result.Pid == pid && result.LastError == "" && !result.LastSuccess.IsZero() && age >= 0 && age < 10*time.Second
}
