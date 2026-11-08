"""Errors raised by the Stratus SDK."""

from __future__ import annotations

from typing import Optional


class StratusError(Exception):
    """A failure reported by Stratus, or a failure to reach it.

    ``code`` is stable and safe to branch on (for example ``SnapshotNotFound``),
    and ``hint`` says what to do next.
    """

    #: Codes produced by the SDK itself rather than the server.
    UNREACHABLE = "StratusUnreachable"
    TIMEOUT = "StratusTimeout"
    BAD_RESPONSE = "InvalidResponse"

    def __init__(self, code: str, message: str, hint: Optional[str] = None, status: int = 0) -> None:
        self.code = code
        self.message = message
        self.hint = hint
        #: HTTP status, or 0 when Stratus could not be reached.
        self.status = status
        text = f"{code}: {message}"
        if hint:
            text += f" ({hint})"
        super().__init__(text)
