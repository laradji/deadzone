// Package sample is a fixture exercising every ParseGodoc chunk shape:
// package overview, exported funcs, exported types with methods and
// constructors, const groups, and var groups. Used as a golden-file
// anchor for parse_test.go — keep this file deterministic.
package sample

// Greeting is the canonical hello-world string.
const Greeting = "hello"

// Operational limits exposed to callers.
const (
	// MaxItems caps the number of items per batch.
	MaxItems = 100
	// MaxRetries caps the per-request retry count.
	MaxRetries = 3
)

// Defaults is the Config used when nothing else applies.
var Defaults = Config{Verbose: false}

// Config controls runtime behavior of the sample package.
type Config struct {
	// Verbose toggles chatty logging.
	Verbose bool
}

// NewConfig returns a default-initialized Config.
func NewConfig() *Config {
	return &Config{}
}

// Apply mutates c per the supplied options.
func (c *Config) Apply(opts ...Option) {
	for _, o := range opts {
		o(c)
	}
}

// Option mutates a Config in place.
type Option func(*Config)

// WithVerbose returns an Option that enables verbose mode.
func WithVerbose() Option {
	return func(c *Config) { c.Verbose = true }
}

// Hello returns a polite greeting; it is a top-level package-scope
// func returning a builtin, so go/doc files it under Package.Funcs
// (not under any type).
func Hello(name string) string {
	return "hello, " + name
}

// unexportedHelper must not appear in any chunk.
func unexportedHelper() {}
