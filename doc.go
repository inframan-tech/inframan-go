// Package inframan is a client for Inframan's API: every capability the
// console, the CLI and agents use, called by name or through a typed method
// generated from the instance's capability spec (capabilities.json).
//
// A client signs in as a person or as a service account. A person's token
// is the OAuth access token `inframan login --url <console>` keeps in
// ~/.inframan/credentials.json (StaticToken; good for 15 minutes, see the
// README). A service account, a non-human member of the instance's
// organization, signs in with its cloud identity and no stored key
// (GoogleWorkloadIdentity).
//
//	c, err := inframan.New("https://inframan-1234abcd.console.inframan.cloud/api",
//		inframan.WithInstance("1234abcd-…"),
//		inframan.WithTokenSource(inframan.GoogleWorkloadIdentity(base, instanceID)))
//	raw, err := c.GetApplication(ctx, inframan.GetApplicationParams{ApplicationID: id})
//
//	var app map[string]any
//	err = c.Call(ctx, "get_application", map[string]any{"application_id": id}, &app)
//
// Each capability has a method named for it (run_agent is RunAgent) taking
// its params struct: required parameters as values, optional ones as
// pointers (Ptr), closed sets as typed constants. A method answers the API's
// JSON as it came; an answer that is not 2xx is an *Error, its Code the API's
// (CodeOf).
//
// A call answered 429 or 503 is sent again, up to three tries in all, and one
// answered 502 or 504 too where it changes nothing; Retry-After is waited out.
// A service account's call answered 401 signs in again once.
//
// The package uses the standard library alone.
package inframan
