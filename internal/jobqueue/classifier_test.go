package jobqueue

import (
	"testing"

	"ovc-agent/internal/jobstore"

	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		taskType string
		action   string
		want     JobClass
	}{
		// Read jobs
		{"vm_inventory is Read", "vm_inventory", "", ClassRead},
		{"hardware_inventory is Read", "hardware_inventory", "", ClassRead},
		{"vm_status is Read", "vm_status", "", ClassRead},
		{"template_inventory is Read", "template_inventory", "", ClassRead},
		{"iso_inventory is Read", "iso_inventory", "", ClassRead},
		{"agent_status is Read", "agent_status", "", ClassRead},

		// host_management Read variants
		{"host_management refresh_hardware is Read", "host_management", "refresh_hardware", ClassRead},
		{"host_management refresh_inventory is Read", "host_management", "refresh_inventory", ClassRead},

		// host_management HostWrite variants
		{"host_management suspend is HostWrite", "host_management", "suspend", ClassHostWrite},
		{"host_management suspend_drain is HostWrite", "host_management", "suspend_drain", ClassHostWrite},
		{"host_management resume is HostWrite", "host_management", "resume", ClassHostWrite},
		{"host_management resume_fallback is HostWrite", "host_management", "resume_fallback", ClassHostWrite},
		{"host_management restart is HostWrite", "host_management", "restart", ClassHostWrite},

		// Download
		{"artifact_download is Download", "artifact_download", "", ClassDownload},

		// Write jobs (default case)
		{"vm_create is Write", "vm_create", "", ClassWrite},
		{"vm_clone is Write", "vm_clone", "", ClassWrite},
		{"vm_edit is Write", "vm_edit", "", ClassWrite},
		{"vm_management is Write", "vm_management", "", ClassWrite},
		{"vm_move is Write", "vm_move", "", ClassWrite},
		{"vm_rename is Write", "vm_rename", "", ClassWrite},
		{"vm_export_template is Write", "vm_export_template", "", ClassWrite},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &jobstore.JobRecord{
				TaskType: tt.taskType,
				Action:   tt.action,
			}
			got := Classify(job)
			assert.Equal(t, tt.want, got)
		})
	}
}
