package cli

// Declarations follow the pinned Ruby lib/confctl/cli/app.rb. Usage is display
// metadata, not a new handler argument-count policy.
func switchOption(names []string, description string, def, negatable bool) OptionSpec {
	return OptionSpec{Key: names[len(names)-1], Names: names, Kind: Switch, Default: Bool(def), DefaultPresent: true, Negatable: negatable, Description: description}
}
func stringOption(names []string, description string) OptionSpec {
	return OptionSpec{Key: names[len(names)-1], Names: names, Kind: String, Metavar: "arg", Description: description}
}
func repeatedOption(names []string, description string) OptionSpec {
	o := stringOption(names, description)
	o.Multiple, o.Default, o.DefaultPresent = true, List(), true
	return o
}
func integerOption(names []string, description, metavar string, def int) OptionSpec {
	return OptionSpec{Key: names[len(names)-1], Names: names, Kind: Integer, Default: Int(def), DefaultPresent: true, Metavar: metavar, Description: description}
}
func filters() []OptionSpec {
	return []OptionSpec{repeatedOption([]string{"a", "attr"}, "Filter by attribute"), repeatedOption([]string{"t", "tag"}, "Filter by tag")}
}
func traceOption() OptionSpec {
	return switchOption([]string{"show-trace"}, "Enable traces in Nix", false, true)
}
func managedOption() OptionSpec {
	o := stringOption([]string{"managed"}, "Filter (un)managed machines")
	o.Choices = []string{"y", "yes", "n", "no", "a", "all"}
	return o
}

// These are also the current, bounded extension option-set declarations.
func MachineFilterOptions() []OptionSpec { return append([]OptionSpec{traceOption()}, filters()...) }
func ConfirmationOptions() []OptionSpec {
	return []OptionSpec{switchOption([]string{"y", "yes"}, "Assume the answer to confirmations is yes", false, true)}
}
func nixOptions() []OptionSpec {
	j := stringOption([]string{"j", "max-jobs"}, "Maximum number of build jobs (see nix build)")
	j.Metavar = "number"
	c := stringOption([]string{"cores"}, "Number of CPU cores to be used (see nix build)")
	c.Metavar = "number"
	return []OptionSpec{j, c}
}
func generationOption(description string) OptionSpec {
	return stringOption([]string{"g", "generation"}, description)
}
func localRemoteOptions() []OptionSpec {
	return []OptionSpec{
		switchOption([]string{"l", "local"}, "List local build generations", false, true),
		switchOption([]string{"r", "remote"}, "List remote machine generations", false, true),
	}
}
func inputChangeOptions(descriptions bool) []OptionSpec {
	d := []string{"", "", "", ""}
	if descriptions {
		d = []string{"Commit changes to git", "Include git log in the commit message", "Open $EDITOR with commit message", "Treat change as downgrade for changelog direction"}
	}
	return []OptionSpec{
		switchOption([]string{"commit"}, d[0], false, true),
		switchOption([]string{"changelog"}, d[1], true, true),
		switchOption([]string{"editor"}, d[2], true, true),
		switchOption([]string{"d", "downgrade"}, d[3], false, true),
	}
}
func options(sets ...[]OptionSpec) []OptionSpec {
	var out []OptionSpec
	for _, set := range sets {
		out = append(out, set...)
	}
	return out
}
func args(names ...string) []ArgumentSpec {
	out := make([]ArgumentSpec, len(names))
	for i, n := range names {
		out[i] = ArgumentSpec{Name: n}
	}
	return out
}
func required(names ...string) []ArgumentSpec {
	out := args(names...)
	for i := range out {
		out[i].Required = true
	}
	return out
}
func group(path []string, summary string) CommandSpec {
	return CommandSpec{Path: path, Summary: summary, ArgumentPolicy: GroupArguments}
}
func leaf(path []string, summary, usage string, arguments []ArgumentSpec, opts []OptionSpec) CommandSpec {
	policy := FreeForm
	if usage == "" {
		policy = NoArguments
	}
	return CommandSpec{Path: path, Summary: summary, Usage: usage, Arguments: arguments, ArgumentPolicy: policy, Options: opts,
		Availability: Availability{Mode: Unavailable, Reason: "Unavailable in confctl-go-prototype."}}
}

func BuiltinRegistry() *Registry {
	color := stringOption([]string{"c", "color"}, "Toggle color mode")
	color.DefaultPresent = true
	color.Default, color.Choices = Str("auto"), []string{"always", "never", "auto"}
	help := switchOption([]string{"h", "help"}, "Show this message", false, false)
	root := group(nil, "Nix deployment configuration management tool")
	root.Usage = "command [command options] [arguments...]"
	commands := []CommandSpec{
		root,
		{Path: []string{"help"}, Summary: "Shows a list of commands or help for one command", Usage: "[command ...]", Arguments: []ArgumentSpec{{Name: "command", Variadic: true}}, Availability: Availability{Mode: Available}},
		leaf([]string{"init"}, "Create a new configuration", "", nil, nil),
		leaf([]string{"add"}, "Add a new machine", "<name>", required("name"), nil),
		leaf([]string{"rename"}, "Rename an existing machine", "<old-name> <new-name>", required("old-name", "new-name"), nil),
		leaf([]string{"rediscover"}, "Update cluster machine list with contents of cluster/", "", nil, nil),
		group([]string{"inputs"}, "Manage flake inputs"),
		leaf([]string{"inputs", "ls"}, "List root-level flake inputs and their locked revisions", "[input-pattern]", args("input-pattern"), nil),
		leaf([]string{"inputs", "update"}, "Update flake inputs in flake.lock", "[input-name ...]", []ArgumentSpec{{Name: "input-name", Variadic: true}}, options(inputChangeOptions(true), []OptionSpec{switchOption([]string{"all"}, "Update all root inputs (dangerous; normally specify names)", false, true)})),
		leaf([]string{"inputs", "set"}, "Set a flake input to a specific revision", "<input-name> <rev>", required("input-name", "rev"), inputChangeOptions(true)),
		group([]string{"inputs", "channel"}, "Channel-based input operations"),
		leaf([]string{"inputs", "channel", "ls"}, "List channels and their role->input mapping (with locked revisions)", "[channel-pattern]", args("channel-pattern"), nil),
		leaf([]string{"inputs", "channel", "update"}, "Update inputs referenced by channels (optionally only a single role)", "<channels> [role]", []ArgumentSpec{{Name: "channels", Required: true}, {Name: "role"}}, inputChangeOptions(false)),
		leaf([]string{"inputs", "channel", "set"}, "Set inputs referenced by channels for a role to a specific revision", "<channels> <role> <rev>", required("channels", "role", "rev"), options(inputChangeOptions(false), []OptionSpec{switchOption([]string{"allow_shared"}, "Allow setting inputs shared with other channels/roles", false, true)})),
		group([]string{"inputs", "machine"}, "Machine-based input operations"),
		leaf([]string{"inputs", "machine", "update"}, "Update the input used by a specific machine for a specific role", "<machine> <role>", required("machine", "role"), inputChangeOptions(false)),
		leaf([]string{"inputs", "machine", "set"}, "Set the input used by a specific machine for a specific role to a revision", "<machine> <role> <rev>", required("machine", "role", "rev"), inputChangeOptions(false)),
	}
	ls := leaf([]string{"ls"}, "List configured machines", "[machine-pattern]", args("machine-pattern"), options(
		[]OptionSpec{traceOption(), managedOption(), switchOption([]string{"L", "list"}, "List possible attributes to output", false, false), stringOption([]string{"o", "output"}, "Select attributes to output"), switchOption([]string{"H", "hide-header"}, "Do not show the header", false, true)}, filters()))
	ls.Handler, ls.Availability = ListHandler, Availability{Mode: Available}
	commands = append(commands, ls)
	commands = append(commands,
		leaf([]string{"build"}, "Build target systems", "[machine-pattern]", args("machine-pattern"), options(MachineFilterOptions(), ConfirmationOptions(), nixOptions())),
		leaf([]string{"deploy"}, "Deploy target systems", "[machine-pattern [switch-action]]", args("machine-pattern", "switch-action"), options(MachineFilterOptions(), ConfirmationOptions(), []OptionSpec{
			generationOption("Deploy selected generation"),
			switchOption([]string{"i", "interactive"}, "Ask for confirmation before activation", false, true),
			switchOption([]string{"dry-activate-first"}, "Try to dry-activate before the real switch", false, true),
			switchOption([]string{"one-by-one"}, "Copy and deploy machines one by one", false, true),
			integerOption([]string{"max-concurrent-copy"}, "Max number of concurrent nix-copy-closure processes", "n", 5),
			switchOption([]string{"copy-only"}, "Do not activate copied closures", false, false),
			switchOption([]string{"enable-auto-rollback"}, "Enable auto-rollback", false, false),
			switchOption([]string{"disable-auto-rollback"}, "Disable auto-rollback", false, false),
			switchOption([]string{"reboot"}, "Reboot target systems after deployment", false, true),
			{Key: "wait-online", Names: []string{"wait-online"}, Kind: String, Default: Str("600"), DefaultPresent: true, Metavar: "arg", Description: "Wait for the machine to boot"},
		}, nixOptions(), []OptionSpec{switchOption([]string{"health-checks"}, "Toggle health checks", true, true), switchOption([]string{"keep-going"}, "Do not abourt on failed health checks", false, true)})),
		leaf([]string{"health-check"}, "Run machine health-checks", "[machine-pattern]", args("machine-pattern"), options(filters(), ConfirmationOptions(), []OptionSpec{integerOption([]string{"j", "max-jobs"}, "Maximum number of health-check jobs", "number", 5)})),
	)
	status := leaf([]string{"status"}, "Check machine status", "[machine-pattern]", args("machine-pattern"), options(filters(), ConfirmationOptions(), []OptionSpec{generationOption("Check status against selected generation")}))
	status.Handler = StatusNone
	status.Availability = Availability{Mode: Conditional, Option: "generation", Equals: Str("none"), Reason: "status requires --generation none; other modes are unavailable"}
	commands = append(commands, status,
		leaf([]string{"changelog"}, "Changelog between deployed and configured input roles", "[machine-pattern [role-pattern]]", args("machine-pattern", "role-pattern"), options(filters(), ConfirmationOptions(), []OptionSpec{generationOption("Show changelog against input roles from selected generation"), switchOption([]string{"d", "downgrade"}, "Show a changelog for downgrade", false, true), switchOption([]string{"v", "verbose"}, "Show full-length changelog descriptions", false, true), switchOption([]string{"p", "patch"}, "Show patches", false, true)}, nixOptions())),
		leaf([]string{"diff"}, "Diff between deployed and configured input roles", "[machine-pattern [role-pattern]]", args("machine-pattern", "role-pattern"), options(filters(), ConfirmationOptions(), []OptionSpec{generationOption("Show diff against input roles from selected generation"), switchOption([]string{"d", "downgrade"}, "Show a changelog for downgrade", false, true)}, nixOptions())),
		leaf([]string{"test-connection"}, "Test SSH connection", "[machine-pattern]", args("machine-pattern"), options([]OptionSpec{managedOption()}, filters(), ConfirmationOptions())),
		leaf([]string{"ssh"}, "Run command over SSH", "[machine-pattern [command [arguments...]]]", []ArgumentSpec{{Name: "machine-pattern"}, {Name: "command"}, {Name: "arguments", Variadic: true}}, options([]OptionSpec{managedOption()}, filters(), ConfirmationOptions(), []OptionSpec{switchOption([]string{"p", "parallel"}, "Run command in parallel on all machines at once", false, true), switchOption([]string{"g", "aggregate"}, "Aggregate identical command output", false, true), stringOption([]string{"i", "input-string"}, "Data passed to standard input"), stringOption([]string{"f", "input-file"}, "File passed to standard input")})),
		leaf([]string{"cssh"}, "Open ClusterSSH", "[machine-pattern]", args("machine-pattern"), options([]OptionSpec{managedOption()}, filters(), ConfirmationOptions())),
		group([]string{"generation"}, "Manage built machine generations"),
		leaf([]string{"generation", "ls"}, "List machine generations", "[machine-pattern [generation-pattern]]", args("machine-pattern", "generation-pattern"), options(filters(), localRemoteOptions())),
		leaf([]string{"generation", "rm"}, "Remove machine generations", "[machine-pattern [generation-pattern|old]]", args("machine-pattern", "generation-pattern|old"), options(filters(), localRemoteOptions(), []OptionSpec{switchOption([]string{"gc", "collect-garbage"}, "Run nix-collect-garbage to delete unreachable store paths", true, true), integerOption([]string{"max-concurrent-gc"}, "Max number of concurrent nix-collect-garbage processes", "n", 5)}, ConfirmationOptions())),
		leaf([]string{"generation", "rotate"}, "Auto-remove old machine generations", "[machine-pattern]", args("machine-pattern"), options(filters(), localRemoteOptions(), []OptionSpec{switchOption([]string{"gc", "collect-garbage"}, "Do not run the garbage collector if enabled in configuration", true, true), integerOption([]string{"max-concurrent-gc"}, "Max number of concurrent nix-collect-garbage processes", "n", 5)}, ConfirmationOptions())),
		leaf([]string{"collect-garbage"}, "Run nix-collect-garbage to deleted unreachable store paths", "[machine-pattern]", args("machine-pattern"), options(filters(), []OptionSpec{integerOption([]string{"max-concurrent-gc"}, "Max number of concurrent nix-collect-garbage processes", "n", 5)}, ConfirmationOptions())),
		group([]string{"gen-data"}, "Generate data files"),
		group([]string{"gen-data", "vpsadmin"}, "Fetch data from vpsAdmin"),
		leaf([]string{"gen-data", "vpsadmin", "all"}, "Generate all data files", "", nil, nil),
		leaf([]string{"gen-data", "vpsadmin", "containers"}, "Generate container data files", "", nil, nil),
		leaf([]string{"gen-data", "vpsadmin", "network"}, "Generate network data files", "", nil, nil),
	)
	r, err := NewRegistry([]OptionSpec{color, help}, commands)
	if err != nil {
		panic(err)
	} // A declaration error is a programmer error.
	return r
}
