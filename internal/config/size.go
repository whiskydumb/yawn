package config

import (
	"fmt"
	"strconv"
	"strings"
)

// Size is a byte count parsed from YAML as "40G", "512M" or a plain number.
type Size int64

var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"TB", 1 << 40}, {"T", 1 << 40},
	{"GB", 1 << 30}, {"G", 1 << 30},
	{"MB", 1 << 20}, {"M", 1 << 20},
	{"KB", 1 << 10}, {"K", 1 << 10},
	{"B", 1},
}

// ParseSize reads a human readable size. An empty string yields 0.
func ParseSize(s string) (Size, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	up := strings.ToUpper(s)
	for _, u := range sizeUnits {
		if !strings.HasSuffix(up, u.suffix) {
			continue
		}
		num := strings.TrimSpace(up[:len(up)-len(u.suffix)])
		if num == "" {
			continue
		}
		v, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return 0, fmt.Errorf("cannot parse size %q: %w", s, err)
		}
		if v < 0 {
			return 0, fmt.Errorf("size %q is negative", s)
		}
		return Size(v * float64(u.mult)), nil
	}
	v, err := strconv.ParseInt(up, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot parse size %q: expected a number with a K/M/G/T suffix", s)
	}
	return Size(v), nil
}

func (s Size) String() string {
	const unit = 1024
	if s < unit {
		return fmt.Sprintf("%d B", int64(s))
	}
	div, exp := int64(unit), 0
	for n := int64(s) / unit; n >= unit && exp < 3; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(s)/float64(div), "KMGT"[exp])
}

func (s Size) Bytes() int64 { return int64(s) }

func (s *Size) UnmarshalYAML(unmarshal func(any) error) error {
	var raw any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case nil:
		*s = 0
	case int:
		*s = Size(v)
	case int64:
		*s = Size(v)
	case float64:
		*s = Size(v)
	case string:
		parsed, err := ParseSize(v)
		if err != nil {
			return err
		}
		*s = parsed
	default:
		return fmt.Errorf("size must be a string or a number, got %T", raw)
	}
	return nil
}
