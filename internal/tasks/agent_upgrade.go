package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ovc-agent/internal/download"
	"ovc-agent/internal/upgrade"
)

// AgentUpgradePayload is the params for the host_update_agent function. The
// backend hands the agent a URL + checksum + target version; the agent decides
// locally where the binary goes.
type AgentUpgradePayload struct {
	DownloadURL    string `json:"download_url"`
	ChecksumSHA256 string `json:"checksum_sha256"`
	Version        string `json:"version"`
}

// handleAgentUpgrade downloads the new agent binary, verifies its checksum, and
// stages it for the drain-mode swap performed by the service process. It runs in
// a goroutine and publishes its own progress + terminal responses via d.onProgress.
//
// Wire status stays "in_progress" (running) even once the download is finished:
// the upgrade has NOT been applied yet. The InternalStatus "completed" lets the
// worker mark the job done so the service enters drain mode. The terminal
// "succeeded" (upgraded: true) is published by the NEW agent after it restarts;
// a "failed" is published by the upgrade process on rollback.
func (d *Dispatcher) handleAgentUpgrade(ctx context.Context, taskID string, payload json.RawMessage) {
	defer func() {
		if r := recover(); r != nil {
			d.publishUpgradeFailure(ctx, taskID, fmt.Sprintf("internal panic in agent upgrade: %v", r))
		}
	}()

	var p AgentUpgradePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		d.publishUpgradeFailure(ctx, taskID, fmt.Sprintf("invalid payload: %v", err))
		return
	}
	if p.DownloadURL == "" || p.Version == "" {
		d.publishUpgradeFailure(ctx, taskID, "download_url and version are required")
		return
	}
	if p.Version == AgentVersion {
		d.publishUpgradeFailure(ctx, taskID, fmt.Sprintf("agent is already at version %s", AgentVersion))
		return
	}

	agentDir := upgrade.GetAgentDir()
	if agentDir == "" {
		d.publishUpgradeFailure(ctx, taskID, "failed to determine agent installation directory")
		return
	}
	finalPath := filepath.Join(agentDir, fmt.Sprintf("ovc-agent-%s.exe", p.Version))
	tmpPath := filepath.Join(agentDir, fmt.Sprintf("ovc-agent-%s.exe.%s.tmp", p.Version, taskID))

	var completed bool
	defer func() {
		if !completed {
			if _, err := os.Stat(tmpPath); err == nil {
				_ = os.Remove(tmpPath)
			}
		}
	}()

	d.publishUpgradeProgress(ctx, taskID, 0, "Starting agent download...")

	// Disk space check (best effort).
	if size, err := download.GetContentLength(p.DownloadURL); err == nil && size > 0 {
		if avail, derr := download.CheckDiskSpace(agentDir); derr == nil && avail < uint64(size) {
			d.publishUpgradeFailure(ctx, taskID, fmt.Sprintf(
				"insufficient disk space for agent binary: need %d bytes, have %d", size, avail))
			return
		}
	}

	progressCb := func(done, total int64) {
		pct := 0
		if total > 0 {
			pct = int(done * 90 / total)
		}
		d.publishUpgradeProgress(ctx, taskID, pct, "Downloading agent...")
	}
	if err := download.DownloadFile(ctx, p.DownloadURL, tmpPath, -1, progressCb); err != nil {
		d.publishUpgradeFailure(ctx, taskID, fmt.Sprintf("download failed: %v", err))
		return
	}

	fi, err := os.Stat(tmpPath)
	if err != nil {
		d.publishUpgradeFailure(ctx, taskID, fmt.Sprintf("failed to stat downloaded file: %v", err))
		return
	}

	if p.ChecksumSHA256 != "" {
		d.publishUpgradeProgress(ctx, taskID, 92, "Verifying checksum...")
		match, got, verr := download.VerifyChecksum(tmpPath, p.ChecksumSHA256)
		if verr != nil {
			d.publishUpgradeFailure(ctx, taskID, fmt.Sprintf("checksum verification error: %v", verr))
			return
		}
		if !match {
			d.publishUpgradeFailure(ctx, taskID, fmt.Sprintf(
				"checksum mismatch: expected %s, got %s", p.ChecksumSHA256, got))
			return
		}
	}

	d.publishUpgradeProgress(ctx, taskID, 95, "Staging new binary...")
	if err := download.PlaceFileWithOverwrite(tmpPath, finalPath, true); err != nil {
		d.publishUpgradeFailure(ctx, taskID, fmt.Sprintf("failed to stage binary: %v", err))
		return
	}
	completed = true

	// Record what the NEW agent must confirm after it restarts.
	if err := upgrade.WritePendingResponse(agentDir, &upgrade.PendingUpgradeResponse{
		TaskID:     taskID,
		Version:    p.Version,
		SizeBytes:  fi.Size(),
		OldVersion: AgentVersion,
	}); err != nil {
		d.logger.Error("agent upgrade: failed to write pending response file", "task_id", taskID, "error", err)
	}

	// Report the download phase done. Wire status stays "running"; InternalStatus
	// "completed" drives the service into drain mode.
	if d.onProgress != nil {
		pct := 99
		d.onProgress(ctx, &TaskResponse{
			TaskID:   taskID,
			HostID:   d.hostID,
			Type:     TaskAgentUpgrade,
			Function: "host_update_agent",
			Status:   "in_progress",
			Progress: &Progress{Percent: pct, Message: "Agent binary downloaded, applying upgrade..."},
			Result: map[string]interface{}{
				"version":         p.Version,
				"old_version":     AgentVersion,
				"size_bytes":      fi.Size(),
				"upgraded":        false,
				"upgrade_pending": true,
			},
			InternalStatus: "completed",
			FinishedAt:     time.Now().UTC().Format(time.RFC3339),
		})
	}

	d.logger.Info("agent upgrade: binary staged, drain mode will apply it",
		"task_id", taskID, "version", p.Version, "binary", finalPath)
}

func (d *Dispatcher) publishUpgradeProgress(ctx context.Context, taskID string, percent int, message string) {
	if d.onProgress == nil {
		return
	}
	d.onProgress(ctx, &TaskResponse{
		TaskID:   taskID,
		HostID:   d.hostID,
		Type:     TaskAgentUpgrade,
		Function: "host_update_agent",
		Status:   "in_progress",
		Progress: &Progress{Percent: percent, Message: message},
	})
}

func (d *Dispatcher) publishUpgradeFailure(ctx context.Context, taskID, errMsg string) {
	d.logger.Error("agent upgrade failed", "task_id", taskID, "error", errMsg)
	if d.onProgress == nil {
		return
	}
	e := errMsg
	d.onProgress(ctx, &TaskResponse{
		TaskID:     taskID,
		HostID:     d.hostID,
		Type:       TaskAgentUpgrade,
		Function:   "host_update_agent",
		Status:     "failed",
		Error:      &e,
		FinishedAt: time.Now().UTC().Format(time.RFC3339),
	})
}
