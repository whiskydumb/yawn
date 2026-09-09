package config

import "testing"

func TestResolveSecretExpandsBracedReference(t *testing.T) {
	t.Setenv("YAWN_TEST_KEY", "s3cret")
	if got := ResolveSecret("${YAWN_TEST_KEY}"); got != "s3cret" {
		t.Errorf("got %q, want the expanded value", got)
	}
}

func TestResolveSecretLeavesBareDollarAlone(t *testing.T) {
	t.Setenv("YAWN_TEST_KEY", "s3cret")
	// real keys contain dollars; mangling one would surface as a confusing
	// authentication failure rather than an error
	const literal = "abc$YAWN_TEST_KEY/def$xyz"
	if got := ResolveSecret(literal); got != literal {
		t.Errorf("got %q, want the literal value %q", got, literal)
	}
}

func TestResolveSecretFallsBackToEnv(t *testing.T) {
	t.Setenv("YAWN_FALLBACK_ONE", "")
	t.Setenv("YAWN_FALLBACK_TWO", "from-env")
	got := ResolveSecret("", "YAWN_FALLBACK_ONE", "YAWN_FALLBACK_TWO")
	if got != "from-env" {
		t.Errorf("got %q, want the first non-empty fallback", got)
	}
}

func TestResolveSecretPrefersExplicitValue(t *testing.T) {
	t.Setenv("YAWN_FALLBACK_ONE", "from-env")
	if got := ResolveSecret("explicit", "YAWN_FALLBACK_ONE"); got != "explicit" {
		t.Errorf("got %q, want the explicit value", got)
	}
}

func TestResolveSecretEmptyWhenNothingSet(t *testing.T) {
	if got := ResolveSecret("", "YAWN_DEFINITELY_UNSET_VARIABLE"); got != "" {
		t.Errorf("got %q, want an empty string", got)
	}
}

func TestResolveSecretExpandsMissingVariableToEmpty(t *testing.T) {
	if got := ResolveSecret("${YAWN_DEFINITELY_UNSET_VARIABLE}"); got != "" {
		t.Errorf("got %q, want an empty string for an unset variable", got)
	}
}
