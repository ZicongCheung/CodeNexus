#!/usr/bin/env python3
"""Package build output into the archives the release publishes.

Kept in Python (standard library only) because zip and tar.gz have to be
produced the same way on Windows and on CI's Linux: shelling out to `zip` would
fail on a Git Bash machine that does not have it. Timestamps, ownership and
names are pinned so that packaging the same tree twice yields byte-identical
archives on a given machine (cross-OS builds still differ in mode bits, which
the host cannot avoid recording).

    python scripts/package.py --format zip --out dist/foo.zip --exec app dir1 file1

--exec names the entries that must be executable inside the archive. It exists
because Windows cannot record a POSIX execute bit: the gateway binary has no
extension, so a tar.gz built on Windows would otherwise unpack as 0644 and the
first `./codenexus` would fail. The caller knows which file is the program, so
it says so rather than the packer guessing.
"""

import argparse
import gzip
import os
import stat
import sys
import tarfile
import zipfile


def collect(paths):
    """Yield (archive name, absolute path) for files, expanding directories."""
    seen = set()
    for path in paths:
        if os.path.isdir(path):
            base = os.path.dirname(path.rstrip("/\\"))
            for root, _, names in os.walk(path):
                for name in sorted(names):
                    full = os.path.join(root, name)
                    arc = os.path.relpath(full, base).replace(os.sep, "/")
                    if arc not in seen:
                        seen.add(arc)
                        yield arc, full
        elif os.path.isfile(path):
            arc = os.path.basename(path)
            if arc not in seen:
                seen.add(arc)
                yield arc, path


def archive_mode(full, arc, exec_names):
    """The POSIX mode an entry should carry in the archive.

    Only the executables are forced; everything else keeps whatever the host
    recorded, which is what makes a Windows-built archive look like a
    Unix-built one once the extractor applies its umask.
    """
    name = os.path.basename(arc)
    if name in exec_names or name.lower().endswith(".exe"):
        return 0o755
    return os.stat(full).st_mode & 0o777


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--format", choices=["zip", "tar.gz"], required=True)
    parser.add_argument("--out", required=True, help="archive path to write")
    parser.add_argument(
        "--exec",
        default="",
        help="comma-separated archive entry names to mark executable",
    )
    parser.add_argument("paths", nargs="+", help="files and directories to include")
    args = parser.parse_args()

    exec_names = {n for n in args.exec.split(",") if n}
    items = list(collect(args.paths))
    if not items:
        print("nothing to package", file=sys.stderr)
        return 1

    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
    if args.format == "zip":
        with zipfile.ZipFile(args.out, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as zf:
            for arc, full in items:
                # Executables keep their mode via the external attributes zip
                # stores; without it a Windows-built archive would unpack 0644.
                info = zipfile.ZipInfo(arc, date_time=(1980, 1, 1, 0, 0, 0))
                info.compress_type = zipfile.ZIP_DEFLATED
                mode = archive_mode(full, arc, exec_names)
                info.external_attr = (stat.S_IFREG | mode) << 16
                with open(full, "rb") as src:
                    zf.writestr(info, src.read())
    else:
        # The gzip container carries its own timestamp and, if left to infer it
        # from the output path, the file's own name -- both would make two
        # builds of the same tree differ. They are pinned, and every member's
        # mtime is zeroed, so the archive is byte-identical across runs.
        with open(args.out, "wb") as raw, gzip.GzipFile(
            filename="", fileobj=raw, mode="wb", mtime=0
        ) as gz, tarfile.open(fileobj=gz, mode="w") as tf:
            for arc, full in items:
                info = tf.gettarinfo(full, arcname=arc)
                info.uid = info.gid = 0
                info.uname = info.gname = "root"
                info.mode = archive_mode(full, arc, exec_names)
                # A fixed stamp keeps repeated builds byte-identical.
                info.mtime = 0
                with open(full, "rb") as src:
                    tf.addfile(info, src)

    total = os.path.getsize(args.out)
    print(f"packaged {len(items)} file(s) -> {args.out} ({total} bytes)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
