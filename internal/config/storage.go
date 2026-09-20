package config

import (
	"path/filepath"
	"strings"
)

// hyperVLeaf is the folder suffix every VM storage path is normalized to end with.
const hyperVLeaf = "HyperV"

// normalizeStoragePath cleans a raw storage path and ensures it ends with the
// "HyperV" leaf folder. Empty input returns an empty string.
//
// Examples:
//
//	"E:\\HyperV"          -> "E:\\HyperV"
//	"E:\\"                -> "E:\\HyperV"
//	"H:\\VMs"             -> "H:\\VMs\\HyperV"
//	"H:\\VMs\\hyperv"     -> "H:\\VMs\\hyperv" (case-insensitive match, kept as-is)
func normalizeStoragePath(raw string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return ""
	}

	p = filepath.Clean(p)

	// Already ends with the HyperV leaf (case-insensitive)?
	if strings.EqualFold(filepath.Base(p), hyperVLeaf) {
		return p
	}

	return filepath.Join(p, hyperVLeaf)
}

// storageVolume returns the volume/drive identifier for a path, upper-cased for
// case-insensitive comparison (e.g. "E:" for "E:\\VM\\HyperV").
func storageVolume(p string) string {
	return strings.ToUpper(filepath.VolumeName(p))
}

// parseAdditionalStorage parses the raw "aditional_vm_storage" INI value into a
// normalized, de-duplicated list of storage paths.
//
// Rules applied:
//   - Split on ";" and trim blanks.
//   - Each path is normalized to end with the "HyperV" leaf folder.
//   - Only the first path per volume/drive is kept (e.g. "E:\\HyperV" and
//     "E:\\VM\\TESTE\\HyperV" collapse to the first one seen).
func parseAdditionalStorage(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	var result []string
	seenVolumes := make(map[string]bool)

	for _, part := range strings.Split(raw, ";") {
		norm := normalizeStoragePath(part)
		if norm == "" {
			continue
		}

		vol := storageVolume(norm)
		if vol != "" && seenVolumes[vol] {
			continue // duplicate drive, keep only the first
		}
		if vol != "" {
			seenVolumes[vol] = true
		}
		result = append(result, norm)
	}

	return result
}
