# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Validate license edits, branding orchestration and real favicon output."""

from pathlib import Path
import subprocess
from unittest.mock import Mock

from PIL import Image
import pytest

import add_license_header as license
import generate_favicon as favicon
import regen_icons as icons


@pytest.mark.parametrize(
    "filename,body,prefix",
    [
        ("script.py", "#!/usr/bin/env python3\nprint('test')\n", "#"),
        ("main.go", "//go:build linux\n\npackage main\n", "//"),
        ("config.toml", "version = 1\n", "#"),
    ],
)
def test_license_header_is_idempotent_and_preserves_body(
    tmp_path, filename, body, prefix
):
    path = tmp_path / filename
    path.write_text(body)
    assert license.add_license_header(str(path), prefix)
    result = path.read_text()
    assert license.check_license(str(path), prefix)
    assert not license.add_license_header(str(path), prefix)
    assert path.read_text() == result
    assert (
        "print('test')" in result
        if filename.endswith("py")
        else body.rstrip() in result
    )
    if body.startswith("#!"):
        assert result.splitlines()[0] == body.splitlines()[0]


def test_license_check_reports_missing_headers_without_writing(tmp_path):
    path = tmp_path / "script.py"
    path.write_text("print('test')\n")
    assert license.process_directory(str(tmp_path), ["py"], check_only=True) == 1
    assert path.read_text() == "print('test')\n"
    assert license.process_directory(str(tmp_path), ["go"], check_only=True) == 0


def test_icons_copy_canonical_svg_and_use_locked_project(
    tmp_path, monkeypatch
):
    (tmp_path / "assets").mkdir()
    (tmp_path / "docs").mkdir()
    master = tmp_path / "assets/maestro.svg"
    master.write_text("<svg/>")
    monkeypatch.setattr(icons, "__file__", str(tmp_path / "scripts/regen_icons.py"))
    command = Mock()
    monkeypatch.setattr(icons.subprocess, "run", command)
    icons.main()
    assert (tmp_path / "docs/maestro.svg").read_bytes() == master.read_bytes()
    args = command.call_args.args[0]
    assert args[:7] == ["uv", "run", "--project", str(tmp_path / "scripts"), "--locked", "--no-dev", "python"]
    assert str(tmp_path / "docs/favicon.ico") in args
    assert command.call_args.kwargs["check"] is True


def test_missing_canonical_icon_fails_before_conversion(tmp_path, monkeypatch):
    monkeypatch.setattr(icons, "__file__", str(tmp_path / "scripts/regen_icons.py"))
    command = Mock()
    monkeypatch.setattr(icons.subprocess, "run", command)
    with pytest.raises(SystemExit, match="Missing canonical SVG"):
        icons.main()
    command.assert_not_called()


def test_favicon_contains_all_three_resolutions(tmp_path, monkeypatch):
    svg = tmp_path / "master.svg"
    svg.write_text(
        '<svg xmlns="http://www.w3.org/2000/svg" width="256" height="256">'
        '<rect width="256" height="256" fill="#ff0000"/></svg>'
    )
    output = tmp_path / "nested/favicon.ico"
    monkeypatch.setattr(
        "sys.argv", ["generate_favicon.py", "-i", str(svg), "-o", str(output)]
    )
    favicon.main()
    with Image.open(output) as image:
        assert image.format == "ICO"
        assert image.ico.sizes() == {(16, 16), (32, 32), (48, 48)}
        assert image.convert("RGB").getpixel((0, 0)) == (255, 0, 0)


def test_license_check_excludes_local_uv_dependencies(tmp_path):
    dependency = tmp_path / ".venv/lib/dependency.py"
    dependency.parent.mkdir(parents=True)
    dependency.write_text("# Third party code\n")
    assert license.process_directory(str(tmp_path), ["py"], check_only=True) == 0
