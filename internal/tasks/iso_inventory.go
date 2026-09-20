package tasks

import (
	"context"
	"os"
	"path/filepath"

	"ovc-agent/internal/hostinfo"
)

// ISOInventoryResult holds the ISO inventory response. It marshals to the shape
// ovc-backend's apply_iso_inventory expects: { items: [{ id, name, path, sizeBytes }] }.
type ISOInventoryResult struct {
	Items []ISOItem `json:"items"`
}

type ISOItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	Checksum  string `json:"checksum,omitempty"`
}

// handleISOInventory scans the configured ISO directory (local_iso_path) and
// reports every .iso file. Pure Go filesystem walk - no PowerShell.
func (d *Dispatcher) handleISOInventory(_ context.Context) (*ISOInventoryResult, error) {
	isos, err := hostinfo.ScanISOs(d.localISOPath)
	if err != nil {
		return nil, err
	}

	items := make([]ISOItem, 0, len(isos))
	for _, iso := range isos {
		var size int64
		if fi, statErr := os.Stat(iso.Path); statErr == nil {
			size = fi.Size()
		}
		items = append(items, ISOItem{
			ID:        iso.Checksum, // stable per file content; falls back to "" if unhashable
			Name:      filepath.Base(iso.Path),
			Path:      iso.Path,
			SizeBytes: size,
			Checksum:  iso.Checksum,
		})
	}

	return &ISOInventoryResult{Items: items}, nil
}
