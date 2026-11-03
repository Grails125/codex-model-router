"""Create a macOS archive with executable modes, using only Python stdlib."""
import os
import pathlib
import sys
import tarfile


def main() -> None:
    stage = pathlib.Path(sys.argv[1]).resolve()
    output = pathlib.Path(sys.argv[2]).resolve()
    executable = {
        "bin/darwin-amd64/codex-router",
        "bin/darwin-arm64/codex-router",
        "install-macos.sh",
        "uninstall-macos.sh",
        "doctor-macos.sh",
        "Install-Codex-Model-Router.command",
        "change-deepseek-key-macos.sh",
        "Change-DeepSeek-API-Key.command",
    }
    with tarfile.open(output, "w:gz") as archive:
        for path in sorted(stage.rglob("*")):
            if (
                "/windows-" in path.as_posix()
                or path.name.lower().endswith(".exe")
                or path.suffix.lower() in {".ps1", ".cmd"}
            ):
                continue
            relative = path.relative_to(stage).as_posix()
            info = archive.gettarinfo(str(path), arcname="CodexModelRouter/" + relative)
            if path.is_file():
                info.mode = 0o755 if relative in executable else 0o644
                info.uid = os.getuid() if hasattr(os, "getuid") else 0
                info.gid = os.getgid() if hasattr(os, "getgid") else 0
                info.uname = info.gname = ""
                with path.open("rb") as source:
                    archive.addfile(info, source)
            elif path.is_dir():
                info.mode = 0o755
                archive.addfile(info)


if __name__ == "__main__":
    main()
