package support

import (
	"fmt"
	"os"
	"sort"

	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/doctor"
	"github.com/dbirks/game-narrator/internal/providers"
)

// envVarDoc records one environment variable the app reads: its name and
// whether it is set. Names survive redaction by design (the operator needs
// them to re-check the configuration); values never do.
type envVarDoc struct {
	Name string `json:"name"`
	Set  bool   `json:"set"`
}

// gatewayDoc is the shared gateway setting. The URL is reduced to scheme +
// redacted host and the token is presence-only.
type gatewayDoc struct {
	URL      string `json:"url"`
	TokenEnv string `json:"token_env"`
	TokenSet bool   `json:"token_set"`
}

// roleDoc is one model role's effective configuration, redacted.
type roleDoc struct {
	Role            string `json:"role"`
	Protocol        string `json:"protocol"`
	Model           string `json:"model"`
	BaseURL         string `json:"base_url"`
	TokenEnv        string `json:"token_env"`
	TokenSet        bool   `json:"token_set"`
	Timeout         string `json:"timeout"`
	ReasoningEffort string `json:"reasoning_effort"`
	State           string `json:"state"`
}

// configDoc is the config section of the bundle.
type configDoc struct {
	Note    string      `json:"note"`
	Gateway gatewayDoc  `json:"gateway"`
	Roles   []roleDoc   `json:"roles"`
	EnvVars []envVarDoc `json:"env_vars"`
}

// buildConfig resolves the effective role configuration from the environment
// (providers.LoadRoles — the same source of truth the runtime uses) and
// redacts it: URLs become scheme + redacted host, tokens become presence
// flags, and environment variable names are kept so the recipient knows what
// to check.
func buildConfig() ([]byte, error) {
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("support: resolve roles: %w", err)
	}

	// The gateway line reflects the shared endpoint: every role's BaseURL
	// derives from it (LoadRoles), and the TokenEnv is the resolved token
	// variable name — kept as a *name*, never its value.
	gw := gatewayDoc{}
	for _, role := range providers.AllRoles {
		cfg := roles[role]
		if gw.URL == "" && cfg.BaseURL != "" {
			gw.URL = RedactURL(cfg.BaseURL)
		}
		if gw.TokenEnv == "" {
			gw.TokenEnv = cfg.TokenEnv
		}
		if cfg.Token() != "" {
			gw.TokenSet = true
		}
	}
	if gw.TokenEnv == "" {
		gw.TokenEnv = providers.EnvGatewayToken
	}

	doc := configDoc{
		Note:    "effective model-role configuration, redacted: URLs are reduced to scheme + redacted host, every token/secret value is replaced by a sentinel, and environment variable names (not values) are kept so the configuration can be re-checked without exposing secrets.",
		Gateway: gw,
	}
	for _, role := range providers.AllRoles {
		cfg := roles[role]
		doc.Roles = append(doc.Roles, roleDoc{
			Role:            string(cfg.Role),
			Protocol:        string(cfg.Protocol),
			Model:           RedactText(cfg.Model),
			BaseURL:         RedactURL(cfg.BaseURL),
			TokenEnv:        cfg.TokenEnv,
			TokenSet:        cfg.Token() != "",
			Timeout:         cfg.Timeout.String(),
			ReasoningEffort: string(cfg.ReasoningEffort),
			State:           cfg.Status(),
		})
	}

	// Every variable the app reads, with its set/unset state: names only.
	names := providers.EnvKeyList()
	names = append(names, bootstrap.EnvListen, bootstrap.EnvDataDir, doctor.EnvGameImagePath)
	sort.Strings(names)
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		_, set := os.LookupEnv(n)
		doc.EnvVars = append(doc.EnvVars, envVarDoc{Name: n, Set: set})
	}

	return marshalDoc(doc)
}
