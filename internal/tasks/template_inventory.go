package tasks

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"ovc-agent/internal/hyperv"
)

const templateMetadataFile = "ovc-template-metadata.json"

// templateSizePSFunc defines Get-OvcTemplateSize, a PowerShell helper that computes
// a template's provisioned disk size (sum of each base disk's virtual/max size) by
// scanning the exported folder directly. It deliberately does NOT rely on
// Compare-VM's planned HardDrives[].Path - for `Compare-VM -Copy` those point at the
// import *destination* (the host's default VHD store), which does not exist yet, so
// `Get-VHD` on them fails and the size comes back as 0. Scanning the export folder's
// "Virtual Hard Disks" directory for .vhd/.vhdx files (checkpoint .avhdx excluded -
// the virtual size is constant across a differencing chain) always hits real files.
const templateSizePSFunc = `
function Get-OvcTemplateSize {
    param([string]$TemplateFolder)
    $total = 0
    $vhdDir = Join-Path $TemplateFolder 'Virtual Hard Disks'
    $scanRoot = if (Test-Path -LiteralPath $vhdDir) { $vhdDir } else { $TemplateFolder }
    $disks = Get-ChildItem -LiteralPath $scanRoot -Recurse -File -ErrorAction SilentlyContinue |
        Where-Object { $_.Extension -eq '.vhd' -or $_.Extension -eq '.vhdx' }
    foreach ($d in $disks) {
        try { $total += [int64](Get-VHD -Path $d.FullName -ErrorAction Stop).Size } catch {}
    }
    return $total
}
`

// TemplateInventoryResult holds the template inventory response
type TemplateInventoryResult struct {
	Templates []TemplateInfo `json:"templates"`
}

type TemplateInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	CPUCount int    `json:"cpuCount"`
	MemoryMB int64  `json:"memoryMb"`
	Notes    string `json:"notes"`
	// SizeBytes is the total PROVISIONED size of the template's disks (sum of
	// Get-VHD .Size - the space a deployed VM occupies once its disks are made
	// Fixed). Read from the metadata JSON's "size" field when present; otherwise
	// computed on the fly with Get-VHD during inventory.
	SizeBytes int64 `json:"sizeBytes"`
	// DiskSizeBytes is the REAL on-disk footprint of the exported template folder
	// (sum of every file's length - Dynamic VHDX report their current size, not
	// the virtual max). Measured natively in Go, not PowerShell.
	DiskSizeBytes int64 `json:"diskSizeBytes"`
	// CreatedAt is the export timestamp, read from the metadata JSON's
	// "exported_at" field (RFC3339). Empty for legacy/hand-placed templates.
	CreatedAt string `json:"createdAt,omitempty"`
}

// dirSize returns the total size, in bytes, of every regular file under root.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// templateDiskSize returns the real on-disk footprint of a template folder,
// logging and returning 0 on error so a single unreadable template does not
// break the whole inventory.
func (d *Dispatcher) templateDiskSize(folder string) int64 {
	size, err := dirSize(folder)
	if err != nil {
		d.logger.Warn("template_inventory: failed to measure on-disk size",
			"path", folder, "error", err)
		return 0
	}
	return size
}

// TemplateMetadata is the content of ovc-template-metadata.json written by vm_export_template.
// It provides a stable, unique ID for each exported template, and optionally the display name and notes.
type TemplateMetadata struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Notes      string `json:"notes,omitempty"`
	SourceVM   string `json:"source_vm"`
	ExportedAt string `json:"exported_at"`
	ExportedBy string `json:"exported_by"`
	// Size is the provisioned disk size (bytes, sum of Get-VHD .Size) captured at
	// export time so the template inventory does not have to re-run Get-VHD.
	Size int64 `json:"size,omitempty"`
}

// generateUUID creates a random UUID v4 string.
func generateUUID() string {
	var uuid [16]byte
	rand.Read(uuid[:])
	uuid[6] = (uuid[6] & 0x0f) | 0x40 // version 4
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16])
}

// readTemplateMetadata attempts to read ovc-template-metadata.json from the template's
// Virtual Machines folder. Returns nil if not found.
func readTemplateMetadata(templateFolder string) *TemplateMetadata {
	metaPath := filepath.Join(templateFolder, "Virtual Machines", templateMetadataFile)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil
	}
	var meta TemplateMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil
	}
	if meta.ID == "" {
		return nil
	}
	return &meta
}

func (d *Dispatcher) handleTemplateInventory(ctx context.Context) (*TemplateInventoryResult, error) {
	if d.templatePath == "" {
		return &TemplateInventoryResult{Templates: []TemplateInfo{}}, nil
	}

	// Strategy:
	// 1. For each template folder, check for ovc-template-metadata.json - if present, use its ID, name, notes, and size
	// 2. If metadata not present (legacy template), fallback to .vmcx filename as ID
	// 3. Use Compare-VM -Copy -GenerateNewID to read hardware metadata (cpu, memory) and
	//    fallback name/notes from the vmcx when metadata fields are absent
	// 4. Provisioned size: use the metadata "size" field when present, else compute
	//    it by scanning the exported folder's disks (Get-OvcTemplateSize)
	script := fmt.Sprintf(templateSizePSFunc+`
$templateRoot = "%s"
$templates = @()

if (-not (Test-Path $templateRoot)) {
    $templates | ConvertTo-Json -Depth 5 -Compress
    return
}

# Find all .vmcx files in subdirectories
$vmcxFiles = Get-ChildItem -Path $templateRoot -Filter *.vmcx -Recurse -ErrorAction SilentlyContinue

foreach ($vmcx in $vmcxFiles) {
    try {
        # Get the template folder (parent of "Virtual Machines" folder)
        $templateFolder = (Split-Path (Split-Path $vmcx.FullName -Parent) -Parent)

        # Check for ovc-template-metadata.json (stable ID, name, notes, size from export)
        $metaFile = Join-Path (Split-Path $vmcx.FullName -Parent) "ovc-template-metadata.json"
        $stableId = $null
        $metaName = $null
        $metaNotes = $null
        $metaSize = $null
        $metaExportedAt = $null
        if (Test-Path $metaFile) {
            try {
                $meta = Get-Content $metaFile -Raw | ConvertFrom-Json
                if ($meta.id) { $stableId = $meta.id }
                if ($meta.name) { $metaName = $meta.name }
                if ($meta.PSObject.Properties['notes'] -and $meta.notes -ne $null) { $metaNotes = [string]$meta.notes }
                if ($meta.PSObject.Properties['size'] -and $meta.size) { $metaSize = [int64]$meta.size }
                if ($meta.PSObject.Properties['exported_at'] -and $meta.exported_at) { $metaExportedAt = [string]$meta.exported_at }
            } catch {}
        }

        # Fallback ID: use .vmcx filename (original VM GUID)
        if (-not $stableId) {
            $stableId = [System.IO.Path]::GetFileNameWithoutExtension($vmcx.Name)
        }

        # Use -GenerateNewID to avoid conflicts with VMs already registered in the cluster
        $report = Compare-VM -Copy -GenerateNewId -Path $vmcx.FullName -ErrorAction Stop
        $vm = $report.VM

        # Name: metadata > vmcx
        $templateName = if ($metaName) { $metaName } else { $vm.Name }
        # Notes: metadata > vmcx
        $templateNotes = if ($metaNotes -ne $null) { $metaNotes } else { [string]$vm.Notes }

        # Provisioned size: metadata "size" when present, else scan the exported
        # folder's disks (Compare-VM's HardDrives paths are unreliable - see helper).
        $sizeBytes = 0
        if ($metaSize) {
            $sizeBytes = $metaSize
        } else {
            $sizeBytes = Get-OvcTemplateSize -TemplateFolder $templateFolder
        }

        $templates += [pscustomobject]@{
            id        = $stableId
            name      = $templateName
            path      = $templateFolder
            cpuCount  = $vm.ProcessorCount
            memoryMb  = [math]::Round($vm.MemoryStartup / 1MB)
            notes     = $templateNotes
            sizeBytes = $sizeBytes
            createdAt = $metaExportedAt
        }

        # Clean up the planned VM to avoid leftover state
        try { Remove-VM -VM $vm -Force -ErrorAction SilentlyContinue } catch {}
    } catch {
        # Skip templates that fail to parse
    }
}

$templates | ConvertTo-Json -Depth 5 -Compress
`, d.templatePath)

	output, err := hyperv.RunPowerShell(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("failed to get template inventory: %v", err)
	}

	output = trimPowerShellOutput(output)

	if len(output) == 0 {
		return &TemplateInventoryResult{Templates: []TemplateInfo{}}, nil
	}

	// Try array first
	var templates []TemplateInfo
	if err := json.Unmarshal(output, &templates); err != nil {
		// Single template (PowerShell returns object instead of array)
		var single TemplateInfo
		if err2 := json.Unmarshal(output, &single); err2 != nil {
			return nil, fmt.Errorf("failed to parse template inventory JSON: %v (raw: %s)", err, string(output))
		}
		templates = []TemplateInfo{single}
	}

	// Enrich each template with its real on-disk footprint (native Go - the
	// PowerShell block only knows the provisioned/virtual size).
	for i := range templates {
		if templates[i].Path != "" {
			templates[i].DiskSizeBytes = d.templateDiskSize(templates[i].Path)
		}
	}

	return &TemplateInventoryResult{Templates: templates}, nil
}
