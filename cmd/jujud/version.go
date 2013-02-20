package main

import (
	"launchpad.net/gnuflag"
	"launchpad.net/juju-core/cmd"
	"launchpad.net/juju-core/version"
)

// VersionCommand is a cmd.Command that prints the current version.
type VersionCommand struct {
	out cmd.Output
}

func (v *VersionCommand) Info() *cmd.Info {
	return &cmd.Info{"version", "", "print the current version", ""}
}

func (v *VersionCommand) SetFlags(f *gnuflag.FlagSet) {
	v.out.AddFlags(f, "smart", cmd.DefaultFormatters)
}

func (v *VersionCommand) Init(args []string) error {
	return cmd.CheckEmpty(args)
}

func (v *VersionCommand) Run(ctxt *cmd.Context) error {
	return v.out.Write(ctxt, version.Current.String())
}
