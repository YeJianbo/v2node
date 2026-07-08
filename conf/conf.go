package conf

import (
	"bytes"
	"fmt"
	"os"

	"github.com/spf13/viper"
	"github.com/wyx2685/v2node/common/crypt"
)

const DefaultNodeRetryCount = 1
const DefaultNodeTimeout = 15

type Conf struct {
	LogConfig   LogConfig    `mapstructure:"Log"`
	NodeConfigs []NodeConfig `mapstructure:"Nodes"`
	PprofPort   int          `mapstructure:"PprofPort"`
}

type LogConfig struct {
	Level  string `mapstructure:"Level"`
	Output string `mapstructure:"Output"`
	Access string `mapstructure:"Access"`
}

type NodeConfig struct {
	APIHost    string `mapstructure:"ApiHost"`
	NodeID     int    `mapstructure:"NodeID"`
	Key        string `mapstructure:"ApiKey"`
	Timeout    int    `mapstructure:"Timeout"`
	RetryCount *int   `mapstructure:"RetryCount"`
}

func New() *Conf {
	return &Conf{
		LogConfig: LogConfig{
			Level:  "info",
			Output: "",
			Access: "none",
		},
	}
}

func (p *Conf) LoadFromPath(filePath string) error {
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("open config file error: %s", err)
	}
	plain, err := crypt.MaybeDecryptConfig(raw)
	if err != nil {
		return fmt.Errorf("decode config file error: %s", err)
	}
	return p.LoadFromBytes(plain)
}

func (p *Conf) LoadFromBytes(data []byte) error {
	v := viper.New()
	v.SetConfigType("json")
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("read config file error: %s", err)
	}
	if err := v.Unmarshal(p); err != nil {
		return fmt.Errorf("unmarshal config error: %s", err)
	}
	for i := range p.NodeConfigs {
		if p.NodeConfigs[i].RetryCount == nil {
			p.NodeConfigs[i].RetryCount = intPtr(DefaultNodeRetryCount)
		}
	}
	return nil
}

func intPtr(v int) *int {
	return &v
}
