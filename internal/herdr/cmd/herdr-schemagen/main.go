// Command herdr-schemagen generates Go types from herdr's API JSON schema
// (`herdr api schema --json`). See package schemagen for the mapping rules.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/WillemDeGroef/herdrcron/internal/herdr/schemagen"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "herdr-schemagen:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("herdr-schemagen", flag.ContinueOnError)
	schemaPath := fs.String("schema", "", "path to herdr-<version>.json (output of `herdr api schema --json`)")
	out := fs.String("out", "", "path of the Go file to write")
	pkg := fs.String("pkg", "", "package name of the generated file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *schemaPath == "" || *out == "" || *pkg == "" {
		return fmt.Errorf("-schema, -out and -pkg are required")
	}
	src, err := schemagen.GenerateFile(*schemaPath, *pkg)
	if err != nil {
		return err
	}
	return os.WriteFile(*out, src, 0o644)
}
