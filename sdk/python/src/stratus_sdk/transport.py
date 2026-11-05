"""HTTP transport for the control API, using only the standard library."""

from __future__ import annotations

import json
import socket
import urllib.error
import urllib.request
from typing import Any, Optional, Protocol

from .errors import StratusError

CONTROL_PREFIX = "/_stratus/control/v1"


class Transport(Protocol):
    """Sends one control API request and returns the decoded JSON body (None when empty)."""

    def request(self, method: str, path: str, body: Any = None) -> Any: ...


class UrllibTransport:
    """Transport over urllib. Proxies are bypassed: Stratus is a local endpoint."""

    def __init__(self, endpoint: str, timeout: float = 60.0) -> None:
        self.endpoint = endpoint
        # Above the server's 30s drain limit for reset, snapshot and restore.
        self.timeout = timeout
        self._opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def request(self, method: str, path: str, body: Any = None) -> Any:
        url = self.endpoint + CONTROL_PREFIX + path
        data = None
        headers = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(url, data=data, method=method, headers=headers)
        try:
            with self._opener.open(req, timeout=self.timeout) as res:
                return _decode(res.read(), res.status, method, path)
        except urllib.error.HTTPError as err:
            raise _to_error(err.code, err.read()) from None
        except (socket.timeout, TimeoutError) as err:
            raise StratusError(StratusError.TIMEOUT, f"{method} {url} timed out after {self.timeout}s") from err
        except urllib.error.URLError as err:
            if isinstance(err.reason, (socket.timeout, TimeoutError)):
                raise StratusError(StratusError.TIMEOUT, f"{method} {url} timed out after {self.timeout}s") from err
            raise StratusError(
                StratusError.UNREACHABLE,
                f"cannot reach Stratus at {self.endpoint}: {err.reason}",
                "start it with `stratus serve`, or set STRATUS_ENDPOINT",
            ) from err
        except ConnectionError as err:
            raise StratusError(
                StratusError.UNREACHABLE,
                f"cannot reach Stratus at {self.endpoint}: {err}",
                "start it with `stratus serve`, or set STRATUS_ENDPOINT",
            ) from err


def _decode(raw: bytes, status: int, method: str, path: str) -> Any:
    if not raw:
        return None
    try:
        return json.loads(raw)
    except ValueError:
        raise StratusError(StratusError.BAD_RESPONSE, f"{method} {path} returned invalid JSON", status=status) from None


def _to_error(status: int, raw: bytes) -> StratusError:
    payload: Optional[Any]
    try:
        payload = json.loads(raw) if raw else None
    except ValueError:
        payload = None
    err = payload.get("error") if isinstance(payload, dict) else None
    if isinstance(err, dict) and isinstance(err.get("code"), str):
        hint = err.get("hint")
        return StratusError(err["code"], str(err.get("message", "")), hint if isinstance(hint, str) else None, status)
    text = raw.decode(errors="replace").strip() or f"request failed with status {status}"
    return StratusError(f"HTTP{status}", text, status=status)
