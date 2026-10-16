package assets

import (
	"encoding/binary"
	"testing"
)

// The tray hands the bytes returned by IconFromICO straight to
// CreateIconFromResourceEx. Windows parses them as a BITMAPINFOHEADER, which is
// little-endian, and answers a NULL HICON for anything else without saying why
// ("The operation completed successfully"). A byte-swapped header therefore
// costs the icon silently, so the shipped artwork is checked here.

func icoEntries(t *testing.T, ico []byte) int {
	t.Helper()
	if len(ico) < 6 {
		t.Fatalf("icon is %d bytes, too short for a directory", len(ico))
	}
	if ico[0] != 0 || ico[1] != 0 {
		t.Errorf("reserved field is %#x, want 0", ico[:2])
	}
	if ico[2] != 1 || ico[3] != 0 {
		t.Errorf("type is %#x, want 1 (icon)", ico[2:4])
	}
	return int(binary.LittleEndian.Uint16(ico[4:6]))
}

func TestShippedIconIsAValidICO(t *testing.T) {
	count := icoEntries(t, Icon)
	if count != 7 {
		t.Fatalf("icon has %d entries, want 7 (16..128 DIB + 256 PNG)", count)
	}
	for i := 0; i < count; i++ {
		entry := Icon[6+i*16 : 6+i*16+16]
		size := binary.LittleEndian.Uint32(entry[8:12])
		start := binary.LittleEndian.Uint32(entry[12:16])
		if int(start)+int(size) > len(Icon) {
			t.Fatalf("entry %d points past the end of the file", i)
		}
	}
}

func TestShippedIconDIBHeadersAreLittleEndian(t *testing.T) {
	count := icoEntries(t, Icon)
	dibs := 0
	for i := 0; i < count; i++ {
		entry := Icon[6+i*16 : 6+i*16+16]
		size := int(binary.LittleEndian.Uint32(entry[8:12]))
		start := int(binary.LittleEndian.Uint32(entry[12:16]))
		payload := Icon[start : start+size]
		if len(payload) >= 8 && string(payload[:8]) == "\x89PNG\r\n\x1a\n" {
			continue // the 256px entry is a PNG, which Windows parses itself
		}
		dibs++
		header, ok := parseDIBHeader(payload)
		if !ok {
			t.Fatalf("entry %d does not carry a little-endian BITMAPINFOHEADER", i)
		}
		if header.size != 40 || header.planes != 1 || header.bpp != 32 || header.compression != 0 {
			t.Errorf("entry %d header = %+v, want a plain 32-bit uncompressed DIB", i, header)
		}
		if header.height != header.width*2 {
			t.Errorf("entry %d biHeight %d is not twice biWidth %d", i, header.height, header.width)
		}
		// The mask rows are padded to four bytes and counted into biHeight.
		pixels := header.width * (header.height / 2)
		mask := ((header.width + 31) / 32) * 4 * (header.height / 2)
		if 40+pixels*4+mask != size {
			t.Errorf("entry %d is %d bytes, want %d", i, size, 40+pixels*4+mask)
		}
		if int(header.imageSize) != pixels*4 {
			t.Errorf("entry %d biSizeImage = %d, want %d", i, header.imageSize, pixels*4)
		}
	}
	if dibs != 6 {
		t.Errorf("found %d DIB entries, want 6", dibs)
	}
}

type dibHeader struct {
	size        uint32
	width       int
	height      int
	planes      uint16
	bpp         uint16
	compression uint32
	imageSize   uint32
}

func parseDIBHeader(payload []byte) (dibHeader, bool) {
	if len(payload) < 40 {
		return dibHeader{}, false
	}
	// Read it little-endian: a big-endian DIB spells biSize as 0x28000000 here,
	// which is what the broken artwork did.
	h := dibHeader{
		size:        binary.LittleEndian.Uint32(payload[0:4]),
		width:       int(int32(binary.LittleEndian.Uint32(payload[4:8]))),
		height:      int(int32(binary.LittleEndian.Uint32(payload[8:12]))),
		planes:      binary.LittleEndian.Uint16(payload[12:14]),
		bpp:         binary.LittleEndian.Uint16(payload[14:16]),
		compression: binary.LittleEndian.Uint32(payload[16:20]),
		imageSize:   binary.LittleEndian.Uint32(payload[20:24]),
	}
	if h.size != 40 || h.width <= 0 || h.height <= 0 {
		return dibHeader{}, false
	}
	return h, true
}

func TestIconFromICOPicksATraySizedEntry(t *testing.T) {
	payload, size, ok := IconFromICO(Icon, 32)
	if !ok {
		t.Fatal("IconFromICO found no usable entry")
	}
	if size != 32 {
		t.Errorf("picked the %dpx entry, want 32px", size)
	}
	if _, valid := parseDIBHeader(payload); !valid {
		t.Fatal("the entry handed to CreateIconFromResourceEx is not a valid DIB")
	}
}

func TestIconFromICORejectsGarbage(t *testing.T) {
	for _, ico := range [][]byte{nil, {}, {0, 0, 0, 0, 0, 0}, Icon[:4]} {
		if _, _, ok := IconFromICO(ico, 32); ok {
			t.Errorf("IconFromICO accepted %d bytes of non-icon data", len(ico))
		}
	}
}

func TestIconPNGIsAPNG(t *testing.T) {
	if len(IconPNG) < 8 || string(IconPNG[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatal("IconPNG does not start with a PNG signature")
	}
}
