package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

func (c *Command) parse(ctx context.Context, args []string) error {
	c.initFlagMap()
	c.args = nil
	commandArgs := make([]string, 0, len(args))
	stopFlags := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" && !stopFlags {
			stopFlags = true
			continue
		}
		if stopFlags {
			commandArgs = append(commandArgs, arg)
			continue
		}
		if arg == "help" && i+1 < len(args) {
			return c.showCommandHelp(args[i+1])
		}
		if arg == "-h" || arg == "--help" || arg == "help" {
			c.showHelp()
			return nil
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			done, err := c.parsePositional(ctx, args, i, arg, &commandArgs)
			if done || err != nil {
				return err
			}
			continue
		}
		if err := c.parseFlag(args, &i); err != nil {
			return err
		}
	}
	c.args = commandArgs
	if len(commandArgs) > 0 && !c.AcceptsArgs {
		return fmt.Errorf("command %q does not accept positional arguments, got: %s\nAvailable flags: %s",
			c.Name, commandArgs[0], c.availableFlags())
	}
	if c.Action != nil {
		return c.Action(ctx, c)
	}
	if len(c.Commands) > 0 {
		c.showHelp()
	}
	return nil
}

func (c *Command) parsePositional(
	ctx context.Context,
	args []string,
	i int,
	arg string,
	commandArgs *[]string,
) (bool, error) {
	if c.isDefaultActionName(arg) {
		return false, nil
	}
	if subcmd := c.findSubcommand(arg); subcmd != nil {
		subcmd.parent = c
		return true, subcmd.parse(ctx, args[i+1:])
	}
	*commandArgs = append(*commandArgs, arg)
	return false, nil
}

func (c *Command) parseFlag(args []string, i *int) error {
	arg := args[*i]
	flagName := strings.TrimPrefix(strings.TrimPrefix(arg, "--"), "-")
	flagValue := ""
	hasValue := false
	if eqIndex := strings.Index(flagName, "="); eqIndex != -1 {
		flagValue = flagName[eqIndex+1:]
		flagName = flagName[:eqIndex]
		hasValue = true
	}
	flag, exists := c.flagMap[flagName]
	if !exists {
		if c.UnknownFlagHint != "" {
			return fmt.Errorf("unknown flag: %s\n%s", flagName, c.UnknownFlagHint)
		}
		return fmt.Errorf("unknown flag: %s\nAvailable flags: %s", flagName, c.availableFlags())
	}
	if bf, ok := flag.(*BoolFlag); ok {
		if hasValue {
			return bf.Parse(flagValue)
		}
		return bf.Parse("")
	}
	if !hasValue {
		if *i+1 >= len(args) {
			return fmt.Errorf("missing value for flag: %s", flagName)
		}
		(*i)++
		flagValue = args[*i]
	}
	return flag.Parse(flagValue)
}

func (c *Command) findSubcommand(name string) *Command {
	for _, command := range c.Commands {
		if command.Name == name || slices.Contains(command.Aliases, name) {
			return command
		}
	}
	return nil
}

func (c *Command) initFlagMap() {
	c.flagMap = make(map[string]Flag, len(c.Flags))
	for _, flag := range c.Flags {
		c.flagMap[flag.Name()] = flag
	}
}

func (c *Command) isDefaultActionName(name string) bool {
	return name == c.Name || slices.Contains(c.DefaultActionNames, name)
}

func (c *Command) showCommandHelp(name string) error {
	subcmd := c.findSubcommand(name)
	if subcmd == nil {
		return fmt.Errorf("unknown command: %s", name)
	}
	subcmd.parent = c
	subcmd.showHelp()
	return nil
}
