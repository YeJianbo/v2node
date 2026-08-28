package cmd

import (
	"context"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/wyx2685/v2node/agent"
)

var (
	ravelStateFile   string
	ravelConfigFile  string
	ravelKeyFile     string
	ravelManagedFile string
)

var ravelCommand = cobra.Command{
	Use:   "ravel",
	Short: "Run the integrated Ravel control plane and node runtime",
	Args:  cobra.NoArgs,
	Run:   ravelHandle,
}

func init() {
	ravelCommand.Flags().StringVar(&ravelStateFile, "state", "/etc/.buncloud-agent/state.json", "Ravel state path")
	ravelCommand.Flags().StringVar(&ravelConfigFile, "config", "/etc/.buncloud-agent/config.enc.json", "encrypted node config path")
	ravelCommand.Flags().StringVar(&ravelKeyFile, "key", "/etc/.buncloud-agent/config.key", "config key path")
	ravelCommand.Flags().StringVar(&ravelManagedFile, "managed", "/etc/.buncloud-agent/managed-nodes.json", "managed node state path")
	command.AddCommand(&ravelCommand)
}

func ravelHandle(_ *cobra.Command, _ []string) {
	agent.SetRuntimeVersion(version)
	state, err := agent.LoadState(ravelStateFile)
	if err != nil {
		log.WithError(err).Fatal("load Ravel state failed")
	}
	client, err := agent.NewClient(state)
	if err != nil {
		log.WithError(err).Fatal("initialize Ravel client failed")
	}
	controller := &agent.Controller{
		Client:      client,
		ConfigFile:  ravelConfigFile,
		KeyFile:     ravelKeyFile,
		ManagedFile: ravelManagedFile,
		Relay:       agent.NewRelayManager("/usr/local/ravel/gost", "/etc/.buncloud-agent/relay.json"),
	}
	changed, nodeCount, err := controller.Sync()
	if err != nil {
		log.WithError(err).Fatal("initial Ravel sync failed")
	}
	_ = changed
	log.Infof("Ravel synchronized %d managed nodes", nodeCount)

	reloadCh := make(chan struct{}, 1)
	go runRavelLoop(controller, state, reloadCh, nodeCount)
	runServer(ravelConfigFile, reloadCh, false)
}

func runRavelLoop(controller *agent.Controller, state agent.State, reloadCh chan<- struct{}, nodeCount int) {
	syncTicker := time.NewTicker(time.Duration(state.SyncInterval) * time.Second)
	statusTicker := time.NewTicker(time.Duration(state.StatusInterval) * time.Second)
	defer syncTicker.Stop()
	defer statusTicker.Stop()
	var qualityRunning atomic.Bool
	var updateRunning atomic.Bool
	qualityNext := time.Now()
	qualityFingerprint := ""
	updateNext := time.Now()
	maybeRunNetworkQuality := func() {
		config := controller.NetworkQualityConfig()
		fingerprint := agent.NetworkQualityFingerprint(config)
		if !config.Enabled {
			qualityFingerprint = fingerprint
			qualityNext = time.Time{}
			return
		}
		if fingerprint != qualityFingerprint {
			qualityFingerprint = fingerprint
			qualityNext = time.Now()
		}
		if !qualityNext.IsZero() && time.Now().Before(qualityNext) {
			return
		}
		if !qualityRunning.CompareAndSwap(false, true) {
			return
		}
		qualityNext = time.Now().Add(agent.NetworkQualityInterval(config))
		go func() {
			defer qualityRunning.Store(false)
			if err := controller.ProbeAndPushNetworkQuality(); err != nil {
				log.WithError(err).Warn("Ravel network quality report failed")
			}
		}()
	}
	maybeRunAutoUpdate := func() {
		config := controller.AutoUpdateConfig()
		if !config.Enabled {
			updateNext = time.Time{}
			return
		}
		if updateNext.IsZero() {
			updateNext = time.Now()
		}
		if time.Now().Before(updateNext) || !updateRunning.CompareAndSwap(false, true) {
			return
		}
		updateNext = time.Now().Add(agent.AutoUpdateInterval(config))
		go func() {
			defer updateRunning.Store(false)
			binaryPath, err := os.Executable()
			if err != nil {
				log.WithError(err).Warn("resolve Ravel executable failed")
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			latest, updated, err := agent.CheckAndInstallRavelUpdate(ctx, config, version, binaryPath)
			if err != nil {
				log.WithError(err).Warn("Ravel auto update check failed")
				return
			}
			if !updated {
				log.Debugf("Ravel is current at %s", latest)
				return
			}
			log.Infof("Ravel updated from %s to %s; scheduling service restart", version, latest)
			if err := scheduleRavelRestart(); err != nil {
				log.WithError(err).Error("schedule Ravel restart failed")
			}
		}()
	}
	for {
		select {
		case <-syncTicker.C:
			changed, count, err := controller.Sync()
			if err != nil {
				log.WithError(err).Warn("Ravel sync failed")
				continue
			}
			nodeCount = count
			if changed {
				select {
				case reloadCh <- struct{}{}:
				default:
				}
			}
		case <-statusTicker.C:
			if err := controller.PushStatus(agent.CollectStatus(nodeCount)); err != nil {
				log.WithError(err).Warn("Ravel status report failed")
			}
			maybeRunNetworkQuality()
			maybeRunAutoUpdate()
		}
	}
}

func scheduleRavelRestart() error {
	command := exec.Command("sh", "-c", "(sleep 2; if command -v systemctl >/dev/null 2>&1; then systemctl restart ravel; else service ravel restart; fi) >/dev/null 2>&1 &")
	return command.Run()
}
