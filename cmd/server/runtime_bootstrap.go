package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	configaccess "github.com/router-for-me/CLIProxyAPI/v7/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cmd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tui"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type startupState struct {
	cfg            *config.Config
	configFilePath string
	configReady    bool
	cloudDeploy    bool
	loginOptions   *cmd.LoginOptions
}

type ioState struct {
	stdout    *os.File
	stderr    *os.File
	logOutput io.Writer
	devNull   *os.File
}

func prepareStartup(flags runtimeFlags) (startupState, error) {
	ctx, err := resolveStartupContext()
	if err != nil {
		return startupState{}, err
	}
	loadDotEnvFile(ctx.workdir)

	result, err := loadConfigResult(flags, ctx, loadStoreSettings(ctx))
	if err != nil {
		return startupState{}, err
	}

	cfg := result.cfg
	if cfg == nil {
		cfg = &config.Config{}
	}
	if err := configureRuntimeConfig(cfg); err != nil {
		return startupState{}, err
	}

	managementasset.SetCurrentConfig(cfg)
	sdkAuth.RegisterTokenStore(resolveTokenStore(result.tokenStore))
	configaccess.Register(&cfg.SDKConfig)

	return startupState{
		cfg:            cfg,
		configFilePath: result.configFilePath,
		configReady:    detectConfigReady(ctx.cloudDeploy, result.configFilePath, cfg),
		cloudDeploy:    ctx.cloudDeploy,
		loginOptions: &cmd.LoginOptions{
			NoBrowser:    flags.noBrowser,
			CallbackPort: flags.oauthCallbackPort,
		},
	}, nil
}

func configureRuntimeConfig(cfg *config.Config) error {
	usage.SetStatisticsEnabled(cfg.UsageStatisticsEnabled)
	usage.SetDetailRetentionLimit(cfg.UsageDetailRetentionLimit)
	usage.SetClientAPIKeyQuotaModelPrices(cfg.ModelPrices)
	coreauth.SetQuotaCooldownDisabled(cfg.DisableCooling)

	if err := logging.ConfigureLogOutput(cfg); err != nil {
		return fmt.Errorf("failed to configure log output: %w", err)
	}
	log.Infof(
		"CLIProxyAPI Version: %s, Commit: %s, BuiltAt: %s",
		buildinfo.Version,
		buildinfo.Commit,
		buildinfo.BuildDate,
	)

	util.SetLogLevel(cfg)
	resolvedAuthDir, err := util.ResolveAuthDir(cfg.AuthDir)
	if err != nil {
		return fmt.Errorf("failed to resolve auth directory: %w", err)
	}
	cfg.AuthDir = resolvedAuthDir
	return nil
}

func resolveTokenStore(store coreauth.Store) coreauth.Store {
	if store != nil {
		return store
	}
	return sdkAuth.NewFileTokenStore()
}

func detectConfigReady(cloudDeploy bool, configFilePath string, cfg *config.Config) bool {
	if !cloudDeploy {
		return true
	}
	info, err := os.Stat(configFilePath)
	if err != nil {
		log.Info("Cloud deploy mode: No configuration file detected; standing by for configuration")
		return false
	}
	if info.IsDir() {
		log.Info("Cloud deploy mode: Config path is a directory; standing by for configuration")
		return false
	}
	if cfg.Port == 0 {
		log.Info("Cloud deploy mode: Configuration file is empty or invalid; standing by for valid configuration")
		return false
	}
	log.Info("Cloud deploy mode: Configuration file detected; starting service")
	return true
}

func dispatchCommand(flags runtimeFlags, state startupState) error {
	switch {
	case flags.vertexImport != "":
		cmd.DoVertexImport(state.cfg, flags.vertexImport, flags.vertexImportPrefix)
	case flags.login:
		cmd.DoLogin(state.cfg, flags.projectID, state.loginOptions)
	case flags.antigravityLogin:
		cmd.DoAntigravityLogin(state.cfg, state.loginOptions)
	case flags.codexLogin:
		cmd.DoCodexLogin(state.cfg, state.loginOptions)
	case flags.codexDeviceLogin:
		cmd.DoCodexDeviceLogin(state.cfg, state.loginOptions)
	case flags.claudeLogin:
		cmd.DoClaudeLogin(state.cfg, state.loginOptions)
	case flags.kimiLogin:
		cmd.DoKimiLogin(state.cfg, state.loginOptions)
	case flags.kiroLogin:
		cmd.DoKiroLogin(state.cfg, state.loginOptions)
	case flags.kiroImport:
		cmd.DoKiroImport(state.cfg, state.loginOptions)
	case flags.kiroRefresh:
		cmd.DoKiroRefresh(state.cfg, state.loginOptions)
	default:
		return runApplication(flags, state)
	}
	return nil
}

func runApplication(flags runtimeFlags, state startupState) error {
	if state.cloudDeploy && !state.configReady {
		cmd.WaitForCloudDeploy()
		return nil
	}
	if flags.localModel && (!flags.tuiMode || flags.standalone) {
		log.Info("Local model mode: using embedded model catalog, remote model updates disabled")
	}
	if flags.tuiMode {
		return runTUI(flags, state)
	}
	startSupportServices(state.configFilePath, flags.localModel)
	return cmd.StartService(state.cfg, state.configFilePath, flags.password)
}

func runTUI(flags runtimeFlags, state startupState) error {
	if flags.standalone {
		return runStandaloneTUI(state, flags.password, flags.localModel)
	}
	if err := tui.Run(state.cfg.Port, flags.password, nil, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
		return err
	}
	return nil
}

func startSupportServices(configFilePath string, localModel bool) {
	// Tie background updaters to a SIGINT/SIGTERM-bound context so they exit
	// promptly on shutdown. Previously each updater used context.Background()
	// and could outlive the main service, leaking goroutines and (for the
	// management asset auto-updater) keeping a polling http.Client alive past
	// process drain in k8s rolling restarts.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	supportServicesShutdown.Store(&cancel)

	managementasset.StartAutoUpdater(ctx, configFilePath)
	misc.StartAntigravityVersionUpdater(ctx)
	if !localModel {
		registry.StartModelsUpdater(ctx)
	}
}

// supportServicesShutdown holds the cancel func for the support-services
// context so test harnesses (or future graceful-shutdown wiring) can stop
// background updaters explicitly. Stored as a pointer so swapping in a no-op
// is race-free via atomic.Pointer.
var supportServicesShutdown atomic.Pointer[context.CancelFunc]

func runStandaloneTUI(state startupState, password string, localModel bool) error {
	startSupportServices(state.configFilePath, localModel)

	hook := tui.NewLogHook(2000)
	hook.SetFormatter(&logging.LogFormatter{})
	log.AddHook(hook)

	ioState, err := suppressProcessIO()
	if err == nil {
		defer ioState.restore()
	}

	password = effectiveTUISecret(password)
	cancel, done := cmd.StartServiceBackground(state.cfg, state.configFilePath, password)

	if !waitForEmbeddedServer(state.cfg.Port, password) {
		if ioState != nil {
			ioState.restore()
		}
		cancel()
		<-done
		return fmt.Errorf("embedded server is not ready")
	}

	runErr := tui.Run(state.cfg.Port, password, hook, ioState.output())
	if ioState != nil {
		ioState.restore()
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", runErr)
	}

	cancel()
	<-done
	return runErr
}

func suppressProcessIO() (*ioState, error) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}

	state := &ioState{
		stdout:    os.Stdout,
		stderr:    os.Stderr,
		logOutput: log.StandardLogger().Out,
		devNull:   devNull,
	}
	log.SetOutput(io.Discard)
	os.Stdout = devNull
	os.Stderr = devNull
	return state, nil
}

func (s *ioState) restore() {
	if s == nil {
		return
	}
	os.Stdout = s.stdout
	os.Stderr = s.stderr
	log.SetOutput(s.logOutput)
	if s.devNull != nil {
		_ = s.devNull.Close()
		s.devNull = nil
	}
}

func (s *ioState) output() io.Writer {
	if s == nil || s.stdout == nil {
		return os.Stdout
	}
	return s.stdout
}

func effectiveTUISecret(password string) string {
	if password != "" {
		return password
	}
	return fmt.Sprintf("tui-%d-%d", os.Getpid(), time.Now().UnixNano())
}

func waitForEmbeddedServer(port int, password string) bool {
	client := tui.NewClient(port, password)
	backoff := 100 * time.Millisecond
	for range 30 {
		if _, err := client.GetConfig(); err == nil {
			return true
		}
		time.Sleep(backoff)
		if backoff < time.Second {
			backoff = time.Duration(float64(backoff) * 1.5)
		}
	}
	return false
}
