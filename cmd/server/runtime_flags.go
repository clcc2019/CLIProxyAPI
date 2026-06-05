package main

import (
	"flag"
	"fmt"
	"os"
)

type runtimeFlags struct {
	codexLogin        bool
	codexDeviceLogin  bool
	claudeLogin       bool
	noBrowser         bool
	kimiLogin         bool
	xaiLogin          bool
	tuiMode           bool
	standalone        bool
	localModel        bool
	oauthCallbackPort int
	configPath        string
	password          string
}

func parseRuntimeFlags() runtimeFlags {
	flags := runtimeFlags{}

	flag.BoolVar(&flags.codexLogin, "codex-login", false, "Login to Codex using OAuth")
	flag.BoolVar(&flags.codexDeviceLogin, "codex-device-login", false, "Login to Codex using device code flow")
	flag.BoolVar(&flags.claudeLogin, "claude-login", false, "Login to Claude using OAuth")
	flag.BoolVar(&flags.noBrowser, "no-browser", false, "Don't open browser automatically for OAuth")
	flag.IntVar(&flags.oauthCallbackPort, "oauth-callback-port", 0, "Override OAuth callback port (defaults to provider-specific port)")
	flag.BoolVar(&flags.kimiLogin, "kimi-login", false, "Login to Kimi using OAuth")
	flag.BoolVar(&flags.xaiLogin, "xai-login", false, "Login to xAI using OAuth")
	flag.StringVar(&flags.configPath, "config", DefaultConfigPath, "Configure File Path")
	flag.StringVar(&flags.password, "password", "", "")
	flag.BoolVar(&flags.tuiMode, "tui", false, "Start with terminal management UI")
	flag.BoolVar(&flags.standalone, "standalone", false, "In TUI mode, start an embedded local server")
	flag.BoolVar(&flags.localModel, "local-model", false, "Use embedded model catalog only, skip remote model fetching")

	flag.CommandLine.Usage = usageForFlags
	flag.Parse()
	return flags
}

func usageForFlags() {
	out := flag.CommandLine.Output()
	_, _ = fmt.Fprintf(out, "Usage of %s\n", os.Args[0])
	flag.CommandLine.VisitAll(func(f *flag.Flag) {
		if f.Name == "password" {
			return
		}
		s := fmt.Sprintf("  -%s", f.Name)
		name, usage := flag.UnquoteUsage(f)
		if name != "" {
			s += " " + name
		}
		if len(s) <= 4 {
			s += "\t"
		} else {
			s += "\n    "
		}
		if usage != "" {
			s += usage
		}
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
			s += fmt.Sprintf(" (default %s)", f.DefValue)
		}
		_, _ = fmt.Fprint(out, s+"\n")
	})
}
