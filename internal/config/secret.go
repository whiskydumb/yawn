package config

import (
	"os"
	"regexp"
	"strings"
)

// envRef matches ${NAME} references inside a config value.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ResolveSecret turns a config value into the actual secret.
//
// A value may name an environment variable as ${NAME}; an empty value falls
// back to the first non-empty variable in fallbacks. Only the ${NAME} form is
// expanded, never a bare $NAME, because secrets legitimately contain dollars
// and mangling one would produce a confusing authentication failure rather
// than an error.
func ResolveSecret(value string, fallbacks ...string) string {
	value = strings.TrimSpace(value)
	if value != "" {
		return envRef.ReplaceAllStringFunc(value, func(ref string) string {
			name := ref[2 : len(ref)-1]
			return os.Getenv(name)
		})
	}
	for _, name := range fallbacks {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}
