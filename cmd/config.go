package cmd

import (
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/wyx2685/v2node/common/crypt"
)

var (
	configInputPath  string
	configOutputPath string
	configKeyRaw     string
)

var configCommand = cobra.Command{
	Use:   "config",
	Short: "Manage BunCloud agent config files",
}

var configKeygenCommand = cobra.Command{
	Use:   "keygen",
	Short: "Generate a config encryption key",
	Run: func(_ *cobra.Command, _ []string) {
		key, err := crypt.GenerateConfigKey()
		if err != nil {
			log.WithField("err", err).Fatal("generate config key failed")
		}
		fmt.Println(crypt.EncodeConfigKey(key))
	},
}

var configEncryptCommand = cobra.Command{
	Use:   "encrypt",
	Short: "Encrypt a JSON config file",
	Run:   runConfigEncrypt,
}

var configDecryptCommand = cobra.Command{
	Use:   "decrypt",
	Short: "Decrypt an encrypted config file",
	Run:   runConfigDecrypt,
}

func init() {
	configEncryptCommand.Flags().StringVar(&configInputPath, "in", "", "input config path")
	configEncryptCommand.Flags().StringVar(&configOutputPath, "out", "", "output config path")
	configEncryptCommand.Flags().StringVar(&configKeyRaw, "key", "", "base64/hex/plain 32-byte config key")
	configEncryptCommand.MarkFlagRequired("in")

	configDecryptCommand.Flags().StringVar(&configInputPath, "in", "", "input config path")
	configDecryptCommand.Flags().StringVar(&configOutputPath, "out", "", "output config path")
	configDecryptCommand.Flags().StringVar(&configKeyRaw, "key", "", "base64/hex/plain 32-byte config key")
	configDecryptCommand.MarkFlagRequired("in")

	configCommand.AddCommand(&configKeygenCommand)
	configCommand.AddCommand(&configEncryptCommand)
	configCommand.AddCommand(&configDecryptCommand)
	command.AddCommand(&configCommand)
}

func runConfigEncrypt(_ *cobra.Command, _ []string) {
	key := mustResolveConfigKey()
	inputPath, outputPath := resolveConfigPaths()

	raw, err := os.ReadFile(inputPath)
	if err != nil {
		log.WithField("err", err).Fatal("read config file failed")
	}

	encrypted, err := crypt.EncryptConfig(raw, key)
	if err != nil {
		log.WithField("err", err).Fatal("encrypt config failed")
	}

	if err := os.WriteFile(outputPath, encrypted, 0o600); err != nil {
		log.WithField("err", err).Fatal("write encrypted config failed")
	}
	log.Infof("encrypted config written to %s", outputPath)
}

func runConfigDecrypt(_ *cobra.Command, _ []string) {
	key := mustResolveConfigKey()
	inputPath, outputPath := resolveConfigPaths()

	raw, err := os.ReadFile(inputPath)
	if err != nil {
		log.WithField("err", err).Fatal("read config file failed")
	}

	plain, err := crypt.DecryptConfig(raw, key)
	if err != nil {
		log.WithField("err", err).Fatal("decrypt config failed")
	}

	if err := os.WriteFile(outputPath, plain, 0o600); err != nil {
		log.WithField("err", err).Fatal("write plain config failed")
	}
	log.Infof("decrypted config written to %s", outputPath)
}

func mustResolveConfigKey() []byte {
	if configKeyRaw != "" {
		key, err := crypt.ParseConfigKey(configKeyRaw)
		if err != nil {
			log.WithField("err", err).Fatal("parse config key failed")
		}
		return key
	}

	key, err := crypt.ReadConfigKeyFromEnv()
	if err != nil {
		log.WithField("err", err).Fatal("read config key failed")
	}
	return key
}

func resolveConfigPaths() (string, string) {
	inputPath := configInputPath
	outputPath := configOutputPath
	if outputPath == "" {
		outputPath = inputPath
	}
	return inputPath, outputPath
}
