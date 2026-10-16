// Package assets holds binary artwork that ships inside the executable.
//
// The icon lives here rather than at the repository root because go:embed can
// only reach into the package's own directory, and the tray needs it at
// runtime to build a HICON without reading anything from disk.
package assets

import _ "embed"

// Icon is the multi-size Windows icon: 16, 24, 32, 48, 64, 128 as DIB entries
// and 256 as PNG. The DIB entries are what a running process can hand to
// CreateIconFromResourceEx directly.
//
//go:embed codenexus.ico
var Icon []byte

// IconPNG is the 256px artwork on its own, useful for docs and for any surface
// that wants a single PNG.
//
//go:embed codenexus-256.png
var IconPNG []byte

// IconFromICO extracts the entry closest to want pixels from an .ico. Windows
// expects a bare bitmap, not the container, so the directory has to be walked
// and the payload handed over on its own. Preferring the larger entry on a tie
// keeps the scaled-down result sharp.
func IconFromICO(ico []byte, want int) ([]byte, int, bool) {
	if len(ico) < 6 || ico[2] != 1 { // reserved must be 0, type must be 1 (icon)
		return nil, 0, false
	}
	count := int(ico[4]) | int(ico[5])<<8
	bestIndex, bestDelta, bestSize := -1, 0, 0
	for i := 0; i < count; i++ {
		off := 6 + i*16
		if off+16 > len(ico) {
			break
		}
		size := int(ico[off])
		if size == 0 { // a zero in the directory means 256
			size = 256
		}
		delta := size - want
		if delta < 0 {
			delta = -delta
		}
		if bestIndex < 0 || delta < bestDelta || (delta == bestDelta && size > bestSize) {
			bestIndex, bestDelta, bestSize = i, delta, size
		}
	}
	if bestIndex < 0 {
		return nil, 0, false
	}
	off := 6 + bestIndex*16
	length := int(ico[off+8]) | int(ico[off+9])<<8 | int(ico[off+10])<<16 | int(ico[off+11])<<24
	start := int(ico[off+12]) | int(ico[off+13])<<8 | int(ico[off+14])<<16 | int(ico[off+15])<<24
	if start < 0 || length <= 0 || start+length > len(ico) {
		return nil, 0, false
	}
	return ico[start : start+length], bestSize, true
}
