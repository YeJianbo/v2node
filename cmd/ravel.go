package cmd

import (
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
		}
	}
}
