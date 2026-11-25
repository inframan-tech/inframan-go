package inframan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ExchangePath is where an ID token of a verified service account is
// exchanged for an instance token, under the API's base.
const ExchangePath = "/v1/auth/service-accounts/token"

// MetadataHostEnv names the variable that moves the metadata server, as
// Google's own libraries read it: a host, or host:port, with no scheme.
const MetadataHostEnv = "GCE_METADATA_HOST"

// defaultMetadataHost is the metadata server's name on Google Cloud.
const defaultMetadataHost = "metadata.google.internal"

// renewBefore is how long before an instance token expires it is renewed.
const renewBefore = 2 * time.Minute

// GoogleWorkloadIdentity signs in as the Google service account the workload
// runs as (GKE Workload Identity, a VM's account): an ID token from the
// metadata server for the audience "inframan:<instanceID>", exchanged at
// <base>/v1/auth/service-accounts/token for an instance token, kept until two
// minutes before it expires (docs/plans/rak-platform.md, C2).
//
// base is the API's, as New takes it. The metadata server is
// metadata.google.internal unless GCE_METADATA_HOST names another, and it is
// reached directly, never through a proxy the environment names.
//
// The source is an Invalidator: a Client that is answered 401 exchanges once
// more.
func GoogleWorkloadIdentity(base, instanceID string) TokenSource {
	if instanceID == "" {
		return failingSource(errors.New("inframan: GoogleWorkloadIdentity needs the instance's ID: its ID tokens are for the audience inframan:<instance>"))
	}
	return ServiceAccountExchange(base, instanceID, MetadataIDTokens("inframan:"+instanceID))
}

// ServiceAccountExchange exchanges the ID tokens idTokens gives (any Google ID
// token of a verified service account, for the audience
// "inframan:<instanceID>") for instance tokens, as GoogleWorkloadIdentity
// does.
//
// instanceID may be "" where the backend serves one instance: the exchange is
// then asked with no instance named.
func ServiceAccountExchange(base, instanceID string, idTokens TokenSource) TokenSource {
	if idTokens == nil {
		return failingSource(errors.New("inframan: ServiceAccountExchange needs a source of ID tokens"))
	}
	u, err := url.Parse(strings.TrimSuffix(base, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return failingSource(fmt.Errorf("inframan: base URL %q is not an absolute URL", base))
	}
	return &exchangeSource{
		url:      u.String() + ExchangePath,
		instance: instanceID,
		idTokens: idTokens,
		http:     &http.Client{Timeout: 30 * time.Second},
		now:      time.Now,
		lock:     make(chan struct{}, 1),
	}
}

// MetadataIDTokens is a source of ID tokens from the metadata server, for
// audience, each with the account's email in it (format=full): what
// GoogleWorkloadIdentity exchanges. Each Token asks the server.
func MetadataIDTokens(audience string) TokenSource {
	h := &http.Client{
		Timeout: 10 * time.Second,
		// The metadata server is the node's: a proxy cannot reach it.
		Transport: &http.Transport{
			Proxy:       nil,
			DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		},
	}
	return TokenFunc(func(ctx context.Context) (string, error) {
		host := os.Getenv(MetadataHostEnv)
		if host == "" {
			host = defaultMetadataHost
		}
		q := url.Values{"audience": {audience}, "format": {"full"}}
		u := "http://" + host + "/computeMetadata/v1/instance/service-accounts/default/identity?" + q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", fmt.Errorf("inframan: metadata server: %w", err)
		}
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := h.Do(req)
		if err != nil {
			return "", fmt.Errorf("inframan: metadata server: %w", err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			return "", fmt.Errorf("inframan: metadata server: read the ID token: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			msg := strings.TrimSpace(string(raw))
			if len(msg) > 300 {
				msg = msg[:300]
			}
			return "", fmt.Errorf("inframan: metadata server answered %d for an ID token: %s", resp.StatusCode, msg)
		}
		tok := strings.TrimSpace(string(raw))
		if tok == "" {
			return "", errors.New("inframan: metadata server answered an empty ID token")
		}
		return tok, nil
	})
}

func failingSource(err error) TokenSource {
	return TokenFunc(func(context.Context) (string, error) { return "", err })
}

// exchangeSource keeps an instance token, renewing it by exchanging a new ID
// token. lock is a semaphore of one, so one exchange runs at a time and a
// caller waiting for it can give up with its context.
type exchangeSource struct {
	url      string
	instance string
	idTokens TokenSource
	http     *http.Client
	now      func() time.Time

	lock   chan struct{}
	token  string
	renew  time.Time // when to exchange again
	expiry time.Time // when token expires
	tokens int       // how many exchanges have succeeded (tests)
}

// exchangeAnswer is the exchange's answer.
type exchangeAnswer struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	InstanceID  string `json:"instance_id"`
}

// Token is the instance token kept, or a new one once it is due.
func (s *exchangeSource) Token(ctx context.Context) (string, error) {
	select {
	case s.lock <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-s.lock }()
	if s.token != "" && s.now().Before(s.renew) {
		return s.token, nil
	}
	tok, expiresIn, err := s.exchange(ctx)
	if err != nil {
		// A token due for renewal is good until it expires: a failed
		// renewal costs nothing until then.
		if s.token != "" && s.now().Before(s.expiry) {
			return s.token, nil
		}
		return "", err
	}
	if expiresIn <= 0 {
		expiresIn = defaultTokenLife
	}
	now := s.now()
	s.token = tok
	s.renew = now.Add(renewAfter(expiresIn))
	s.expiry = now.Add(expiresIn)
	s.tokens++
	return tok, nil
}

// Invalidate drops tok if it is the token kept.
func (s *exchangeSource) Invalidate(tok string) {
	s.lock <- struct{}{}
	defer func() { <-s.lock }()
	if s.token == tok {
		s.token = ""
	}
}

// defaultTokenLife is an instance token's life where the exchange's answer
// says nothing of it: the 15 minutes the API gives.
const defaultTokenLife = 15 * time.Minute

// renewAfter is how long a token that expires in expiresIn is kept: until two
// minutes before it expires, or half its life where it has less than four
// minutes.
func renewAfter(expiresIn time.Duration) time.Duration {
	if expiresIn < 2*renewBefore {
		return expiresIn / 2
	}
	return expiresIn - renewBefore
}

func (s *exchangeSource) exchange(ctx context.Context) (string, time.Duration, error) {
	idt, err := s.idTokens.Token(ctx)
	if err != nil {
		return "", 0, err
	}
	in := map[string]string{"id_token": idt}
	if s.instance != "" {
		in["instance_id"] = s.instance
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(raw))
	if err != nil {
		return "", 0, fmt.Errorf("inframan: service-account token exchange: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "inframan-go")
	if s.instance != "" {
		req.Header.Set(InstanceHeader, s.instance)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("inframan: service-account token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("inframan: service-account token exchange: read the answer: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", 0, errorOf(exchangeName, resp.StatusCode, resp.Header, body)
	}
	var ans exchangeAnswer
	if err := json.Unmarshal(body, &ans); err != nil {
		return "", 0, fmt.Errorf("inframan: service-account token exchange: decode the answer: %w", err)
	}
	if ans.AccessToken == "" {
		return "", 0, errors.New("inframan: service-account token exchange answered no access_token")
	}
	return ans.AccessToken, time.Duration(ans.ExpiresIn) * time.Second, nil
}

// exchangeName is what an *Error of the exchange names as its capability.
const exchangeName = "service_account_token"
