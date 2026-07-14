package config

import (
	"fmt"
	"runtime"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from human strings like "2h" or
// "30m" in YAML and marshals back to the same form.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) {
	if d == 0 {
		return "", nil
	}
	return time.Duration(d).String(), nil
}

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// OSCommand is a command that may be a single string (used on all platforms) or
// a per-OS mapping {unix, windows}. The scalar form "auto" requests detection.
type OSCommand struct {
	Unix    string `yaml:"unix"`
	Windows string `yaml:"windows"`
}

func (c *OSCommand) UnmarshalYAML(value *yaml.Node) error {
	// Scalar form: a single command for all platforms.
	var s string
	if err := value.Decode(&s); err == nil {
		c.Unix = s
		c.Windows = s
		return nil
	}
	// Mapping form: per-OS commands. The keys are validated EXPLICITLY, because value.Decode
	// silently ignores unknown ones — the same fail-open trap KnownFields closes for the rest
	// of the config. A typo like `unx:` would otherwise leave the command empty (blocking with
	// requireGates) or, on Windows, fall through to the Unix command and misfire.
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("command must be a string or a {unix, windows} map")
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		key := value.Content[i].Value
		if key != "unix" && key != "windows" {
			return fmt.Errorf("command map: unknown key %q (expected `unix` or `windows`)", key)
		}
		if seen[key] {
			return fmt.Errorf("command map: duplicate key %q", key)
		}
		seen[key] = true
	}
	var m struct {
		Unix    string `yaml:"unix"`
		Windows string `yaml:"windows"`
	}
	if err := value.Decode(&m); err != nil {
		return fmt.Errorf("command must be a string or a {unix, windows} map: %w", err)
	}
	c.Unix = m.Unix
	c.Windows = m.Windows
	return nil
}

// Resolve returns the command for the current OS.
func (c OSCommand) Resolve() string {
	if runtime.GOOS == "windows" && c.Windows != "" {
		return c.Windows
	}
	return c.Unix
}
