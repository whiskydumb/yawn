package config

import (
	"fmt"
	"time"
)

// Duration reads a YAML value such as "90s" or "30m".
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var raw any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case nil:
		*d = 0
	case int:
		// a bare number is seconds, which is what people expect to be able to write
		*d = Duration(time.Duration(v) * time.Second)
	case float64:
		*d = Duration(time.Duration(v * float64(time.Second)))
	case string:
		if v == "" {
			*d = 0
			return nil
		}
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("cannot parse duration %q: %w", v, err)
		}
		if parsed < 0 {
			return fmt.Errorf("duration %q is negative", v)
		}
		*d = Duration(parsed)
	default:
		return fmt.Errorf("duration must be a string or a number, got %T", raw)
	}
	return nil
}
