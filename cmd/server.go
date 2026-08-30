package cmd

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
	"github.com/wyx2685/v2node/limiter"
	"github.com/wyx2685/v2node/node"
)

var (
	config string
	watch  bool
)

var serverCommand = cobra.Command{
	Use:   "server",
	Short: "Run BunCloud managed agent",
	Run:   serverHandle,
	Args:  cobra.NoArgs,
}

func init() {
	serverCommand.PersistentFlags().
		StringVarP(&config, "config", "c",
			resolveDefaultConfigPath(), "config file path")
	serverCommand.PersistentFlags().
		BoolVarP(&watch, "watch", "w",
			true, "watch file path change")
	command.AddCommand(&serverCommand)
}

func resolveDefaultConfigPath() string {
	candidates := []string{
		strings.TrimSpace(os.Getenv("BUNCLOUD_CONFIG_PATH")),
		strings.TrimSpace(os.Getenv("V2NODE_CONFIG_PATH")),
		"/etc/.buncloud-agent/config.enc.json",
		"/etc/.buncloud-agent/config.json",
		"/etc/v2node/config.json",
	}

	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	if candidates[0] != "" {
		return filepath.Clean(candidates[0])
	}
	if candidates[1] != "" {
		return filepath.Clean(candidates[1])
	}
	return "/etc/.buncloud-agent/config.enc.json"
}

func serverHandle(_ *cobra.Command, _ []string) {
	if err := runServer(config, nil, watch); err != nil {
		log.WithError(err).Error("server stopped")
	}
}

type runtimeApplyReport struct {
	Status         string
	Error          string
	ActiveNodes    int
	RequestedNodes int
	Failures       []node.NodeFailure
	Snapshot       node.RuntimeSnapshot
}

type runtimeBundle struct {
	config *conf.Conf
	nodes  *node.Node
	core   *core.V2Core
}

func runServer(configPath string, reloadCh chan struct{}, watchConfig bool) error {
	return runServerWithApply(configPath, reloadCh, watchConfig, configPath+".last-good", node.RuntimeSnapshot{}, nil)
}

func runServerWithApply(
	configPath string,
	reloadCh chan struct{},
	watchConfig bool,
	fallbackPath string,
	snapshot node.RuntimeSnapshot,
	onApply func(runtimeApplyReport),
) error {
	showVersion()
	log.SetFormatter(&log.TextFormatter{
		DisableTimestamp: true,
		DisableQuote:     true,
		PadLevelText:     false,
	})
	if reloadCh == nil {
		reloadCh = make(chan struct{}, 1)
	}
	limiter.Init()

	bundle, report, err := startRuntime(configPath, reloadCh, snapshot)
	if err != nil && fallbackPath != "" {
		if restoreErr := copyRuntimeConfig(fallbackPath, configPath); restoreErr == nil {
			bundle, report, err = startRuntime(configPath, reloadCh, snapshot)
			if err == nil {
				report.Status = "failed"
				report.Error = "desired configuration failed; restored last-good configuration"
			}
		}
	}
	if err != nil {
		if onApply != nil {
			onApply(report)
		}
		return err
	}
	defer func() {
		if bundle != nil {
			_ = bundle.nodes.Close()
			_ = closeCoreSafely(bundle.core)
		}
	}()
	configureRuntimeLogging(bundle.config)
	snapshot = report.Snapshot
	if onApply != nil {
		onApply(report)
	}
	log.Infof("Nodes started: %d/%d", report.ActiveNodes, report.RequestedNodes)

	// Enable pprof if configured
	if bundle.config.PprofPort != 0 {
		go func() {
			log.Infof("Starting pprof server on :%d", bundle.config.PprofPort)
			if err := http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", bundle.config.PprofPort), nil); err != nil {
				log.WithField("err", err).Error("pprof server failed")
			}
		}()
	}
	if watchConfig {
		// On file change, just signal reload; do not run reload concurrently here
		err = bundle.config.Watch(configPath, func() {
			select {
			case reloadCh <- struct{}{}:
			default: // drop if a reload is already queued
			}
		})
		if err != nil {
			return fmt.Errorf("start config watcher: %w", err)
		}
	}
	// clear memory
	runtime.GC()

	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(osSignals)

	for {
		select {
		case <-osSignals:
			log.Info("收到退出信号，正在关闭程序...")
			return nil
		case <-reloadCh:
			log.Info("收到重启信号，正在重新加载配置...")
			report, err := reloadRuntime(configPath, fallbackPath, reloadCh, snapshot, &bundle)
			if onApply != nil {
				onApply(report)
			}
			if err != nil {
				log.WithField("err", err).Error("配置应用失败，现有可运行节点已保留")
				continue
			}
			configureRuntimeLogging(bundle.config)
			snapshot = report.Snapshot
			log.Infof("配置应用完成: %s, nodes=%d/%d", report.Status, report.ActiveNodes, report.RequestedNodes)
		}
	}
}

func reloadRuntime(
	configPath string,
	fallbackPath string,
	reloadCh chan struct{},
	snapshot node.RuntimeSnapshot,
	current **runtimeBundle,
) (runtimeApplyReport, error) {
	prepared, err := prepareRuntime(configPath, reloadCh, snapshot)
	if err != nil {
		return runtimeApplyReport{Status: "failed", Error: err.Error()}, err
	}

	old := *current
	closeErr := old.nodes.Close()
	if coreErr := closeCoreSafely(old.core); coreErr != nil && closeErr == nil {
		closeErr = coreErr
	}
	if closeErr != nil {
		log.WithError(closeErr).Warn("旧运行时清理不完整，继续激活已准备的新配置")
	}

	report, activationErr := activateRuntime(prepared)
	if activationErr == nil || report.ActiveNodes > 0 || report.RequestedNodes == 0 {
		*current = prepared
		runtime.GC()
		return report, nil
	}
	if fallbackPath != "" {
		if restoreErr := copyRuntimeConfig(fallbackPath, configPath); restoreErr == nil {
			fallback, fallbackReport, fallbackErr := startRuntime(configPath, reloadCh, snapshot)
			if fallbackErr == nil {
				_ = closeCoreSafely(prepared.core)
				*current = fallback
				fallbackReport.Status = "failed"
				fallbackReport.Error = activationErr.Error() + "; restored last-good configuration"
				return fallbackReport, activationErr
			}
		}
	}

	// Keep an empty core alive so a later valid revision can recover without a service crash loop.
	*current = prepared
	return report, activationErr
}

func startRuntime(configPath string, reloadCh chan struct{}, snapshot node.RuntimeSnapshot) (*runtimeBundle, runtimeApplyReport, error) {
	bundle, err := prepareRuntime(configPath, reloadCh, snapshot)
	if err != nil {
		return nil, runtimeApplyReport{Status: "failed", Error: err.Error()}, err
	}
	report, err := activateRuntime(bundle)
	if err != nil && report.ActiveNodes == 0 && report.RequestedNodes > 0 {
		_ = closeCoreSafely(bundle.core)
		return nil, report, err
	}
	return bundle, report, nil
}

func prepareRuntime(configPath string, reloadCh chan struct{}, snapshot node.RuntimeSnapshot) (*runtimeBundle, error) {
	configuration := conf.New()
	if err := configuration.LoadFromPath(configPath); err != nil {
		return nil, err
	}
	nodes, err := node.NewWithSnapshot(configuration.NodeConfigs, snapshot)
	if err != nil {
		return nil, err
	}
	if nodes.RequestedCount() > 0 && len(nodes.NodeInfos) == 0 {
		return nil, fmt.Errorf("none of the %d managed nodes could be prepared: %s", nodes.RequestedCount(), nodes.FailureMessage())
	}
	v2core := core.New(configuration)
	v2core.ReloadCh = reloadCh
	if err := startCoreSafely(v2core, nodes.NodeInfos); err != nil {
		_ = closeCoreSafely(v2core)
		return nil, err
	}
	return &runtimeBundle{config: configuration, nodes: nodes, core: v2core}, nil
}

func activateRuntime(bundle *runtimeBundle) (runtimeApplyReport, error) {
	err := bundle.nodes.Start(bundle.config.NodeConfigs, bundle.core)
	report := runtimeApplyReport{
		Status:         "success",
		ActiveNodes:    bundle.nodes.ActiveCount(),
		RequestedNodes: bundle.nodes.RequestedCount(),
		Failures:       bundle.nodes.Failures(),
		Snapshot:       bundle.nodes.RuntimeSnapshot(),
	}
	if err != nil {
		report.Error = err.Error()
		if report.ActiveNodes > 0 {
			report.Status = "partial"
			return report, nil
		}
		report.Status = "failed"
		return report, err
	}
	return report, nil
}

func startCoreSafely(v2core *core.V2Core, infos []*panel.NodeInfo) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("start core panic: %v", recovered)
		}
	}()
	return v2core.Start(infos)
}

func closeCoreSafely(v2core *core.V2Core) (err error) {
	if v2core == nil || v2core.Server == nil {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("close core panic: %v", recovered)
		}
	}()
	return v2core.Close()
}

func configureRuntimeLogging(configuration *conf.Conf) {
	switch configuration.LogConfig.Level {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn", "warning":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	}

	oldWriter := log.StandardLogger().Out
	newWriter := os.Stdout
	if configuration.LogConfig.Output != "" {
		file, err := os.OpenFile(configuration.LogConfig.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.WithField("err", err).Error("Open log file failed, using stdout instead")
		} else {
			newWriter = file
		}
	}
	log.SetOutput(newWriter)
	if oldFile, ok := oldWriter.(*os.File); ok && oldFile != os.Stdout && oldFile != os.Stderr && oldFile != newWriter {
		_ = oldFile.Close()
	}
}

func copyRuntimeConfig(sourcePath, targetPath string) error {
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(targetPath), ".runtime-config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, targetPath)
}
