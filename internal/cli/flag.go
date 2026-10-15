package cli

import (
	"fmt"
	"strconv"
)

var (
	// ErrInvalidBoolValue reports a boolean flag value that cannot be parsed.
	ErrInvalidBoolValue = fmt.Errorf("invalid boolean value")

	// ErrInvalidIntValue keeps numeric flag failures distinguishable from validation failures.
	ErrInvalidIntValue = fmt.Errorf("invalid integer value")
)

// Flag keeps help rendering and parsing independent of concrete flag storage.
type Flag interface {
	Name() string
	Usage() string
	Parse(value string) error
	IsSet() bool
}

type baseFlag struct {
	name  string
	usage string
	set   bool
}

// Name returns the flag name without dashes.
func (b *baseFlag) Name() string { return b.name }

// Usage returns the flag's help text.
func (b *baseFlag) Usage() string { return b.usage }

// IsSet reports whether the user explicitly provided this flag.
func (b *baseFlag) IsSet() bool { return b.set }

// BoolFlag exists because rask only needs explicit booleans, not a generic flag framework.
type BoolFlag struct {
	baseFlag
	value bool
}

// Parse sets the flag value from a string.
func (f *BoolFlag) Parse(value string) error {
	if value == "" {
		f.value = true
		f.set = true
		return nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidBoolValue, value)
	}
	f.value = parsed
	f.set = true
	return nil
}

// Value returns the current boolean value.
func (f *BoolFlag) Value() bool { return f.value }

// Bool creates a boolean flag without opening a broader option system.
func Bool(name string, usage string) *BoolFlag {
	return &BoolFlag{baseFlag: baseFlag{name: name, usage: usage}}
}

type IntFlag struct {
	baseFlag
	value int
}

// Parse records an explicit integer so callers can keep zero as their unset value.
func (f *IntFlag) Parse(value string) error {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidIntValue, value)
	}
	f.value = parsed
	f.set = true
	return nil
}

// Value exposes the parsed integer while absent flags still read as zero.
func (f *IntFlag) Value() int { return f.value }

// Int creates the only numeric flag shape rask accepts.
func Int(name string, usage string) *IntFlag {
	return &IntFlag{baseFlag: baseFlag{name: name, usage: usage}}
}

// StringFlag stores text options.
type StringFlag struct {
	baseFlag
	value string
}

// Parse records an explicit string value.
func (f *StringFlag) Parse(value string) error {
	f.value = value
	f.set = true
	return nil
}

// Value returns the current string value.
func (f *StringFlag) Value() string { return f.value }

// String creates a string flag with an empty default.
func String(name string, usage string) *StringFlag {
	return &StringFlag{baseFlag: baseFlag{name: name, usage: usage}}
}
