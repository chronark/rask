package cli

import (
	"context"
	"errors"
	"io"
	"strings"
)

var (
	// ErrNoArguments reports a Run call without a program name.
	ErrNoArguments = errors.New("no arguments provided")
)

// Action represents a command handler function.
type Action func(context.Context, *Command) error

// Command represents a CLI command with its configuration and runtime state.
type Command struct {
	// Name is the command name.
	Name string

	// Usage is the short description shown in help.
	Usage string

	// Description is the longer description shown in help.
	Description string

	// Examples are example invocations shown in help.
	Examples []string

	// Commands are subcommands routed by their name or aliases.
	Commands []*Command

	// Flags are the flags accepted by this command.
	Flags []Flag

	// Action is executed after parsing succeeds.
	Action Action

	// Aliases are alternative command names.
	Aliases []string

	// AcceptsArgs reports whether positional arguments are accepted.
	AcceptsArgs bool

	// DefaultActionNames are aliases for this command's own action.
	DefaultActionNames []string

	// Output receives generated help text.
	Output io.Writer

	// ErrorOutput receives parser diagnostics printed before help.
	ErrorOutput io.Writer

	// UnknownFlagHint replaces the generic available-flags text when set.
	UnknownFlagHint string

	args    []string
	flagMap map[string]Flag
	parent  *Command
}

// Args returns the non-flag arguments passed to the command.
func (c *Command) Args() []string {
	return append([]string(nil), c.args...)
}

// Bool returns false for absent flags because callers treat lookups as optional.
func (c *Command) Bool(name string) bool {
	if flag, ok := c.flagMap[name]; ok {
		if bf, ok := flag.(*BoolFlag); ok {
			return bf.Value()
		}
	}
	return false
}

// Int returns zero for absent flags so startup validation owns domain bounds.
func (c *Command) Int(name string) int {
	if flag, ok := c.flagMap[name]; ok {
		if intFlag, ok := flag.(*IntFlag); ok {
			return intFlag.Value()
		}
	}
	return 0
}

// String returns empty text for absent flags.
func (c *Command) String(name string) string {
	if flag, ok := c.flagMap[name].(*StringFlag); ok {
		return flag.Value()
	}
	return ""
}

// IsSet reports whether a flag was explicitly provided.
func (c *Command) IsSet(name string) bool {
	flag, ok := c.flagMap[name]
	return ok && flag.IsSet()
}

// Run executes the command with process-shaped arguments.
func (c *Command) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return ErrNoArguments
	}
	return c.parse(ctx, args[1:])
}

// SetOutput recursively sets the output writers for command help and errors.
func SetOutput(c *Command, out io.Writer, errOut io.Writer) {
	c.Output = out
	c.ErrorOutput = errOut
	for _, command := range c.Commands {
		SetOutput(command, out, errOut)
	}
}

func (c *Command) availableFlags() string {
	if len(c.Flags) == 0 {
		return "none"
	}
	names := make([]string, len(c.Flags))
	for i, flag := range c.Flags {
		names[i] = flag.Name()
	}
	return strings.Join(names, ", ")
}
