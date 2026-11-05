"""The pytest fixtures, exercised against a real Stratus when STRATUS_ENDPOINT is set."""

import os
import unittest
import urllib.error
import urllib.request

try:
    import pytest
except ImportError:  # collected by plain unittest
    raise unittest.SkipTest("pytest is not installed")

pytestmark = pytest.mark.skipif(not os.environ.get("STRATUS_ENDPOINT"), reason="set STRATUS_ENDPOINT")


def _status(endpoint: str, path: str, method: str) -> int:
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(urllib.request.Request(endpoint + path, method=method), timeout=10) as res:
            return res.status
    except urllib.error.HTTPError as err:
        return err.code


def test_isolated_fixture_rolls_back_state(stratus_isolated):
    assert _status(stratus_isolated.endpoint, "/sdk-py-fixture", "PUT") == 200


def test_state_from_previous_test_is_gone(stratus):
    assert _status(stratus.endpoint, "/sdk-py-fixture", "HEAD") == 404
