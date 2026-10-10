package server

import "testing"

func TestDSHLocalhostAndLegacyRedirects(t *testing.T) {
	for _, host := range []string{"localhost:3080", "127.0.0.1:3080"} {
		if got := rewriteDSHLocation("http://"+host+"/path?q=1#part", "codegate.example.test:8443"); got != "https://codegate.example.test:8443/path?q=1#part" {
			t.Fatal(got)
		}
	}
	for _, location := range []string{"/relative", "http://localhost:3080.evil/path", "http://localhost:3081/path", "http://external.test/path", "http://user@localhost:3080/path"} {
		if got := rewriteDSHLocation(location, "codegate.example.test"); got != location {
			t.Fatal("unexpected redirect rewrite")
		}
	}
}
