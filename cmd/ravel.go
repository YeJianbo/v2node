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
	var currentUpdateVersion atomic.Value
	currentUpdateVersion.Store(version)
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
		if config.RequestID != "" {
			if !updateRunning.CompareAndSwap(false, true) {
				return
			}
			go func(request agent.AutoUpdateConfig) {
				defer updateRunning.Store(false)
				binaryPath, err := os.Executable()
				if err != nil {
					log.WithError(err).Warn("resolve Ravel executable failed")
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				currentVersion, _ := currentUpdateVersion.Load().(string)
				target, updated, updateErr := agent.CheckAndInstallRequestedRavelUpdate(
					ctx,
					request,
					currentVersion,
					binaryPath,
				)
				status := "success"
				installedVersion := currentVersion
				message := ""
				if updateErr != nil {
					status = "failed"
					message = updateErr.Error()
					log.WithError(updateErr).Warn("Ravel manual update failed")
				} else if target != "" {
					installedVersion = target
					currentUpdateVersion.Store(target)
				}
				if err := controller.AcknowledgeUpdate(request, status, installedVersion, message); err != nil {
					log.WithError(err).Warn("acknowledge Ravel manual update failed")
				} else {
					controller.ClearUpdateRequest(request.RequestID)
				}
				if updateErr != nil || !updated {
					return
				}
				log.Infof("Ravel manually updated from %s to %s; scheduling service restart", currentVersion, target)
				if err := scheduleRavelRestart(); err != nil {
					log.WithError(err).Error("schedule Ravel restart failed")
				}
			}(config)
			return
		}
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
			currentVersion, _ := currentUpdateVersion.Load().(string)
			latest, updated, err := agent.CheckAndInstallRavelUpdate(ctx, config, currentVersion, binaryPath)
			if err != nil {
				log.WithError(err).Warn("Ravel auto update check failed")
				return
			}
			if !updated {
				log.Debugf("Ravel is current at %s", latest)
				return
			}
			currentUpdateVersion.Store(latest)
			log.Infof("Ravel updated from %s to %s; scheduling service restart", currentVersion, latest)
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
