package importer

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/oauth2"
)

// runDeviceCodeFlow drives the OAuth 2.0 Device Authorization
// Grant (RFC 8628). Posts to the device-code endpoint, surfaces
// the user-facing URL + code, and polls the token endpoint
// until the user authorizes — or the device code expires.
//
// No browser launch on this side: device flow is the headless
// option, intended for setups where the host can't open a local
// browser.
func runDeviceCodeFlow(ctx context.Context, p Prompter, cfg *oauth2.Config) (*oauth2.Token, error) {
	if cfg.Endpoint.DeviceAuthURL == "" {
		return nil, errors.New("device flow needs a device-authorization endpoint; supply one in the manifest's `deviceAuthUrl`")
	}
	auth, err := cfg.DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("device authorization request: %w", err)
	}

	p.Info("To authorize this app, visit:")
	p.Info("  " + auth.VerificationURI)
	p.Info("And enter the code:")
	p.Info("  " + auth.UserCode)
	if auth.VerificationURIComplete != "" {
		p.Info("Or open this pre-filled link directly:")
		p.Info("  " + auth.VerificationURIComplete)
	}
	p.Info("Waiting for authorization...")

	token, err := cfg.DeviceAccessToken(ctx, auth)
	if err != nil {
		return nil, fmt.Errorf("device token request: %w", err)
	}
	return token, nil
}
