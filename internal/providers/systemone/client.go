package systemone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/dbirks/game-narrator/internal/providers"
)

// endpointPath is the System One route on the gateway (docs/spec/06 "Provider
// registry": the decision role speaks to the System One endpoint; the live
// contract verified against the collective gateway is <base> + /systemone,
// ADR-0021).
const endpointPath = "/systemone"

// Client talks to one System One deployment for one model. It mirrors the
// openai adapter's discipline: one request per Decide call, every failure
// classified as a *providers.Error, no retries (retry and circuit policy
// belong to the model router), and the request context owns the deadline —
// the runtime's 1500 ms decision budget (docs/spec/06 "Timeout and
// cancellation defaults") is a context deadline the caller sets, not a
// hidden transport timeout.
type Client struct {
	BaseURL string
	Token   string
	Model   string
	// HTTP is the transport used for requests. When nil, a client with no
	// overall timeout is used: per-request deadlines come from the context.
	HTTP *http.Client

	maxImages     int
	maxImageBytes int
	maxOptions    int
	doubt         *doubtLimiter
}

// ClientOption configures a Client at construction time.
type ClientOption func(*config)

type config struct {
	maxImages     int
	maxImageBytes int
	maxOptions    int
	doubtLimit    int
	doubtUncert   func(Decision) bool
}

// WithLimits overrides the local wire caps (all three must be positive to
// take effect; zero values fall back to the package defaults).
func WithLimits(maxImages, maxImageBytes, maxOptions int) ClientOption {
	return func(c *config) {
		c.maxImages, c.maxImageBytes, c.maxOptions = maxImages, maxImageBytes, maxOptions
	}
}

// WithDoubtLimit enables the consecutive-doubtful-decision budget
// (docs/spec/06 "Rate-limit consecutive doubtful decisions"): at most
// limit consecutive doubtful decisions are allowed per session key
// (Request.Key, or a shared bucket when the key is empty), after which
// Decide fails fast with a rate_limited error instead of hammering the
// GPU over one unreadable textbox. A confident answer resets the streak.
// uncertain defaults to DefaultUncertain.
func WithDoubtLimit(limit int, uncertain func(Decision) bool) ClientOption {
	return func(c *config) {
		c.doubtLimit = limit
		c.doubtUncert = uncertain
	}
}

// New returns a client for a base URL. baseURL is validated, not silently
// repaired, so a misconfigured env var fails at startup.
func New(baseURL, token, model string, opts ...ClientOption) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, providers.NewError(providers.KindUnsupportedCapability, 0, "systemone: BaseURL is empty for model "+model)
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("systemone: invalid base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("systemone: base URL scheme %q is not supported, use http or https", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("systemone: base URL has no host")
	}
	if u.User != nil {
		return nil, errors.New("systemone: credentials in the base URL are not allowed, use the token field")
	}
	cfg := config{maxImages: MaxImages, maxImageBytes: MaxImageBytes, maxOptions: MaxOptions}
	for _, opt := range opts {
		opt(&cfg)
	}
	c := &Client{
		BaseURL:       strings.TrimRight(u.String(), "/"),
		Token:         token,
		Model:         model,
		maxImages:     cfg.maxImages,
		maxImageBytes: cfg.maxImageBytes,
		maxOptions:    cfg.maxOptions,
	}
	if cfg.doubtLimit > 0 {
		uncertain := cfg.doubtUncert
		if uncertain == nil {
			uncertain = DefaultUncertain
		}
		c.doubt = newDoubtLimiter(cfg.doubtLimit, uncertain)
	}
	return c, nil
}

// NewFromRole builds a client for a configured decision role, rejecting roles
// that are not configured or that do not speak System One.
func NewFromRole(cfg providers.RoleConfig) (*Client, error) {
	if !cfg.Configured() {
		err := providers.NewError(providers.KindUnsupportedCapability, 0, "role "+string(cfg.Role)+" is NOT CONFIGURED")
		err.Role = cfg.Role
		return nil, err
	}
	if cfg.Protocol != providers.ProtocolSystemOne {
		err := providers.NewError(providers.KindUnsupportedCapability, 0,
			fmt.Sprintf("role %s uses protocol %q, which has no System One adapter", cfg.Role, cfg.Protocol))
		err.Role = cfg.Role
		err.Model = cfg.Model
		return nil, err
	}
	c, err := New(cfg.BaseURL, cfg.Token(), cfg.Model)
	if err != nil {
		var pe *providers.Error
		if errors.As(err, &pe) {
			pe.Role = cfg.Role
		}
		return nil, err
	}
	if cfg.Timeout > 0 {
		base := c.HTTP
		if base == nil {
			base = &http.Client{}
		}
		cloned := *base
		cloned.Timeout = cfg.Timeout
		c.HTTP = &cloned
	}
	return c, nil
}

// String renders the client identity for logs with the credential masked, so
// a stray %v of a client can never print a token or a gateway address.
func (c Client) String() string {
	return fmt.Sprintf("systemone.Client{BaseURL: <set>, Model: %q, Token: %q, HTTP: %s}",
		c.Model, mask(c.Token), transportLabel(c.HTTP))
}

// LogValue keeps the same guarantee for structured logging.
func (c Client) LogValue() any {
	token := ""
	if c.Token != "" {
		token = providers.MaskValue
	}
	return map[string]any{
		"model": c.Model,
		"token": token,
	}
}

func mask(secret string) string {
	if secret == "" {
		return ""
	}
	return providers.MaskValue
}

func transportLabel(h *http.Client) string {
	if h == nil {
		return "context-deadline-only"
	}
	return fmt.Sprintf("timeout=%s", h.Timeout)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// No transport-level Timeout: the context deadline is the single source of
	// truth so a superseded observation cancels promptly (docs/spec/13).
	return &http.Client{}
}

// limits returns the effective local wire caps.
func (c *Client) limits() limits {
	return limits{maxImages: c.maxImages, maxImageBytes: c.maxImageBytes, maxOptions: c.maxOptions}
}

// DefaultUncertain is the default doubtful-decision classifier for the
// consecutive-doubtful budget: a decision is doubtful when any choice answer
// picked the explicit "no answer" option (none/unknown) or any noul answer is
// below 0.5. It is deliberately permissive — the budget exists to stop
// hammering, not to decide (docs/spec/05 leaves the commitment policy to the
// runtime).
func DefaultUncertain(d Decision) bool {
	for _, a := range d.Answers {
		switch a.Type {
		case TypeChoice:
			if a.Choice == "none" || a.Choice == "unknown" {
				return true
			}
		case TypeNoul:
			if a.Noul < 0.5 {
				return true
			}
		}
	}
	return false
}

// doubtLimiter is the per-key token bucket for consecutive doubtful
// decisions: each key starts with doubtLimit tokens, every doubtful answer
// spends one, and a confident answer refills the bucket. When the bucket is
// empty the next call is refused before any request is sent.
type doubtLimiter struct {
	limit     int
	uncertain func(Decision) bool

	mu     sync.Mutex
	streak map[string]int // key -> consecutive doubtful decisions so far
}

const (
	defaultDoubtKey = ""
	// maxDoubtKeys bounds the tracker's memory: when the key space is larger
	// than this (a pathological caller), the whole tracker resets. Keys are
	// session-scoped in real use, so this is a hygiene cap, not policy.
	maxDoubtKeys = 8192
)

func newDoubtLimiter(limit int, uncertain func(Decision) bool) *doubtLimiter {
	return &doubtLimiter{limit: limit, uncertain: uncertain, streak: make(map[string]int)}
}

// admit refuses the call when the key has already spent its budget.
func (l *doubtLimiter) admit(key string) *providers.Error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.streak[key] >= l.limit {
		e := providers.NewError(providers.KindRateLimited, 0, fmt.Sprintf(
			"systemone: %d consecutive doubtful decisions for session %q; refusing to ask again (raise the doubt limit or wait for a fresh observation)",
			l.streak[key], key))
		e.Retryable = false
		return e
	}
	if len(l.streak) > maxDoubtKeys {
		l.streak = make(map[string]int)
	}
	return nil
}

// record spends a token on a doubtful answer or refills on a confident one.
func (l *doubtLimiter) record(key string, d Decision) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.uncertain(d) {
		l.streak[key]++
	} else {
		delete(l.streak, key)
	}
}

// Decide sends one System One request and returns the validated, normalised
// decision. The effective model is Request.Model or, when that is empty, the
// client's model. The doubt budget (when configured) applies under
// Request.Key.
func (c *Client) Decide(ctx context.Context, req Request) (Decision, error) {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = c.Model
	}
	req.Model = model
	if err := req.validate(c.limits()); err != nil {
		return Decision{}, err
	}
	if c.doubt != nil {
		if e := c.doubt.admit(req.Key); e != nil {
			e.Role, e.Model = providers.RoleDecision, model
			return Decision{}, e
		}
	}
	raw, err := req.encodeRequest()
	if err != nil {
		return Decision{}, err
	}
	latency, header, respBody, reqID, reqIDSource, callErr := c.doRaw(ctx, raw)
	if callErr != nil {
		return Decision{}, callErr
	}
	var payload responsePayload
	if err := json.Unmarshal(respBody, &payload); err != nil {
		e := providers.NewError(providers.KindBadSchema, http.StatusOK, "systemone: response is not valid JSON")
		e.Err, e.RequestID, e.Model = err, reqID, model
		e.Retryable = false
		return Decision{}, e
	}
	d, err := payload.toDecision(model, req, header, latency, reqID, reqIDSource)
	if err != nil {
		return Decision{}, err
	}
	if c.doubt != nil {
		c.doubt.record(req.Key, d)
	}
	return d, nil
}

// endpoint resolves <base> + /systemone.
func (c *Client) endpoint() (string, error) {
	u, err := url.Parse(strings.TrimSpace(c.BaseURL))
	if err != nil {
		return "", fmt.Errorf("systemone: invalid base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("systemone: base URL scheme %q is not supported", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + endpointPath
	return u.String(), nil
}
