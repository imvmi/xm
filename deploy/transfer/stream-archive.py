#!/usr/bin/env python3
"""Write the complete ZIP64 migration archive to stdout; no local ZIP is created."""
import pathlib
import subprocess
import sys
import zipfile


def main():
    root = pathlib.Path(__file__).resolve().parent / "app"
    state = subprocess.check_output(
        ["systemctl", "--user", "show", "xm.service", "--property=ActiveState", "--value"],
        text=True,
    ).strip()
    if state != "inactive":
        raise RuntimeError("Source XM service must remain stopped during transfer.")
    if not (root / "manifest.json").is_file():
        raise RuntimeError("Migration preparation is not complete.")
    print("Streaming full XM ZIP64 archive; keep the SSH connection open.", file=sys.stderr)
    with zipfile.ZipFile(sys.stdout.buffer, "w", allowZip64=True) as archive:
        for path in sorted(root.rglob("*")):
            if not path.is_file():
                continue
            rel = path.relative_to(root)
            # Media are already compressed. Only compress metadata and binaries.
            compression = (
                zipfile.ZIP_STORED
                if rel.parts[:2] in (("data", "media"), ("data", "avatars"))
                else zipfile.ZIP_DEFLATED
            )
            archive.write(
                path, "XM/" + rel.as_posix(), compress_type=compression, compresslevel=3
            )
    print("Full ZIP stream completed successfully.", file=sys.stderr)


if __name__ == "__main__":
    try:
        main()
    except (BrokenPipeError, KeyboardInterrupt):
        sys.exit(1)
    except Exception as exc:
        print(str(exc), file=sys.stderr)
        sys.exit(1)
