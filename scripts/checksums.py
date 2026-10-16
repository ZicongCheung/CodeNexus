#!/usr/bin/env python3
"""Write a SHA256SUMS file covering the release archives.

    python scripts/checksums.py dist
"""

import hashlib
import os
import sys


def main():
    directory = sys.argv[1] if len(sys.argv) > 1 else "dist"
    names = sorted(
        n for n in os.listdir(directory) if n.startswith("codenexus_") and os.path.isfile(os.path.join(directory, n))
    )
    if not names:
        print(f"no archives found in {directory}", file=sys.stderr)
        return 1
    lines = []
    for name in names:
        digest = hashlib.sha256(open(os.path.join(directory, name), "rb").read()).hexdigest()
        lines.append(f"{digest}  {name}")
    out = os.path.join(directory, "SHA256SUMS")
    with open(out, "w", encoding="utf-8", newline="\n") as fh:
        fh.write("\n".join(lines) + "\n")
    print("\n".join(lines))
    print(f"wrote {out}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
