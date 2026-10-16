#!/usr/bin/env python3
"""Build the application icon from the source artwork.

The console ships an icon so the executable is identifiable in Explorer, the
taskbar and the tray. Windows wants one .ico holding several sizes, and it
wants the small entries as DIBs rather than PNGs so a running process can load
them straight into an HICON without touching the disk.

Everything here is standard library: the sources are RGBA8 PNGs, resizing is a
box filter, and the .ico is assembled by hand. That keeps the build reproducible
on any machine and in CI, with nothing to install.

    python scripts/make-icons.py --src <dir with the source PNGs> --out assets
"""

import argparse
import os
import struct
import sys
import zlib

# The sizes Windows and the tray actually ask for. 256 stays a PNG to keep the
# executable small; the rest are DIBs because that is what CreateIcon-
# FromResourceEx accepts.
DIB_SIZES = [16, 24, 32, 48, 64, 128]
PNG_SIZES = [256]

# Sources, best first. A size that exists as a source is used verbatim instead
# of being resampled, which keeps the small icons crisp.
SOURCE_FILES = [
    "favicon-16x16.png",
    "favicon-32x32.png",
    "mstile-150x150.png",
    "apple-touch-icon.png",
    "android-chrome-192x192.png",
    "android-chrome-512x512.png",
]


def read_png(path):
    """Decode an 8-bit RGBA PNG into (width, height, bytearray of RGBA rows)."""
    data = open(path, "rb").read()
    if data[:8] != b"\x89PNG\r\n\x1a\n":
        raise ValueError(f"{path}: not a PNG")
    pos = 8
    header = None
    idat = bytearray()
    while pos < len(data):
        length, ctype = struct.unpack(">I4s", data[pos : pos + 8])
        body = data[pos + 8 : pos + 8 + length]
        pos += 12 + length  # length + type + body + crc
        if ctype == b"IHDR":
            width, height, depth, color, comp, filt, interlace = struct.unpack(">IIBBBBB", body)
            if depth != 8 or color != 6:
                raise ValueError(f"{path}: expected 8-bit RGBA, got depth={depth} color={color}")
            if interlace != 0:
                raise ValueError(f"{path}: interlaced PNGs are not supported")
            header = (width, height)
        elif ctype == b"IDAT":
            idat += body
        elif ctype == b"IEND":
            break
    if header is None:
        raise ValueError(f"{path}: missing IHDR")
    width, height = header

    raw = zlib.decompress(bytes(idat))
    stride = width * 4
    out = bytearray(height * stride)
    prev = bytearray(stride)
    pos = 0
    for y in range(height):
        ftype = raw[pos]
        pos += 1
        line = bytearray(raw[pos : pos + stride])
        pos += stride
        if ftype == 1:  # Sub
            for i in range(4, stride):
                line[i] = (line[i] + line[i - 4]) & 0xFF
        elif ftype == 2:  # Up
            for i in range(stride):
                line[i] = (line[i] + prev[i]) & 0xFF
        elif ftype == 3:  # Average
            for i in range(stride):
                left = line[i - 4] if i >= 4 else 0
                line[i] = (line[i] + ((left + prev[i]) >> 1)) & 0xFF
        elif ftype == 4:  # Paeth
            for i in range(stride):
                a = line[i - 4] if i >= 4 else 0
                b = prev[i]
                c = prev[i - 4] if i >= 4 else 0
                p = a + b - c
                pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
                if pa <= pb and pa <= pc:
                    pr = a
                elif pb <= pc:
                    pr = b
                else:
                    pr = c
                line[i] = (line[i] + pr) & 0xFF
        elif ftype != 0:
            raise ValueError(f"{path}: unknown filter {ftype}")
        out[y * stride : (y + 1) * stride] = line
        prev = line
    return width, height, out


def resize(width, height, pixels, size):
    """Box-filter down (or up) to size x size. Returns RGBA bytes."""
    if width == size and height == size:
        return bytes(pixels)
    out = bytearray(size * size * 4)
    for y in range(size):
        y0 = y * height / size
        y1 = (y + 1) * height / size
        iy0, iy1 = int(y0), max(int(y1 + 0.999), int(y0) + 1)
        for x in range(size):
            x0 = x * width / size
            x1 = (x + 1) * width / size
            ix0, ix1 = int(x0), max(int(x1 + 0.999), int(x0) + 1)
            r = g = b = a = 0
            n = 0
            for sy in range(iy0, min(iy1, height)):
                row = sy * width * 4
                for sx in range(ix0, min(ix1, width)):
                    i = row + sx * 4
                    r += pixels[i]
                    g += pixels[i + 1]
                    b += pixels[i + 2]
                    a += pixels[i + 3]
                    n += 1
            i = (y * size + x) * 4
            out[i] = r // n
            out[i + 1] = g // n
            out[i + 2] = b // n
            out[i + 3] = a // n
    return bytes(out)


def png_encode(size, rgba):
    """Encode RGBA bytes as a PNG. Only used for the 256px entry."""
    raw = bytearray()
    stride = size * 4
    for y in range(size):
        raw.append(0)
        raw += rgba[y * stride : (y + 1) * stride]

    def chunk(ctype, body):
        return (
            struct.pack(">I", len(body))
            + ctype
            + body
            + struct.pack(">I", zlib.crc32(ctype + body) & 0xFFFFFFFF)
        )

    out = b"\x89PNG\r\n\x1a\n"
    out += chunk(b"IHDR", struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0))
    out += chunk(b"IDAT", zlib.compress(bytes(raw), 9))
    out += chunk(b"IEND", b"")
    return out


def dib_entry(size, rgba):
    """Build an ICO DIB entry: a bottom-up BGRA bitmap plus its AND mask.

    The AND mask is all zero because the alpha channel carries the shape; it
    still has to be present, and its rows are padded to four bytes.
    """
    header = struct.pack(
        ">IiiHHIIiiII",
        40,
        size,
        size * 2,  # XOR bitmap + AND mask
        1,
        32,
        0,
        size * size * 4,
        0,
        0,
        0,
        0,
    )
    xor = bytearray()
    for y in range(size - 1, -1, -1):  # bottom-up
        row = y * size * 4
        for x in range(size):
            i = row + x * 4
            xor += bytes((rgba[i + 2], rgba[i + 1], rgba[i], rgba[i + 3]))
    mask_stride = ((size + 31) // 32) * 4
    mask = bytes(mask_stride * size)
    return header + bytes(xor) + mask


def build_ico(entries):
    """Assemble an .ico from (size, payload, is_png) entries."""
    count = len(entries)
    out = struct.pack("<HHH", 0, 1, count)
    offset = 6 + 16 * count
    directory = b""
    body = b""
    for size, payload, is_png in entries:
        dimension = 0 if size >= 256 else size
        directory += struct.pack(
            "<BBBBHHII",
            dimension,
            dimension,
            0,
            0,
            1,
            32,
            len(payload),
            offset + len(body),
        )
        body += payload
    return out + directory + body


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--src", required=True, help="directory holding the source PNGs")
    parser.add_argument("--out", default="assets", help="output directory")
    args = parser.parse_args()

    sources = {}
    for name in SOURCE_FILES:
        path = os.path.join(args.src, name)
        if os.path.exists(path):
            sources[name] = read_png(path)
    if not sources:
        print(f"no source PNGs found in {args.src}", file=sys.stderr)
        return 1

    # Prefer a source that already has the size we need; fall back to the
    # largest one and resample.
    largest = max(sources.values(), key=lambda s: s[0])
    entries = []
    for size in DIB_SIZES + PNG_SIZES:
        exact = next((s for s in sources.values() if s[0] == size), None)
        w, h, px = exact if exact else largest
        rgba = resize(w, h, px, size)
        if size in PNG_SIZES:
            entries.append((size, png_encode(size, rgba), True))
        else:
            entries.append((size, dib_entry(size, rgba), False))

    os.makedirs(args.out, exist_ok=True)
    ico_path = os.path.join(args.out, "codenexus.ico")
    with open(ico_path, "wb") as fh:
        fh.write(build_ico(entries))
    print(f"wrote {ico_path} ({os.path.getsize(ico_path)} bytes, sizes {DIB_SIZES + PNG_SIZES})")

    # A standalone 256px PNG is handy for docs and for platforms that want one.
    png_path = os.path.join(args.out, "codenexus-256.png")
    with open(png_path, "wb") as fh:
        fh.write(png_encode(256, resize(largest[0], largest[1], largest[2], 256)))
    print(f"wrote {png_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
