package privatebroker

import "testing"

func TestParseAcceptsOnlyTheInvocationOwnedLoopbackBroker(t *testing.T) {
	endpoint, err := Parse("http://127.0.0.1:41234/go", "/go")
	if err != nil || endpoint == nil || endpoint.URL != "http://127.0.0.1:41234/go" || endpoint.Host != "127.0.0.1:41234" {
		t.Fatalf("Parse(loopback) = (%+v, %v), want the broker endpoint", endpoint, err)
	}
	if endpoint, err := Parse("http://[::1]:41234/npm", "/npm"); err != nil || endpoint == nil || endpoint.Host != "[::1]:41234" {
		t.Fatalf("Parse(ipv6 loopback) = (%+v, %v), want the broker endpoint", endpoint, err)
	}

	for _, raw := range []string{"", "https://go.putnami.dev", "https://registry.npmjs.org/"} {
		t.Run("direct:"+raw, func(t *testing.T) {
			if endpoint, err := Parse(raw, "/go"); err != nil || endpoint != nil {
				t.Fatalf("Parse(%q) = (%+v, %v), want the unchanged direct path", raw, endpoint, err)
			}
		})
	}

	for _, raw := range []string{
		"http://localhost:8080/go",
		"http://127.0.0.1/go",
		"http://127.0.0.1:8080/",
		"http://127.0.0.1:8080/npm",
		"http://127.0.0.1:8080/go/",
		"http://user@127.0.0.1:8080/go",
		"http://127.0.0.1:8080/go?scope=wide",
		"http://127.0.0.1:8080/go#fragment",
		"https://127.0.0.1:8080/go",
		"http://192.0.2.10:8080/go",
		"http://127.0.0.1:70000/go",
		" http://127.0.0.1:8080/go",
		"::not a url",
	} {
		t.Run("refused:"+raw, func(t *testing.T) {
			if endpoint, err := Parse(raw, "/go"); err == nil {
				t.Fatalf("Parse(%q) = %+v, want fail closed", raw, endpoint)
			}
		})
	}
}

func TestFromEnvReadsTheNamedVariable(t *testing.T) {
	t.Setenv("PUTNAMI_TEST_BROKER_URL", "http://127.0.0.1:8080/put")
	endpoint, err := FromEnv("PUTNAMI_TEST_BROKER_URL", "/put")
	if err != nil || endpoint == nil || endpoint.Host != "127.0.0.1:8080" {
		t.Fatalf("FromEnv() = (%+v, %v)", endpoint, err)
	}
	t.Setenv("PUTNAMI_TEST_BROKER_URL", "")
	if endpoint, err := FromEnv("PUTNAMI_TEST_BROKER_URL", "/put"); err != nil || endpoint != nil {
		t.Fatalf("FromEnv(unset) = (%+v, %v), want absent", endpoint, err)
	}
}
