// Package o365client handles all interactions with the Microsoft Graph API,
// including message retrieval, attachment streaming, and mailbox management.
//
// OBJECTIVE:
// This package implements the authentication provider for the Microsoft Graph SDK.
// It manages the injection of bearer tokens into outgoing API requests.
//
// CORE FUNCTIONALITY:
// 1. Token Injection: Adds the "Authorization: Bearer <token>" header to all requests.
// 2. Provider Interface: Implements Kiota's AuthenticationProvider interface.
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
