/**
 * A failure reported by Stratus, or a failure to reach it.
 *
 * `code` is stable and safe to branch on (for example "SnapshotNotFound"),
 * and `hint` says what to do next.
 */
export class StratusError extends Error {
  override readonly name = "StratusError";

  constructor(
    readonly code: string,
    message: string,
    readonly hint: string | undefined,
    /** HTTP status, or 0 when Stratus could not be reached. */
    readonly status: number,
    options?: { cause?: unknown },
  ) {
    super(hint ? `${code}: ${message} (${hint})` : `${code}: ${message}`, options);
  }

  /** Codes produced by the SDK itself rather than the server. */
  static readonly Unreachable = "StratusUnreachable";
  static readonly Timeout = "StratusTimeout";
  static readonly BadResponse = "InvalidResponse";
}
