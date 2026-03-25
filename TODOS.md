# TODOS

## Failed resume detection: ResumeAttempt(..., false)

When a client reconnects with a connectionID that has already expired (grace
window elapsed), identify this as a "failed resume" and emit
`ResumeAttempt(roomID, connectionID, false)` before creating the new session.

The `success bool` parameter exists but is currently always `true`. Detecting
failed resumes is non-trivial: after grace expiry the session is destroyed and
the connectionID is freed. Distinguishing "new client reusing an ID" from
"client that missed the resume window" may require protocol-level signaling
(e.g. a client-sent `resume` intent header on the upgrade request).

**Depends on:** MetricsCollector interface (this branch), possibly protocol
changes in the `.github` repo.
