package core

import (
	"fmt"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func (v *V2Core) AddNode(tag string, info *panel.NodeInfo) error {
	if info != nil && info.Type == "ravel" {
		runtime, err := newRavelRuntime(tag, info)
		if err != nil {
			return fmt.Errorf("build ravel runtime: %w", err)
		}
		v.ravelMu.Lock()
		defer v.ravelMu.Unlock()
		if _, exists := v.ravel[tag]; exists {
			_ = runtime.close()
			return fmt.Errorf("ravel node %s already exists", tag)
		}
		v.ravel[tag] = runtime
		return nil
	}
	inBoundConfig, err := buildInbound(info, tag)
	if err != nil {
		return fmt.Errorf("build inbound error: %s", err)
	}
	err = v.addInbound(inBoundConfig)
	if err != nil {
		return fmt.Errorf("add inbound error: %s", err)
	}
	return nil
}

func (v *V2Core) DelNode(tag string) error {
	v.ravelMu.Lock()
	runtime, exists := v.ravel[tag]
	if exists {
		delete(v.ravel, tag)
	}
	v.ravelMu.Unlock()
	if exists {
		if err := runtime.close(); err != nil {
			return fmt.Errorf("close ravel node: %w", err)
		}
		return nil
	}
	err := v.removeInbound(tag)
	if err != nil {
		return fmt.Errorf("remove in error: %s", err)
	}
	return nil
}
