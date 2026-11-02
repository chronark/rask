package cli

import (
	"fmt"
	"io"
	"strings"
)

func (c *Command) buildCommandPath() []string {
	path := []string{}
	for command := c; command != nil; command = command.parent {
		path = append([]string{command.Name}, path...)
	}
	return path
}

func (c *Command) buildFlagName(flag Flag) string {
	name := flag.Name()
	prefix := "--" + name
	if len(name) == 1 {
		prefix = "-" + name
	}
	if _, ok := flag.(*BoolFlag); ok {
		return prefix + " bool"
	}
	if _, ok := flag.(*IntFlag); ok {
		return prefix + " int"
	}
	if _, ok := flag.(*StringFlag); ok {
		return prefix + " string"
	}
	return prefix
}

func (c *Command) buildFlagUsage(flag Flag) string {
	return flag.Usage()
}

func (c *Command) output() io.Writer {
	if c.Output != nil {
		return c.Output
	}
	if c.parent != nil {
		return c.parent.output()
	}
	return io.Discard
}

func (c *Command) showCommands(out io.Writer) {
	if len(c.Commands) == 0 {
		return
	}
	fmt.Fprintln(out, "COMMANDS:")
	maxLen := c.maxCommandLength()
	for _, command := range c.Commands {
		name := command.Name
		if len(command.Aliases) > 0 {
			name += ", " + strings.Join(command.Aliases, ", ")
		}
		fmt.Fprintf(out, "   %-*s %s\n", maxLen+10, name, command.Usage)
	}
	fmt.Fprintf(out, "   %-*s %s\n\n", maxLen+10, "help, h", "Shows help for commands")
}

func (c *Command) showExamples(out io.Writer) {
	if len(c.Examples) == 0 {
		return
	}
	fmt.Fprintln(out, "EXAMPLES:")
	for _, example := range c.Examples {
		fmt.Fprintf(out, "   %s\n", example)
	}
	fmt.Fprintln(out)
}

func (c *Command) showFlags(out io.Writer) {
	if len(c.Flags) == 0 {
		return
	}
	fmt.Fprintln(out, "OPTIONS:")
	for _, flag := range c.Flags {
		fmt.Fprintf(out, "   %-25s %s\n", c.buildFlagName(flag), c.buildFlagUsage(flag))
	}
	fmt.Fprintln(out)
}

func (c *Command) showGlobalOptions(out io.Writer) {
	fmt.Fprintln(out, "GLOBAL OPTIONS:")
	fmt.Fprintf(out, "   %-25s %s\n\n", "--help, -h", "show help")
}

func (c *Command) showHeader(out io.Writer) {
	fmt.Fprintf(out, "NAME:\n   %s", c.Name)
	if c.Usage != "" {
		fmt.Fprintf(out, " - %s", c.Usage)
	}
	fmt.Fprintln(out)
	if c.Description == "" {
		fmt.Fprintln(out)
		return
	}
	fmt.Fprintln(out, "\nDESCRIPTION:")
	for _, line := range strings.Split(c.Description, "\n") {
		for _, wrapped := range wrapText(line, 77) {
			fmt.Fprintf(out, "   %s\n", wrapped)
		}
	}
	fmt.Fprintln(out)
}

func (c *Command) showHelp() {
	out := c.output()
	c.showHeader(out)
	c.showUsageLine(out)
	c.showCommands(out)
	c.showFlags(out)
	c.showGlobalOptions(out)
	c.showExamples(out)
}

func (c *Command) showUsageLine(out io.Writer) {
	fmt.Fprintf(out, "USAGE:\n   %s", strings.Join(c.buildCommandPath(), " "))
	if len(c.Flags) > 0 {
		fmt.Fprint(out, " [options]")
	}
	if len(c.Commands) > 0 {
		fmt.Fprint(out, " [command]")
	}
	if c.AcceptsArgs {
		fmt.Fprint(out, " [arguments...]")
	}
	fmt.Fprint(out, "\n\n")
}

func (c *Command) maxCommandLength() int {
	maxLen := len("help, h")
	for _, command := range c.Commands {
		name := command.Name
		if len(command.Aliases) > 0 {
			name += ", " + strings.Join(command.Aliases, ", ")
		}
		maxLen = max(maxLen, len(name))
	}
	return maxLen
}

func wrapText(text string, maxWidth int) []string {
	if len(text) <= maxWidth {
		return []string{text}
	}
	lines := []string{}
	for len(text) > maxWidth {
		cut := strings.LastIndex(text[:maxWidth], " ")
		if cut <= 0 {
			cut = maxWidth
		}
		lines = append(lines, text[:cut])
		text = strings.TrimLeft(text[cut:], " ")
	}
	return append(lines, text)
}
