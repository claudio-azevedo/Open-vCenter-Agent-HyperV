package tasks

import (
	"bytes"
	"image/jpeg"
	"testing"
)

// TestRGB565ToJPEG_SkipsHeader checks that a frame carrying the 4-byte header
// decodes to the requested size with the header skipped (no row shift), and that
// full-intensity RGB565 channels expand to (close to) 255 after JPEG round-trip.
func TestRGB565ToJPEG_SkipsHeader(t *testing.T) {
	const w, h = 16, 8
	raw := make([]byte, thumbnailHeaderBytes, thumbnailHeaderBytes+w*h*2)
	for i := 0; i < w*h; i++ {
		// pure red: r5=31, g6=0, b5=0 -> 0xF800, little-endian
		raw = append(raw, 0x00, 0xF8)
	}

	out, err := rgb565ToJPEG(raw, w, h, 90)
	if err != nil {
		t.Fatalf("rgb565ToJPEG: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("size = %dx%d, want %dx%d", b.Dx(), b.Dy(), w, h)
	}
	r, g, b, _ := img.At(w-1, h-1).RGBA()
	if r>>8 < 240 || g>>8 > 20 || b>>8 > 20 {
		t.Fatalf("pixel = (%d,%d,%d), want ~(255,0,0)", r>>8, g>>8, b>>8)
	}
}

func TestRGB565ToJPEG_ShortFrame(t *testing.T) {
	if _, err := rgb565ToJPEG(make([]byte, 10), 16, 8, 70); err == nil {
		t.Fatal("expected an error for a frame shorter than width*height*2")
	}
}

func TestParseThumbnailOutput_SingleObject(t *testing.T) {
	raws, err := parseThumbnailOutput([]byte(`{"id":"ABC","data":"AAAA"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(raws) != 1 || raws[0].ID != "ABC" {
		t.Fatalf("raws = %+v", raws)
	}
}

func TestAssembleVMInventory_BlankGuestOSIsOmitted(t *testing.T) {
	blank, win := "  ", "Windows Server 2022 Datacenter"
	vms := assembleVMInventory(
		[]vmBasicInfo{{ID: "a", Name: "a", GuestOS: &blank}, {ID: "b", Name: "b", GuestOS: &win}},
		nil, nil, nil, nil, nil, nil, nil,
	)
	if vms[0].GuestOS != nil {
		t.Fatalf("blank guestOs should be nil, got %q", *vms[0].GuestOS)
	}
	if vms[1].GuestOS == nil || *vms[1].GuestOS != win {
		t.Fatalf("guestOs = %v, want %q", vms[1].GuestOS, win)
	}
}
