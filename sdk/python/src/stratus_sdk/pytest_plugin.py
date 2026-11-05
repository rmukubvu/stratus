"""pytest fixtures, registered automatically when stratus-sdk is installed.

``stratus``: a session-wide client, waited until ready.
``stratus_isolated``: the same client, with all state rolled back after the test.
"""

from __future__ import annotations

from typing import Iterator

import pytest

from .client import StratusClient


@pytest.fixture(scope="session")
def stratus() -> StratusClient:
    client = StratusClient()
    client.wait_until_ready()
    return client


@pytest.fixture
def stratus_isolated(stratus: StratusClient) -> Iterator[StratusClient]:
    with stratus.snapshot_scope():
        yield stratus
