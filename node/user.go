package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"sort"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
)

func (c *Controller) reportUserTrafficTask(ctx context.Context) (err error) {
	defer c.reportDomainTraffic(ctx)
	var reportmin = 0
	var devicemin = 0
	if c.info.Common.BaseConfig != nil {
		reportmin = c.info.Common.BaseConfig.NodeReportMinTraffic
		devicemin = c.info.Common.BaseConfig.DeviceOnlineMinTraffic
	}
	userTraffic, _ := c.server.GetUserTrafficSlice(c.tag, reportmin)
	userTraffic = mergePendingTraffic(c.pendingTraffic, userTraffic)
	c.pendingTraffic = userTraffic
	if len(userTraffic) > 0 {
		err = c.apiClient.ReportUserTraffic(ctx, userTraffic)
		if err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Info("Report user traffic failed")
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
		} else {
			c.pendingTraffic = nil
			log.WithField("tag", c.tag).Infof("Report %d users traffic", len(userTraffic))
			//log.WithField("tag", c.tag).Debugf("User traffic: %+v", userTraffic)
		}
	}

	if onlineDevice, err := c.limiter.GetOnlineDevice(); err != nil {
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Info("Get online device failed")
	} else if len(*onlineDevice) > 0 {
		var result []panel.OnlineUser
		var nocountUID = make(map[int]struct{})
		for _, traffic := range userTraffic {
			total := traffic.Upload + traffic.Download
			if total < int64(devicemin*1000) {
				nocountUID[traffic.UID] = struct{}{}
			}
		}
		for _, online := range *onlineDevice {
			if _, ok := nocountUID[online.UID]; !ok {
				result = append(result, online)
			}
		}
		data := make(map[int][]string)
		for _, onlineuser := range result {
			// json structure: { UID1:["ip1","ip2"],UID2:["ip3","ip4"] }
			data[onlineuser.UID] = append(data[onlineuser.UID], onlineuser.IP)
		}
		if len(data) != 0 {
			err := c.apiClient.ReportNodeOnlineUsers(ctx, &data)
			if err != nil {
				log.WithFields(log.Fields{
					"tag": c.tag,
					"err": err,
				}).Info("Report online users failed")
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
			}
		}
		log.WithField("tag", c.tag).Infof("Total %d online users, %d Reported", len(*onlineDevice), len(result))
	}

	return nil
}

func mergePendingTraffic(pending, fresh []panel.UserTraffic) []panel.UserTraffic {
	if len(pending) == 0 {
		return fresh
	}
	merged := make(map[int]panel.UserTraffic, len(pending)+len(fresh))
	for _, values := range [][]panel.UserTraffic{pending, fresh} {
		for _, value := range values {
			total := merged[value.UID]
			total.UID = value.UID
			total.Upload += value.Upload
			total.Download += value.Download
			merged[value.UID] = total
		}
	}
	result := make([]panel.UserTraffic, 0, len(merged))
	for _, total := range merged {
		result = append(result, total)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UID < result[j].UID })
	return result
}

func compareUserList(old, new []panel.UserInfo) (deleted, added, modified []panel.UserInfo) {
	oldMap := make(map[string]panel.UserInfo, len(old))
	for _, u := range old {
		oldMap[u.Uuid] = u
	}

	for _, u := range new {
		if o, ok := oldMap[u.Uuid]; !ok {
			added = append(added, u)
		} else {
			if o.SpeedLimit != u.SpeedLimit ||
				o.DeviceLimit != u.DeviceLimit ||
				ravelCredentialFingerprint(o.RavelCredentials) != ravelCredentialFingerprint(u.RavelCredentials) {
				modified = append(modified, u)
			}
			delete(oldMap, u.Uuid)
		}
	}

	deletedUUIDs := make([]string, 0, len(oldMap))
	for uuid := range oldMap {
		deletedUUIDs = append(deletedUUIDs, uuid)
	}
	sort.Strings(deletedUUIDs)
	for _, uuid := range deletedUUIDs {
		deleted = append(deleted, oldMap[uuid])
	}

	return deleted, added, modified
}

func ravelCredentialFingerprint(credentials []panel.RavelCredential) [sha256.Size]byte {
	credentialHashes := make([][sha256.Size]byte, len(credentials))
	for i := range credentials {
		h := sha256.New()
		writeFingerprintString(h, credentials[i].CredentialID)
		writeFingerprintString(h, credentials[i].CapabilityKey)
		writeFingerprintUint32(h, credentials[i].KeyVersion)
		writeFingerprintUint32(h, credentials[i].PolicyID)
		writeFingerprintString(h, credentials[i].GatewayGroup)
		writeFingerprintInt64(h, credentials[i].NotBefore)
		writeFingerprintInt64(h, credentials[i].NotAfter)
		copy(credentialHashes[i][:], h.Sum(nil))
	}
	sort.Slice(credentialHashes, func(i, j int) bool {
		return bytes.Compare(credentialHashes[i][:], credentialHashes[j][:]) < 0
	})

	h := sha256.New()
	writeFingerprintUint64(h, uint64(len(credentialHashes)))
	for i := range credentialHashes {
		_, _ = h.Write(credentialHashes[i][:])
	}
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], h.Sum(nil))
	return fingerprint
}

func writeFingerprintString(h hash.Hash, value string) {
	writeFingerprintUint64(h, uint64(len(value)))
	_, _ = h.Write([]byte(value))
}

func writeFingerprintUint32(h hash.Hash, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	_, _ = h.Write(encoded[:])
}

func writeFingerprintUint64(h hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = h.Write(encoded[:])
}

func writeFingerprintInt64(h hash.Hash, value int64) {
	writeFingerprintUint64(h, uint64(value))
}
