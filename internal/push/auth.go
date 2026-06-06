package push

import (
	"context"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// OIDCClient builds an *http.Client that attaches a client-credentials bearer
// token. tokenURL is the issuer's token endpoint; audience is passed as an
// extra form value (Keycloak honours "audience").
func OIDCClient(ctx context.Context, tokenURL, clientID, clientSecret, audience string, timeout time.Duration) *http.Client {
	cfg := clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     tokenURL,
		EndpointParams: map[string][]string{
			"audience": {audience},
		},
	}
	base := &http.Client{Timeout: timeout}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, base)
	hc := cfg.Client(ctx)
	hc.Timeout = timeout
	return hc
}
