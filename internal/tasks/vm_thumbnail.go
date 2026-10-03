package tasks

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"strings"
	"time"

	"ovc-agent/internal/hyperv"
)

// Console thumbnail settings. Hyper-V renders the frame at the requested size, so
// no resampling happens on the agent side.
const (
	thumbnailWidth   = 320
	thumbnailHeight  = 240
	thumbnailQuality = 70
	thumbnailTimeout = 2 * time.Minute

	// thumbnailHeaderBytes is the fixed-size header that precedes the RGB565
	// pixel data in the ImageData buffer (observed on every VM: buffer length is
	// always width*height*2 + 4). It must be skipped, otherwise every row shifts
	// by two pixels and the right edge wraps into the leftmost columns.
	thumbnailHeaderBytes = 4
)

// vmThumbnailRaw is one entry of the thumbnail PowerShell block output: the VM id
// and the raw RGB565 frame returned by GetVirtualSystemThumbnailImage, base64 encoded.
type vmThumbnailRaw struct {
	ID   string `json:"id"`
	Data string `json:"data"`
}

// buildThumbnailScript returns the PowerShell block that grabs the console frame of
// every Running VM. It only fetches the raw bytes; the JPEG encoding is done in Go.
//
// The settings object passed as TargetSystem is the *realized* settings of the VM
// (VirtualSystemType = Microsoft:Hyper-V:System:Realized), keyed by the VM GUID.
// Matching by ElementName alone could pick a checkpoint's settings instead.
// Failures are per VM: a VM that cannot be captured is simply left out.
func buildThumbnailScript(width, height int) string {
	return fmt.Sprintf(`
try {
    $running = @(Get-VM -ErrorAction Stop | Where-Object { $_.State -eq "Running" })
    if ($running.Count -eq 0) {
        Write-Output "[]"
        return
    }

    $ns  = "root/virtualization/v2"
    $svc = Get-CimInstance -Namespace $ns -ClassName Msvm_VirtualSystemManagementService -ErrorAction Stop

    $settings = @{}
    $allSettings = @(Get-CimInstance -Namespace $ns -ClassName Msvm_VirtualSystemSettingData -Filter "VirtualSystemType='Microsoft:Hyper-V:System:Realized'" -ErrorAction Stop)
    foreach ($s in $allSettings) {
        if ($s.VirtualSystemIdentifier) { $settings["$($s.VirtualSystemIdentifier)".ToLower()] = $s }
    }

    $results = [System.Collections.ArrayList]::new()
    foreach ($v in $running) {
        try {
            $key = "$($v.Id)".ToLower()
            if (-not $settings.ContainsKey($key)) { continue }

            $r = Invoke-CimMethod -InputObject $svc -MethodName GetVirtualSystemThumbnailImage -ErrorAction Stop -Arguments @{
                TargetSystem = $settings[$key]
                WidthPixels  = [uint16]%d
                HeightPixels = [uint16]%d
            }
            if ($r.ReturnValue -ne 0 -or -not $r.ImageData) { continue }

            $null = $results.Add([pscustomobject]@{
                id   = "$($v.Id)"
                data = [Convert]::ToBase64String([byte[]]$r.ImageData)
            })
        } catch {}
    }

    if ($results.Count -eq 0) { Write-Output "[]" }
    else { ConvertTo-Json -InputObject $results -Compress }
} catch {
    [Console]::Error.WriteLine("INVENTORY_THUMBNAIL_ERROR: $($_.Exception.Message)")
    exit 1
}
`, width, height)
}

// collectVMThumbnails captures the console thumbnail of every Running VM and
// returns JPEG bytes keyed by lowercase VM id. It is best effort: any failure is
// logged and yields an empty (or partial) map, never an inventory error.
func (d *Dispatcher) collectVMThumbnails() map[string][]byte {
	thumbs := make(map[string][]byte)

	blockCtx, cancel := context.WithTimeout(context.Background(), thumbnailTimeout)
	output, err := hyperv.RunPowerShell(blockCtx, buildThumbnailScript(thumbnailWidth, thumbnailHeight))
	cancel()
	if err != nil {
		d.logger.Warn("vm_inventory: thumbnail capture failed, inventory sent without thumbnails", "error", err)
		return thumbs
	}

	raws, err := parseThumbnailOutput(output)
	if err != nil {
		d.logger.Warn("vm_inventory: failed to parse thumbnail output", "error", err)
		return thumbs
	}

	totalBytes := 0
	for _, raw := range raws {
		frame, err := base64.StdEncoding.DecodeString(raw.Data)
		if err != nil {
			d.logger.Warn("vm_inventory: invalid thumbnail base64", "vm_id", raw.ID, "error", err)
			continue
		}
		if expected := thumbnailWidth*thumbnailHeight*2 + thumbnailHeaderBytes; len(frame) != expected {
			d.logger.Info("vm_inventory: unexpected thumbnail frame size",
				"vm_id", raw.ID, "bytes", len(frame), "expected", expected)
		}
		jpg, err := rgb565ToJPEG(frame, thumbnailWidth, thumbnailHeight, thumbnailQuality)
		if err != nil {
			d.logger.Warn("vm_inventory: thumbnail conversion failed", "vm_id", raw.ID, "error", err)
			continue
		}
		thumbs[strings.ToLower(raw.ID)] = jpg
		totalBytes += len(jpg)
	}

	d.logger.Info("vm_inventory: thumbnails captured",
		"captured", len(thumbs), "returned", len(raws), "jpeg_total_bytes", totalBytes)
	return thumbs
}

// parseThumbnailOutput parses the thumbnail block output, accepting either a JSON
// array or a single object.
func parseThumbnailOutput(output []byte) ([]vmThumbnailRaw, error) {
	output = trimPowerShellOutput(output)
	if len(output) == 0 || string(output) == "[]" {
		return nil, nil
	}

	var raws []vmThumbnailRaw
	if err := json.Unmarshal(output, &raws); err != nil {
		var single vmThumbnailRaw
		if err2 := json.Unmarshal(output, &single); err2 != nil {
			return nil, err
		}
		raws = []vmThumbnailRaw{single}
	}
	return raws, nil
}

// rgb565ToJPEG converts a raw RGB565 frame (little-endian, 2 bytes per pixel, rows
// packed with no padding) as returned by GetVirtualSystemThumbnailImage into a JPEG.
//
// If the buffer is longer than one frame, the extra leading bytes are treated as
// a header and skipped, so the pixel rows stay aligned.
func rgb565ToJPEG(raw []byte, width, height, quality int) ([]byte, error) {
	expected := width * height * 2
	if len(raw) < expected {
		return nil, fmt.Errorf("thumbnail frame too short: got %d bytes, expected %d", len(raw), expected)
	}
	raw = raw[len(raw)-expected:]

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := (y*width + x) * 2
			v := uint16(raw[i]) | uint16(raw[i+1])<<8

			r5 := (v >> 11) & 0x1F
			g6 := (v >> 5) & 0x3F
			b5 := v & 0x1F

			// Expand to 8 bits by replicating the high bits into the low bits so
			// full intensity maps to 255.
			o := img.PixOffset(x, y)
			img.Pix[o] = uint8(r5<<3 | r5>>2)
			img.Pix[o+1] = uint8(g6<<2 | g6>>4)
			img.Pix[o+2] = uint8(b5<<3 | b5>>2)
			img.Pix[o+3] = 0xFF
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
