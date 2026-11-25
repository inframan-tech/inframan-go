# Changelog

## v0.1.0 — 2026-11-25

The first release.

- `Client`: `New`, `Call` by name, and a typed method for every capability of
  the instance's spec (254 in the spec this release was generated from), each
  with a params struct: required parameters as values, optional ones as
  pointers (`Ptr`), closed sets as typed constants.
- `Capabilities`, `CapabilityNamed` and `SpecHash`: what the client was
  generated with, for discovery and for comparing with an instance's
  `GET /api/v1/capabilities.json` (its `ETag`).
- Sign in as a person (`StaticToken`) or as a service account with no stored
  key: `GoogleWorkloadIdentity` (the metadata server's ID token, exchanged for
  an instance token, kept until two minutes before it expires) and
  `ServiceAccountExchange` (any source of ID tokens). A call answered 401
  exchanges once more.
- Retries: 429 and 503 on any call, 502, 504 and a request that got no answer
  on a call that changes nothing; up to three tries, `Retry-After` honoured
  (up to 30 s), backoff otherwise. `WithMaxTries`, `WithSleep`.
- `*Error` with the API's `Code`, `Status`, `RetryAfter` and `Body`;
  `CodeOf`, `StatusOf`, `ErrMissingParameter`.
- `Poll`, `Client.Pages` and a typed `…Pages` helper for each capability paged
  by a cursor or an offset.
- `cmd/inframan-gen`: the generator, from a spec file or an instance's URL.
