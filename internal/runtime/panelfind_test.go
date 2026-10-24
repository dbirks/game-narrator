package runtime

import "testing"

func TestPanelFindConfigOnByDefault(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{{"", true}, {"on", true}, {"off", false}, {"0", false}, {"FALSE", false}} {
		t.Setenv(EnvPanelFind, tc.env)
		if got := panelFindConfig().Enabled; got != tc.want {
			t.Errorf("%s=%q: Enabled=%v, want %v", EnvPanelFind, tc.env, got, tc.want)
		}
	}
}
