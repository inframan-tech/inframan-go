# inframan-go

The Go client for [Inframan](https://inframan.cloud)'s API: every capability
the console, the CLI and agents use — create an application, run an agent,
read a release's pipeline — called by name or through a typed method.

The methods are generated from the instance's own capability spec
(`GET /api/v1/capabilities.json`), so the client says what the API serves:
each method carries the capability's documentation, its parameters, where each
goes and what permission it needs. The package uses the standard library
alone.

## Install

```sh
go get github.com/inframan-tech/inframan-go@v0.1.0
```

Go 1.26 or later.

## Sign in

A client is made for one instance's API: its console's address followed by
`/api`.

```go
c, err := inframan.New("https://inframan-1234abcd.console.inframan.cloud/api",
	inframan.WithInstance(instanceID),
	inframan.WithTokenSource(tokens))
```

### As a person

For development and short scripts. No CLI command prints a token for the
API; the CLI keeps one. On an instance that signs people in itself
(`AUTH_MODE=hosting`), `inframan login --url <console>` signs the person in
in the browser as the CLI's OAuth client, `inframan-cli`, and keeps the
session in `~/.inframan/credentials.json`, under `hostings`, keyed by the
console's origin:

- `access_token` is the person's, good for 15 minutes on every instance they
  may open at that console. A request names its instance (`WithInstance`);
  `instance` beside the token is the one `inframan instance use` chose.
- The CLI renews it with its refresh token when a command that calls the API
  finds it within a minute of expiring, so run one (`inframan instance list`)
  just before reading it.
- Never use the refresh token yourself. It is single-use and rotated, and a
  second use revokes the CLI's whole sign-in.

```sh
inframan login --url https://inframan-1234abcd.console.inframan.cloud
inframan instance list    # renews the access token when it is about to expire
export INFRAMAN_TOKEN=$(jq -r '.hostings[.active_hosting].access_token' ~/.inframan/credentials.json)
export INFRAMAN_INSTANCE_ID=$(jq -r '.hostings[.active_hosting].instance' ~/.inframan/credentials.json)
```

```go
c, err := inframan.New(base,
	inframan.WithInstance(os.Getenv("INFRAMAN_INSTANCE_ID")),
	inframan.WithTokenSource(inframan.StaticToken(os.Getenv("INFRAMAN_TOKEN"))))
```

On an instance whose people sign in through Inframan's central service, the
CLI keeps a per-instance token instead, under `instances.<instance id>.token`,
also good for 15 minutes. Anything that runs longer than that, or on its own,
signs in as a service account.

### As a service account

A program that runs on its own — a backend, a job — signs in as a **service
account**: a non-human member of the instance's organization, which signs in
with its Google Cloud identity and holds no key anywhere.

Once, an admin of the organization (or of the instance):

1. Adds the service account under Settings → Members, naming its Google
   service account (`<name>@<project>.iam.gserviceaccount.com`) and the role it
   holds on the instance (`instance_viewer`, `instance_developer` or
   `instance_admin`).
2. Runs the ceremony Inframan shows: makes the Google service account if it
   does not exist, grants themselves `roles/iam.serviceAccountTokenCreator` on
   it, and proves they control it by posting an ID token for the ceremony's
   one-hour challenge.
3. Lets the workload run as it: on GKE, Workload Identity
   (`roles/iam.workloadIdentityUser` for the workload's Kubernetes service
   account). For a service on Inframan, its own cloud identity is the account
   to name.

The workload then signs in with nothing stored:

```go
tokens := inframan.GoogleWorkloadIdentity(base, instanceID)
```

It asks the metadata server for an ID token for the audience
`inframan:<instance>`, exchanges it at `/api/v1/auth/service-accounts/token`
for an instance token, and keeps that until two minutes before it expires, one
exchange at a time. A call answered 401 exchanges once more. The metadata
server is `metadata.google.internal` unless `GCE_METADATA_HOST` names another.

With an ID token from anywhere else (another cloud's federation, a test):

```go
tokens := inframan.ServiceAccountExchange(base, instanceID, myIDTokens)
```

A service account may call only the capabilities offered to agents
(`CapabilityInfo.HasSurface(inframan.SurfaceAgent)`); any other answers 403
with the code `service_account_scope`. The full ceremony is in the docs at
[inframan.cloud/docs](https://inframan.cloud/docs), under service accounts.

## Call a capability

Each capability has a method named for it, taking its params struct:

```go
raw, err := c.RunAgent(ctx, inframan.RunAgentParams{
	ServiceID: builderID,
	Input:     inframan.Ptr("Build the app SPEC.md describes."),
})
```

Required parameters are values, optional ones pointers (`inframan.Ptr`), and a
parameter with a closed set of values takes typed constants
(`inframan.UpdateRevisionSettingModeFixed`). A method answers the API's JSON as
it came, a `json.RawMessage`, to decode into what the program reads of it:

```go
var out struct {
	Run struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"run"`
}
err = json.Unmarshal(raw, &out)
```

Or by name, decoding as it goes — for a capability the client was not
generated with, `RegisterRoute` it first:

```go
var app map[string]any
err := c.Call(ctx, "get_application", map[string]any{"application_id": id}, &app)
```

`Capabilities` lists every capability the client was generated with (name,
summary, method, path, permission, whether it changes state, its surfaces and
parameters), and `SpecHash` is the spec's hash: the `ETag` an instance's
`GET /api/v1/capabilities.json` answers while it serves the same capabilities.

## Waiting and pages

```go
err := inframan.Poll(ctx, 5*time.Second, func(ctx context.Context) (bool, error) {
	raw, err := c.GetAgentRun(ctx, inframan.GetAgentRunParams{ServiceID: agent, RunID: run})
	if err != nil {
		return false, err
	}
	// decode raw; return true once the run's status is one it ends with
	return finished(raw), nil
})
```

A capability paged by a cursor or an offset has a `…Pages` helper that sends
each answer's `next_cursor` or `next_offset` on, handing every page to a
callback (`StopPaging` stops early):

```go
err := c.ListAgentRunsPages(ctx, inframan.ListAgentRunsParams{ServiceID: agent}, func(page json.RawMessage) error {
	…
	return nil
})
```

## Errors

An answer that is not 2xx is an `*inframan.Error`: its `Status`, the API's
`Code` and `Message`, `RetryAfter` and the whole `Body`, for the fields a
refusal carries beyond them.

```go
_, err := c.RunAgent(ctx, p)
switch inframan.CodeOf(err) {
case "infrastructure_asleep": // wake it first
case "queue_full":            // ten runs already wait
}
if inframan.StatusOf(err) == http.StatusNotFound { … }
```

A call that leaves out a required parameter is refused before anything is
sent (`errors.Is(err, inframan.ErrMissingParameter)`). A sign-in that fails is
an error of the token source's, an `*Error` where the exchange refused it
(`not_verified`, `disabled`, `no_instance_access`).

## Retries

- 429 and 503 are sent again for any call: neither did the work.
- 502, 504 and a request that got no answer are sent again only for a call that
  changes nothing (a GET, or a read sent as a POST): a gateway that timed out
  may have let a change through.
- Up to three tries in all (`WithMaxTries`; 1 turns it off). `Retry-After`,
  in seconds or as a date, is waited out up to 30 seconds; an answer asking
  for longer is returned with its `RetryAfter` for the program to decide.
  Otherwise the wait is a backoff from half a second, with a random part.
- `WithSleep` replaces the wait, for tests.

## Generating

`zz_capabilities.go` is generated from the spec:

```sh
go run ./cmd/inframan-gen -spec spec/capabilities.json   # the committed spec
go run ./cmd/inframan-gen -spec https://<console domain>/api/v1/capabilities.json
```

In the Inframan monorepo the SDK is outside `go.work`: build, test and
generate with `GOWORK=off` (`GOWORK=off go generate ./...`). There the spec is
written from the API's own registry, in `inframan-backend`:

```sh
go run ./cmd/capability-spec -o ../sdk/go/spec/capabilities.json
```

and a test of each side fails when its file is stale.

## Examples

- [`examples/list-applications`](examples/list-applications): list an
  instance's applications, as a person or as a service account.
- [`examples/run-agent`](examples/run-agent): start a run of an on-demand
  agent and follow its transcript until it ends.

## License

Apache-2.0. See [LICENSE](LICENSE).
