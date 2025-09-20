package o365client

import (
	"context"
	"errors"
	"net/url"

	abstractions "github.com/microsoft/kiota-abstractions-go"
)

// StaticTokenAuthenticationProvider authenticates requests with a static access token.
type StaticTokenAuthenticationProvider struct {
	accessToken string
}

// NewStaticTokenAuthenticationProvider creates a new StaticTokenAuthenticationProvider.
func NewStaticTokenAuthenticationProvider(accessToken string) (*StaticTokenAuthenticationProvider, error) {
	if accessToken == "" {
		return nil, errors.New("access token cannot be empty")
	}
	return &StaticTokenAuthenticationProvider{
		accessToken: accessToken,
	}, nil
}

// AuthenticateRequest adds the Authorization header to the request.
func (s *StaticTokenAuthenticationProvider) AuthenticateRequest(ctx context.Context, request *abstractions.RequestInformation, additionalAuthenticationContext map[string]interface{}) error {
	if request == nil {
		return errors.New("request cannot be nil")
	}
	if request.Headers == nil {
		request.Headers = abstractions.NewRequestHeaders()
	}
	request.Headers.Add("Authorization", "Bearer "+s.accessToken)
	return nil
}

// GetAuthorizationToken is required by the AccessTokenProvider interface
func (s *StaticTokenAuthenticationProvider) GetAuthorizationToken(ctx context.Context, request *url.URL, additionalAuthenticationContext map[string]interface{}) (string, error) {
	return s.accessToken, nil
}
