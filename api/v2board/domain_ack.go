package panel

import "encoding/json"

func jsonAccepted(data []byte) bool {
	var result struct {
		Accepted bool `json:"accepted"`
	}
	return json.Unmarshal(data, &result) == nil && result.Accepted
}
