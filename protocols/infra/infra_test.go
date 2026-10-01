package infra

import "testing"

func TestProtocolVersion(t *testing.T) {
	if ProtocolVersion != 2 {
		t.Errorf("ProtocolVersion = %d, want 2", ProtocolVersion)
	}
}

func TestCanonicalFilenames(t *testing.T) {
	if PerProjectManifestDir != "infra" {
		t.Errorf("PerProjectManifestDir = %q, want infra", PerProjectManifestDir)
	}
	if PerProjectManifestFilename != "requirements.json" {
		t.Errorf("PerProjectManifestFilename = %q, want requirements.json", PerProjectManifestFilename)
	}
	if AggregatedManifestDir != ".gen" {
		t.Errorf("AggregatedManifestDir = %q, want .gen", AggregatedManifestDir)
	}
	if AggregatedManifestFilename != "requirements.json" {
		t.Errorf("AggregatedManifestFilename = %q, want requirements.json", AggregatedManifestFilename)
	}
}

func TestValidEngines(t *testing.T) {
	wanted := []Engine{EnginePostgres, EngineMySQL, EngineSQLite, EngineFirestore}
	for _, e := range wanted {
		if !ValidEngines[e] {
			t.Errorf("ValidEngines missing %q", e)
		}
	}
	if ValidEngines["mongo"] {
		t.Error("ValidEngines should not include mongo")
	}
}

func TestFrameworkContributor(t *testing.T) {
	got := FrameworkContributor("go.putnami.dev/database")
	want := ContributorID("framework:go.putnami.dev/database")
	if got != want {
		t.Errorf("FrameworkContributor = %q, want %q", got, want)
	}
}

func TestContributorManual(t *testing.T) {
	if ContributorManual != "manual" {
		t.Errorf("ContributorManual = %q, want manual", ContributorManual)
	}
}

func TestContributorPrecedence(t *testing.T) {
	manual := ContributorPrecedence(ContributorManual)
	framework := ContributorPrecedence(FrameworkContributor("database"))
	if manual <= framework {
		t.Errorf("ContributorPrecedence(manual)=%d must outrank framework=%d", manual, framework)
	}
	// Same-rank contributors share a precedence; the merge falls back to
	// deterministic ordering for those.
	if ContributorPrecedence(FrameworkContributor("storage")) != framework {
		t.Error("two framework contributors should share precedence")
	}
}

func TestDefaultRuntime(t *testing.T) {
	rt := DefaultRuntime()
	if rt == nil || rt.Scaling == nil || rt.Ingress == nil {
		t.Fatalf("DefaultRuntime returned an incomplete value: %+v", rt)
	}
	if rt.Scaling.Max == nil || *rt.Scaling.Max != DefaultScalingMax {
		t.Errorf("scaling.max = %v, want %d", rt.Scaling.Max, DefaultScalingMax)
	}
	if rt.Scaling.Concurrency == nil || *rt.Scaling.Concurrency != DefaultScalingConcurrency {
		t.Errorf("scaling.concurrency = %v, want %d", rt.Scaling.Concurrency, DefaultScalingConcurrency)
	}
	if rt.Ingress.Public == nil || *rt.Ingress.Public != DefaultIngressPublic {
		t.Errorf("ingress.public = %v, want %v", rt.Ingress.Public, DefaultIngressPublic)
	}

	// Each call must return a fresh struct: callers mutating one runtime
	// must not affect another.
	other := DefaultRuntime()
	*other.Scaling.Max = 99
	if *rt.Scaling.Max == 99 {
		t.Error("DefaultRuntime returns aliased pointers; callers can mutate each other's state")
	}
}
