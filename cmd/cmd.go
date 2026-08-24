package cmd

import (
	log "github.com/sirupsen/logrus"

	"github.com/spf13/cobra"
)

var command = &cobra.Command{
	Use:     "ravel",
	Aliases: []string{"buncloud-agent", "v2node"},
}

func Run() {
	err := command.Execute()
	if err != nil {
		log.WithField("err", err).Error("Execute command failed")
	}
}
