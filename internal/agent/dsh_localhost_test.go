package agent

import "testing"

func TestDSHStartupURLsUseLocalhost(t *testing.T) {
	for _, host := range []string{"localhost:3080", "127.0.0.1:3080"} {
		u, err := dshBootstrapURL("http://" + host + "/?token=fixture")
		if err != nil || u.Host != "localhost:3080" || u.Query().Get("token") != "fixture" {
			t.Fatalf("startup normalization failed: %v", err)
		}
	}
	for _, bad := range []string{"http://localhost:3081/?token=x", "http://localhost:3080.evil/?token=x", "http://example.com:3080/?token=x", "http://user@localhost:3080/?token=x", "https://localhost:3080/?token=x", "http://localhost:3080/"} {
		if _, err := dshBootstrapURL(bad); err == nil {
			t.Fatal("unsafe URL accepted")
		}
	}
}
