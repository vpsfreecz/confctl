"""Create the finite synthetic upstream for the Nix offline-dependency fixture."""
import json
from pathlib import Path
import sys
import zipfile

root = Path(sys.argv[1]) / "example.test/hookdep/@v"
root.mkdir(parents=True)
module = b"module example.test/hookdep\n\ngo 1.23\n"
(root / "v1.0.0.mod").write_bytes(module)
(root / "v1.0.0.info").write_text(
    json.dumps({"Version": "v1.0.0", "Time": "2026-01-01T00:00:00Z"}) + "\n"
)
with zipfile.ZipFile(root / "v1.0.0.zip", "w", zipfile.ZIP_STORED) as archive:
    for name, data in {
        "go.mod": module,
        "dep.go": b"package hookdep\n\nconst Value = 42\n",
    }.items():
        info = zipfile.ZipInfo("example.test/hookdep@v1.0.0/" + name)
        info.external_attr = 0o644 << 16
        archive.writestr(info, data)
