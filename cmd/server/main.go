package main

import (
	"fmt"
	"os"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	log "github.com/sirupsen/logrus"
	// automaxprocs sets GOMAXPROCS from the container CPU quota at startup.
	// In CPU-limited containers (k8s, ECS) Go would otherwise see all host
	// CPUs and over-schedule goroutines, paying measurable GC and tail-latency
	// cost. The blank import installs the init hook; no other code needed.
	_ "go.uber.org/automaxprocs"
)

var (
	Version           = "dev"
	Commit            = "none"
	BuildDate         = "unknown"
	DefaultConfigPath = ""
)

func init() {
	logging.SetupBaseLogger()
	buildinfo.Version = Version
	buildinfo.Commit = Commit
	buildinfo.BuildDate = BuildDate
}

func main() {
	printVersion()

	flags := parseRuntimeFlags()
	state, err := prepareStartup(flags)
	if err != nil {
		log.Errorf("startup failed: %v", err)
		os.Exit(1)
	}

	if err := dispatchCommand(flags, state); err != nil {
		log.Errorf("runtime failed: %v", err)
		os.Exit(1)
	}
}

func printVersion() {
	fmt.Printf(
		"CLIProxyAPI Version: %s, Commit: %s, BuiltAt: %s\n",
		buildinfo.Version,
		buildinfo.Commit,
		buildinfo.BuildDate,
	)
}
