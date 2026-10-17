package support

import "testing"

// TestRedactText is the table-driven core of the redaction layer (docs/spec/13
// "Logging/privacy", docs/release.md "Hard rules"). Every row is a value a
// report, log line or config dump might plausibly carry; every result must be
// safe to ship to a third party.
func TestRedactText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// empty and inert input passes through
		{"empty stays empty", "", ""},
		{"plain text untouched", "storage OK, 12 rows counted", "storage OK, 12 rows counted"},

		// URLs: only the scheme survives
		{"https URL with host port path",
			"gateway at https://gw.lan.example:8443/v1/chat is down",
			"gateway at https://<redacted-host> is down"},
		{"http URL with private ip host",
			"dialing http://192.168.1.50:9000/health",
			"dialing http://<redacted-host>"},
		{"URL with userinfo",
			"see https://user:pass@gw.local/v1 now",
			"see https://<redacted-host> now"},
		{"URL at end of sentence",
			"see https://gw.local/v1",
			"see https://<redacted-host>"},
		{"ws scheme also reduced",
			"stream at wss://relay.lan/ws/1",
			"stream at wss://<redacted-host>"},

		// bearer tokens and authorization headers
		{"bearer token",
			"request failed: Authorization: Bearer sk-abc-1234567890abcdef",
			"request failed: Authorization: <redacted>"},
		{"bearer token lowercase",
			"sent bearer xyz.abc-def_012345 to the gateway",
			"sent bearer <redacted> to the gateway"},
		{"authorization token scheme",
			"sent Authorization: tok-abcdefgh123456 to the gateway",
			"sent Authorization: <redacted> to the gateway"},
		{"token query parameter",
			"curl http://gw/health?token=abc.def-0189XYZ",
			"curl http://<redacted-host>"},

		// provider-style API keys
		{"sk- key",
			"exported OPENAI key sk-ant-abc123456789abcdef for testing",
			"exported OPENAI key <redacted> for testing"},
		{"sk_ key",
			"stored sk_legacykey12345 in the env",
			"stored <redacted> in the env"},
		{"github personal token",
			"ghp_abcdefghijklmnopqrstuvwxyz123456",
			"<redacted>"},
		{"github fine-grained token",
			"github_pat_abcdefghijklmnopqrstuvwxyz1234",
			"<redacted>"},
		{"game-narrator api token",
			"the session token is gn_0123456789abcdef",
			"the session token is <redacted>"},

		// home paths become ~
		{"home path with tail",
			"log file at /home/david/dev/game-narrator/gn.log",
			"log file at ~/dev/game-narrator/gn.log"},
		{"home path bare",
			"home dir /home/david",
			"home dir ~"},
		{"non-home path untouched",
			"var dir /var/log/journal untouched",
			"var dir /var/log/journal untouched"},

		// private IPv4
		{"private ip class a",
			"connected to 10.0.0.1:8080",
			"connected to <redacted>:8080"},
		{"private ip class b high",
			"route via 172.31.255.255 failed",
			"route via <redacted> failed"},
		{"private ip class c",
			"the gateway is at 192.168.1.50",
			"the gateway is at <redacted>"},
		{"public ip kept",
			"8.8.8.8 and 113.10.0.1 are public",
			"8.8.8.8 and 113.10.0.1 are public"},
		{"loopback kept",
			"binds 127.0.0.1:8080 only",
			"binds 127.0.0.1:8080 only"},
		{"edge of class b kept",
			"172.32.0.1 is outside the private range",
			"172.32.0.1 is outside the private range"},
		{"not an ip",
			"version 1.2.3.4.5 is nonsense",
			"version 1.2.3.4.5 is nonsense"},

		// private IPv6
		{"link-local v6",
			"neigh fe80::1 on eth0",
			"neigh <redacted> on eth0"},
		{"ula v6",
			"peer fd12:3456:789a::4567 answered",
			"peer <redacted> answered"},
		{"global v6 kept",
			"2001:4860:4860::8888 is public",
			"2001:4860:4860::8888 is public"},

		// emails
		{"email address",
			"contact ops@example.com for help",
			"contact <redacted> for help"},
		{"email in angle brackets",
			"from <narrator+alerts@lan.example>",
			"from <<redacted>>"},

		// combinations
		{"everything at once",
			"https://gw.lan:8443/v1 Bearer gn_abcdef123456 from /home/david/logs 192.168.5.5 a@b.example",
			"https://<redacted-host> Bearer <redacted> from ~/logs <redacted> <redacted>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RedactText(c.in); got != c.want {
				t.Errorf("RedactText(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestRedactURL pins the config-value rule: a URL becomes scheme + redacted
// host, and a non-URL value is scrubbed as text.
func TestRedactURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://gateway.lan.local:8443/v1", "https://<redacted-host>"},
		{"http://10.1.2.3:9090", "http://<redacted-host>"},
		{"https://gw.example", "https://<redacted-host>"},
		{"not a url", "not a url"},
		{"gn_0123456789abcdef", "<redacted>"},
		{"", ""},
	}
	for _, c := range cases {
		if got := RedactValue(c.in); got != c.want {
			t.Errorf("RedactValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRedactConfig pins the config-tree rule: credential key values are
// replaced wholesale, every string value is scrubbed, and environment
// variable *names* (keys) survive.
func TestRedactConfig(t *testing.T) {
	in := map[string]any{
		"listen":            "127.0.0.1:8080",
		"gateway_url":       "https://gw.lan.local:8443/v1",
		"gateway_token":     "gn_0123456789abcdef",
		"GATEWAY_TOKEN_ENV": "MY_SECRET_TOKEN_VAR",
		"roles": []any{
			map[string]any{
				"role":     "fast_reasoning",
				"model":    "qwen3-8b",
				"base_url": "https://gw.lan.local:8443/v1",
				"token":    "sk-ant-abc123456789abcdef",
				"timeout":  "10s",
			},
		},
		"count": 3,
		"ok":    true,
	}
	got, ok := RedactConfig(in).(map[string]any)
	if !ok {
		t.Fatalf("RedactConfig returned %T, want map[string]any", got)
	}
	if v, _ := got["gateway_token"].(string); v != Redacted {
		t.Errorf("gateway_token = %q, want %q", v, Redacted)
	}
	if v, _ := got["gateway_url"].(string); v != "https://<redacted-host>" {
		t.Errorf("gateway_url = %q, want redacted URL", v)
	}
	// The env var *name* survives; only its value is the secret.
	if _, ok := got["GATEWAY_TOKEN_ENV"]; !ok {
		t.Error("env var name GATEWAY_TOKEN_ENV was dropped")
	}
	// The value named by the env var key is scrubbed as a string value: the
	// variable name itself is what the operator needs.
	roles, _ := got["roles"].([]any)
	if len(roles) != 1 {
		t.Fatalf("roles = %v", got["roles"])
	}
	role, _ := roles[0].(map[string]any)
	if v, _ := role["token"].(string); v != Redacted {
		t.Errorf("role token = %q, want %q", v, Redacted)
	}
	if v, _ := role["base_url"].(string); v != "https://<redacted-host>" {
		t.Errorf("role base_url = %q, want redacted URL", v)
	}
	if v, _ := role["model"].(string); v != "qwen3-8b" {
		t.Errorf("role model = %q, want untouched %q", v, "qwen3-8b")
	}
	if got["count"] != 3 || got["ok"] != true {
		t.Errorf("numbers and bools must pass through, got %v / %v", got["count"], got["ok"])
	}
}
