package inframan

import "context"

// TokenSource gives the bearer token a request is signed with. It is asked
// before every request, so a source that renews its token renews it there.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// TokenFunc is a TokenSource of a function.
type TokenFunc func(ctx context.Context) (string, error)

// Token calls f.
func (f TokenFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// StaticToken is a source of one token, for development and tests: a
// person's OAuth access token, which `inframan login --url <console>` keeps
// in ~/.inframan/credentials.json (hostings.<console origin>.access_token,
// good for 15 minutes), or a service account's from the exchange. No CLI
// command prints one.
func StaticToken(token string) TokenSource {
	return TokenFunc(func(context.Context) (string, error) { return token, nil })
}

// Invalidator is a TokenSource that can be told the API refused a token it
// gave. When a call is answered 401, Call tells the source and sends the
// request once more, with the token the source gives next.
//
// Invalidate is given the token refused, so a source that has renewed it
// since (for another call) keeps the new one.
type Invalidator interface {
	TokenSource
	Invalidate(token string)
}
