package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads, defaults, resolves secrets for, and validates the configuration
// at path.
//
// Every failure here is fatal by design. The Gateway must never start with a
// configuration it cannot fully validate, because the alternative is
// discovering a broken upstream or an unauthenticated lifecycle API on the
// first real request.
//
// Unknown keys are rejected rather than ignored: a misspelled key such as
// "trusted_proxys" would otherwise silently leave the default in place, which
// is exactly the kind of misconfiguration that looks like it works.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer func() { _ = f.Close() }()

	cfg := Default()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	if err := cfg.resolveSecrets(); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s:\n%w", path, err)
	}

	return cfg, nil
}

// resolveSecrets replaces the environment variable *names* held in the file
// with their values.
//
// A missing, empty, or whitespace-only variable is a startup failure, never a
// silent empty credential. The failure messages name the variable but never
// its value, so a startup error is safe to paste into a ticket.
func (c *Config) resolveSecrets() error {
	var errs []error

	if c.Registry.DSNEnv == "" {
		errs = append(errs, errors.New("registry.dsn_env must name the environment variable holding the registry DSN"))
	} else if dsn, ok := lookupSecret(c.Registry.DSNEnv); !ok {
		errs = append(errs, fmt.Errorf("environment variable %s (registry.dsn_env) is unset or empty", c.Registry.DSNEnv))
	} else {
		c.Registry.DSN = dsn
	}

	// Only token mode has secrets to resolve. An unrecognised mode is left
	// alone here so that Validate can report it with the list of valid
	// values, rather than this function failing with a less useful message.
	if c.InternalAuth.Mode == AuthModeToken {
		seen := make(map[string]bool, len(c.InternalAuth.Tokens))
		for i := range c.InternalAuth.Tokens {
			t := &c.InternalAuth.Tokens[i]
			label := t.Service
			if label == "" {
				label = fmt.Sprintf("#%d", i)
			}

			if t.TokenEnv == "" {
				errs = append(errs, fmt.Errorf("internal_auth.tokens[%s]: token_env is required", label))
				continue
			}

			tok, ok := lookupSecret(t.TokenEnv)
			if !ok {
				errs = append(errs, fmt.Errorf("environment variable %s (service token for %q) is unset or empty", t.TokenEnv, label))
				continue
			}
			if len(tok) < minServiceTokenLength {
				errs = append(errs, fmt.Errorf("service token for %q is %d characters; minimum is %d",
					label, len(tok), minServiceTokenLength))
				continue
			}

			t.Token = tok
			seen[t.Service] = true
		}

		for svc := range seen {
			count := 0
			for i := range c.InternalAuth.Tokens {
				if c.InternalAuth.Tokens[i].Service == svc {
					count++
				}
			}
			if count > 1 {
				errs = append(errs, fmt.Errorf("internal_auth: service %q is configured %d times; each service must have exactly one token", svc, count))
			}
		}
	}

	return errors.Join(errs...)
}

// lookupSecret reads an environment variable, treating an unset, empty, or
// whitespace-only value as absent.
func lookupSecret(env string) (string, bool) {
	v, ok := os.LookupEnv(env)
	if !ok || strings.TrimSpace(v) == "" {
		return "", false
	}
	return v, true
}
