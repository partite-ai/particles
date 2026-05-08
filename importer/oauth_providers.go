package importer

// oauthProvider holds the well-known endpoints for a named OAuth
// provider. Used to pre-fill URL prompts when the manifest's
// `capabilities.credentials.<name>.provider` hint matches. Users
// can always override the pre-fill at the prompt — the table is a
// convenience, not a constraint.
type oauthProvider struct {
	Auth      string
	Token     string
	Device    string
	Revoke    string
	UserAgent string
}

var oauthProviders = map[string]oauthProvider{
	"github": {
		Auth:   "https://github.com/login/oauth/authorize",
		Token:  "https://github.com/login/oauth/access_token",
		Device: "https://github.com/login/device/code",
		// GitHub doesn't expose a public revocation endpoint
		// for OAuth apps; revocation goes through the user's
		// settings page.
	},
	"google": {
		Auth:   "https://accounts.google.com/o/oauth2/v2/auth",
		Token:  "https://oauth2.googleapis.com/token",
		Device: "https://oauth2.googleapis.com/device/code",
		Revoke: "https://oauth2.googleapis.com/revoke",
	},
	"slack": {
		Auth:  "https://slack.com/oauth/v2/authorize",
		Token: "https://slack.com/api/oauth.v2.access",
	},
}

func providerPresets(name string) oauthProvider {
	if p, ok := oauthProviders[name]; ok {
		return p
	}
	return oauthProvider{}
}
