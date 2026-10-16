package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// stub is a second adapter for registry tests. It reads nothing: the tests here
// are about registration and detection ordering, and a stub that returns a fixed
// confidence says exactly that. It is *not* a fake parse result presented as real
// (AGENTS.md "No simulation theater") — nothing in this file claims to test a
// parser.
type stub struct {
	m      Manifest
	called Confidence
}

func (s stub) Manifest() Manifest { return s.m }

func (s stub) Detect(ctx context.Context, name string, head []byte) Confidence {
	return s.detect(name, head)
}

func (s stub) detect(name string, head []byte) Confidence { return s.called }

func (s stub) Parse(ctx context.Context, r io.Reader, opts ParseOptions) (*ParseResult, error) {
	return nil, errors.New("stub adapter parses nothing")
}

func manifest(id, version string, formats ...string) Manifest {
	if len(formats) == 0 {
		formats = []string{"csv"}
	}
	return Manifest{
		ID: id, Version: version, SchemaVersion: 1,
		SupportedFormats:    formats,
		RequiredPermissions: []string{PermReadSource},
	}
}

func TestManifestValidate(t *testing.T) {
	good := manifest("generic", "1.0.0", "csv", "json")
	if err := good.Validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{name: "empty id", mutate: func(m *Manifest) { m.ID = "" }, want: "Manifest.id"},
		{name: "uppercase id", mutate: func(m *Manifest) { m.ID = "Generic" }, want: "Manifest.id"},
		{name: "path-ish id", mutate: func(m *Manifest) { m.ID = "../etc" }, want: "Manifest.id"},
		{name: "hidden id", mutate: func(m *Manifest) { m.ID = ".hidden" }, want: "Manifest.id"},
		{name: "not semver", mutate: func(m *Manifest) { m.Version = "1.0" }, want: "Manifest.version"},
		{name: "padded semver", mutate: func(m *Manifest) { m.Version = "01.0.0" }, want: "Manifest.version"},
		{name: "word version", mutate: func(m *Manifest) { m.Version = "latest" }, want: "Manifest.version"},
		{name: "zero schema", mutate: func(m *Manifest) { m.SchemaVersion = 0 }, want: "Manifest.schemaVersion"},
		{name: "no formats", mutate: func(m *Manifest) { m.SupportedFormats = nil }, want: "supportedFormats"},
		{name: "duplicate format", mutate: func(m *Manifest) { m.SupportedFormats = []string{"csv", "csv"} }, want: "twice"},
		{name: "blank format", mutate: func(m *Manifest) { m.SupportedFormats = []string{" csv"} }, want: "supportedFormats"},
		{name: "unknown permission", mutate: func(m *Manifest) { m.RequiredPermissions = []string{"execute_binaries"} }, want: "not a known permission"},
		{name: "bad locale", mutate: func(m *Manifest) { m.Locales = []string{"en_US"} }, want: "BCP-47"},
		{name: "too short locale", mutate: func(m *Manifest) { m.Locales = []string{"x"} }, want: "BCP-47"},
		{name: "trailing hyphen locale", mutate: func(m *Manifest) { m.Locales = []string{"en-"} }, want: "BCP-47"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := good
			m.SupportedFormats = append([]string(nil), good.SupportedFormats...)
			m.RequiredPermissions = append([]string(nil), good.RequiredPermissions...)
			tc.mutate(&m)
			err := m.Validate()
			if err == nil {
				t.Fatalf("manifest %v accepted (want a complaint about %s)", m, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
			if !errors.Is(err, ErrInvalidOptions) {
				t.Errorf("error does not wrap ErrInvalidOptions: %v", err)
			}
		})
	}

	// A semver with a prerelease and build tag is fine: an adapter under
	// development publishes 1.1.0-rc1+build.77 and must still state a version.
	prerelease := good
	prerelease.Version = "1.1.0-rc1+build.77"
	prerelease.Locales = []string{"en", "ja", "pt-BR", "zh-Hans"}
	if err := prerelease.Validate(); err != nil {
		t.Errorf("prerelease version rejected: %v", err)
	}
}

func TestManifestSupportsFormat(t *testing.T) {
	m := manifest("generic", "1.0.0", "csv", "json")
	if !m.SupportsFormat("csv") || !m.SupportsFormat("CSV") {
		t.Error("a declared format was not supported")
	}
	if !m.SupportsFormat("") || !m.SupportsFormat(FormatAuto) {
		t.Error("auto should always be supported; sniffing is not a format claim")
	}
	if m.SupportsFormat("bmg") {
		t.Error("an undeclared format was supported")
	}
}

func TestSemver(t *testing.T) {
	for _, ok := range []string{"0.0.0", "1.0.0", "12.34.56", "1.0.0-rc1", "1.0.0+build", "1.0.0-rc.1+b.2"} {
		if _, err := ParseSemver(ok); err != nil {
			t.Errorf("ParseSemver(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1", "1.0", "1.0.0.0", "v1.0.0", "1.0.x", "1.0.0-", "1.0.0-rc 1", "01.0.0"} {
		if _, err := ParseSemver(bad); err == nil {
			t.Errorf("ParseSemver(%q) accepted a malformed version", bad)
		}
	}
	v, err := ParseSemver("2.5.1-rc2+build.9")
	if err != nil {
		t.Fatal(err)
	}
	if v.Major != 2 || v.Minor != 5 || v.Patch != 1 || v.Pre != "rc2" {
		t.Errorf("parsed %v", v)
	}
	if v.String() != "2.5.1-rc2" {
		t.Errorf("String() = %q (build metadata is discarded, so the round trip is not exact and the doc says so)", v.String())
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if r.Len() != 0 {
		t.Fatalf("a new registry holds %d adapters", r.Len())
	}
	a := stub{m: manifest("alpha", "1.0.0", "csv"), called: ConfidentFormat}
	b := stub{m: manifest("beta", "1.0.0", "json"), called: ConfidentSniffed}
	if err := r.Register(a); err != nil {
		t.Fatalf("Register(alpha): %v", err)
	}
	if err := r.Register(b); err != nil {
		t.Fatalf("Register(beta): %v", err)
	}
	if err := r.Register(stub{m: manifest("alpha", "2.0.0", "csv")}); err == nil {
		t.Error("a colliding id was accepted; the row that recorded the first adapter would now read the second")
	} else if !errors.Is(err, ErrNotRegistered) {
		t.Errorf("collision error = %v, want ErrNotRegistered", err)
	}
	if err := r.Register(stub{m: manifest("", "1.0.0")}); err == nil {
		t.Error("an invalid manifest was registered")
	} else if !errors.Is(err, ErrNotRegistered) {
		t.Errorf("invalid manifest error = %v, want ErrNotRegistered", err)
	}
	if err := r.Register(nil); err == nil {
		t.Error("nil registered")
	}

	got, ok := r.Get("beta")
	if !ok || got.Manifest().ID != "beta" {
		t.Errorf("Get(beta) = %v, %v", got, ok)
	}
	if _, ok := r.Get("gamma"); ok {
		t.Error("an unregistered id was found")
	}
	if names := r.Adapters(); len(names) != 2 || names[0].ID != "alpha" || names[1].ID != "beta" {
		t.Errorf("Adapters() = %v, want registration order", names)
	}

	matches := r.Detect(context.Background(), "whatever", []byte("x"))
	if len(matches) != 2 {
		t.Fatalf("Detect returned %d matches", len(matches))
	}
	if matches[0].Manifest.ID != "alpha" || matches[1].Manifest.ID != "beta" {
		t.Errorf("Detect ordering = %s (%s), %s (%s); want best confidence first",
			matches[0].Manifest.ID, matches[0].Confidence, matches[1].Manifest.ID, matches[1].Confidence)
	}
	if _, ok := r.DetectOne(context.Background(), "nonsense", nil); !ok {
		t.Error("DetectOne found nothing where Detect found two")
	}
}

// TestRegistryDetectOrdersStably is the predictability requirement: an adapter
// chosen automatically must not change between runs because a map iterated in a
// different order (AGENTS.md "Keep runtime predictable").
func TestRegistryDetectOrdersStably(t *testing.T) {
	r := NewRegistry()
	for _, m := range []Manifest{
		manifest("zeta", "1.0.0"), manifest("mid", "1.0.0"), manifest("alpha", "1.0.0"),
	} {
		if err := r.Register(stub{m: m, called: 0.5}); err != nil {
			t.Fatal(err)
		}
	}
	var first []string
	for range 20 {
		var ids []string
		for _, match := range r.Detect(context.Background(), "x", []byte("y")) {
			ids = append(ids, match.Manifest.ID)
		}
		if first == nil {
			first = ids
			continue
		}
		if strings.Join(ids, ",") != strings.Join(first, ",") {
			t.Fatalf("ordering changed: %v then %v", first, ids)
		}
	}
	if strings.Join(first, ",") != "alpha,mid,zeta" {
		t.Errorf("equal-confidence adapters ordered %v, want id order", first)
	}
}

// TestRegistryDropsInvalidConfidence guards the one number an adapter returns
// about itself: NaN or 1.5 would sort unpredictably.
func TestRegistryDropsInvalidConfidence(t *testing.T) {
	r := NewRegistry()
	for i, c := range []Confidence{Confidence(0), Confidence(-1), Confidence(2), Confidence(nan())} {
		if err := r.Register(stub{m: manifest(fmt.Sprintf("zero%d", i), "1.0.0"), called: c}); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.Detect(context.Background(), "n", []byte("x")); len(got) != 0 {
		t.Errorf("Detect kept %d of 4 adapters that reported 0, -1, 2 and NaN", len(got))
	}
}

func TestParseOptionsNormalize(t *testing.T) {
	profile := games.NewID()

	got, err := ParseOptions{ProfileID: profile}.Normalize()
	if err != nil {
		t.Fatalf("a minimal options set was rejected: %v", err)
	}
	if got.MaxBytes != DefaultMaxBytes || got.MaxRows != DefaultMaxRows {
		t.Errorf("defaults not filled: %+v", got)
	}
	if got.DefaultKind != "" {
		t.Errorf("DefaultKind should stay empty for the adapter's own default, got %q", got.DefaultKind)
	}

	for name, opts := range map[string]ParseOptions{
		"no profile":   {},
		"bad profile":  {ProfileID: "not-a-ulid"},
		"bad source":   {ProfileID: profile, SourceID: "xyz"},
		"bad kind":     {ProfileID: profile, DefaultKind: games.TextKind("cutscene")},
		"bad language": {ProfileID: profile, Language: strings.Repeat("long", 20)},
	} {
		if _, err := opts.Normalize(); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !errors.Is(err, ErrInvalidOptions) {
			t.Errorf("%s: error %v does not wrap ErrInvalidOptions", name, err)
		}
	}

	clamped, err := ParseOptions{ProfileID: profile, MaxBytes: HardMaxBytes * 4, MaxRows: HardMaxRows * 3}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if clamped.MaxBytes != HardMaxBytes || clamped.MaxRows != HardMaxRows {
		t.Errorf("limits not clamped: %+v", clamped)
	}
	lowered, err := ParseOptions{ProfileID: profile, MaxBytes: 1024, MaxRows: 10}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if lowered.MaxBytes != 1024 || lowered.MaxRows != 10 {
		t.Errorf("a caller's tighter limit was overwritten: %+v", lowered)
	}
}

// nan returns a quiet NaN without tripping the compiler's constant division
// check; Confidence(0/0.0) is a compile error, and NaN is the value that must not
// be sortable.
func nan() float64 {
	x := 1.0
	return x / (x - x)
}

func TestConfidenceValid(t *testing.T) {
	for _, c := range []Confidence{ConfidentNone, ConfidentText, 1} {
		if !c.Valid() {
			t.Errorf("%v reported invalid", c)
		}
	}
	if Confidence(-0.01).Valid() || Confidence(1.01).Valid() || Confidence(nan()).Valid() {
		t.Error("an out-of-range confidence was accepted")
	}
	if ConfidentFormat.String() != "0.90" {
		t.Errorf("String() = %q", ConfidentFormat.String())
	}
}

func TestWarningAndStatsShapes(t *testing.T) {
	// The result types are what the onboarding UI binds; they carry no invariants
	// of their own beyond those games.Validate enforces on the messages, so the
	// test here is that the zero value is usable and JSON-shaped as documented.
	var res ParseResult
	if len(res.Messages) != 0 || res.Stats.Rows != 0 || res.Warnings != nil {
		t.Errorf("zero ParseResult is not empty: %+v", res)
	}
	if (ColumnMap{}).Empty() != true {
		t.Error("the zero ColumnMap should read as 'auto-map everything'")
	}
	if (ColumnMap{Text: "line"}).Empty() {
		t.Error("a mapped column is not empty")
	}
}
