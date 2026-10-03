# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

"""Go validation includes imported internal packages and excludes stdlib."""

from unittest.mock import Mock

import pytest

import check_go


def test_import_graph_filters_standard_library_and_duplicates(monkeypatch):
    runner = Mock(side_effect=["github.com/goabonga/maestro", "net/http\ngithub.com/goabonga/maestro/cmd/svc\ngithub.com/goabonga/maestro/internal/transport\ngithub.com/goabonga/maestro/internal/transport"])
    monkeypatch.setattr(check_go, "run", runner)
    entry, packages = check_go.packages("maestro-svc")
    assert entry == ["./cmd/svc"]
    assert packages == ["github.com/goabonga/maestro/cmd/svc", "github.com/goabonga/maestro/internal/transport"]
    assert runner.call_args.args[-1] == "./cmd/svc"


def test_unknown_component_does_not_run_tools(monkeypatch):
    runner = Mock()
    monkeypatch.setattr(check_go, "run", runner)
    with pytest.raises(ValueError, match="Unknown Go component"):
        check_go.packages("../unregistered")
    runner.assert_not_called()


def test_empty_import_graph_blocks_validation(monkeypatch):
    monkeypatch.setattr(check_go, "run", Mock(side_effect=["github.com/goabonga/maestro", "net/http"]))
    with pytest.raises(ValueError, match="No Go packages"):
        check_go.packages("maestro-svc")
