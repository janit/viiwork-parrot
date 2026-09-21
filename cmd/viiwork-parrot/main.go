package main

import (
	"fmt"
	"os"
	"sort"
)

var version = "dev"

type command struct {
	name  string
	usage string
	run   func(args []string) error
}

var commands []command

func register(c command) { commands = append(commands, c) }

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	name := os.Args[1]
	if name == "version" {
		fmt.Println(version)
		return
	}
	for _, c := range commands {
		if c.name == name {
			if err := c.run(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "viiwork-parrot %s: %v\n", name, err)
				os.Exit(1)
			}
			return
		}
	}
	usage()
	os.Exit(2)
}

func usage() {
	sort.Slice(commands, func(i, j int) bool { return commands[i].name < commands[j].name })
	fmt.Fprintln(os.Stderr, "usage: viiwork-parrot <command> [flags]\n\ncommands:")
	fmt.Fprintf(os.Stderr, "  %-10s %s\n", "version", "print version")
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-10s %s\n", c.name, c.usage)
	}
}
