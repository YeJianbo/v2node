package limiter

import (
	"errors"
	"strings"
	"sync"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/common/rate"
)

var limitLock sync.RWMutex
var limiter map[string]*Limiter

func Init() {
	limiter = map[string]*Limiter{}
}

type Limiter struct {
	mu            sync.Mutex
	Nodetype      string         // Node type, e.g. "v2ray", "trojan", "shadowsocks"
	SpeedLimit    int            // Node speed limit in Mbps
	UserOnlineIP  *sync.Map      // Key: TagUUID, value: {Key: Ip, value: Uid}
	OldUserOnline *sync.Map      // Key: onlineDeviceKey, value: struct{}
	UUIDtoUID     map[string]int // Key: UUID, value: Uid
	UserLimitInfo *sync.Map      // Key: TagUUID value: UserLimitInfo
	SpeedLimiter  *sync.Map      // key: TagUUID, value: *DynamicBucket
	AliveList     map[int]int    // Key: Uid, value: alive_ip
}

type UserLimitInfo struct {
	UID               int
	SpeedLimit        int
	DeviceLimit       int
	DynamicSpeedLimit int
	ExpireTime        int64
	OverLimit         bool
}

type onlineDeviceKey struct {
	uid int
	ip  string
}

func AddLimiter(nodetype string, tag string, users []panel.UserInfo, aliveList map[int]int) *Limiter {
	l := &Limiter{
		Nodetype:      nodetype,
		UserOnlineIP:  new(sync.Map),
		UserLimitInfo: new(sync.Map),
		SpeedLimiter:  new(sync.Map),
		AliveList:     aliveList,
		OldUserOnline: new(sync.Map),
	}
	uuidmap := make(map[string]int)
	for i := range users {
		uuidmap[users[i].Uuid] = users[i].Id
		userLimit := &UserLimitInfo{}
		userLimit.UID = users[i].Id
		if users[i].SpeedLimit != 0 {
			userLimit.SpeedLimit = users[i].SpeedLimit
		}
		if users[i].DeviceLimit != 0 {
			userLimit.DeviceLimit = users[i].DeviceLimit
		}
		userLimit.OverLimit = false
		l.UserLimitInfo.Store(format.UserTag(tag, users[i].Uuid), userLimit)
	}
	l.UUIDtoUID = uuidmap
	limitLock.Lock()
	limiter[tag] = l
	limitLock.Unlock()
	return l
}

func GetLimiter(tag string) (info *Limiter, err error) {
	limitLock.RLock()
	info, ok := limiter[tag]
	limitLock.RUnlock()
	if !ok {
		return nil, errors.New("not found")
	}
	return info, nil
}

func DeleteLimiter(tag string) {
	limitLock.Lock()
	delete(limiter, tag)
	limitLock.Unlock()
}

func (l *Limiter) UpdateUser(tag string, added []panel.UserInfo, deleted []panel.UserInfo, modified []panel.UserInfo) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range deleted {
		l.UserLimitInfo.Delete(format.UserTag(tag, deleted[i].Uuid))
		l.UserOnlineIP.Delete(format.UserTag(tag, deleted[i].Uuid))
		l.SpeedLimiter.Delete(format.UserTag(tag, deleted[i].Uuid))
		delete(l.UUIDtoUID, deleted[i].Uuid)
		delete(l.AliveList, deleted[i].Id)
	}
	for i := range modified {
		userSpeedLimit := modified[i].SpeedLimit
		if v, ok := l.UserLimitInfo.Load(format.UserTag(tag, modified[i].Uuid)); ok {
			u := v.(*UserLimitInfo)
			u.SpeedLimit = modified[i].SpeedLimit
			u.DeviceLimit = modified[i].DeviceLimit
			if u.ExpireTime != 0 && u.ExpireTime < time.Now().Unix() {
				u.DynamicSpeedLimit = 0
				u.ExpireTime = 0
			}
			userSpeedLimit = determineSpeedLimit(u.SpeedLimit, u.DynamicSpeedLimit)
			l.UserLimitInfo.Store(format.UserTag(tag, modified[i].Uuid), u)
		}
		limit := int64(determineSpeedLimit(l.SpeedLimit, userSpeedLimit)) * 1000000 / 8
		if limit > 0 {
			if v, ok := l.SpeedLimiter.Load(format.UserTag(tag, modified[i].Uuid)); ok {
				d := v.(*rate.DynamicBucket)
				d.Update(limit)
			} else {
				d := rate.NewDynamicBucket(limit)
				l.SpeedLimiter.Store(format.UserTag(tag, modified[i].Uuid), d)
			}
		} else {
			if v, ok := l.SpeedLimiter.Load(format.UserTag(tag, modified[i].Uuid)); ok {
				v.(*rate.DynamicBucket).Update(0)
			}
			l.SpeedLimiter.Delete(format.UserTag(tag, modified[i].Uuid))
		}
	}
	for i := range added {
		userLimit := &UserLimitInfo{
			UID: added[i].Id,
		}
		if added[i].SpeedLimit != 0 {
			userLimit.SpeedLimit = added[i].SpeedLimit
			userLimit.ExpireTime = 0
		}
		if added[i].DeviceLimit != 0 {
			userLimit.DeviceLimit = added[i].DeviceLimit
		}
		userLimit.OverLimit = false
		l.UserLimitInfo.Store(format.UserTag(tag, added[i].Uuid), userLimit)
		l.UUIDtoUID[added[i].Uuid] = added[i].Id
	}
}

func (l *Limiter) UpdateDynamicSpeedLimit(tag, uuid string, limit int, expire time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if v, ok := l.UserLimitInfo.Load(format.UserTag(tag, uuid)); ok {
		info := v.(*UserLimitInfo)
		info.DynamicSpeedLimit = limit
		info.ExpireTime = expire.Unix()
		if v, ok := l.SpeedLimiter.Load(format.UserTag(tag, uuid)); ok {
			effective := determineSpeedLimit(l.SpeedLimit, determineSpeedLimit(info.SpeedLimit, limit))
			v.(*rate.DynamicBucket).Update(int64(effective) * 1000000 / 8)
		}
	} else {
		return errors.New("not found")
	}
	return nil
}

// Credentials may rotate, but rate buckets and local device reservations belong to the user.
func (l *Limiter) ReplaceRavelUsers(tag string, users []panel.UserInfo) {
	l.mu.Lock()
	defer l.mu.Unlock()
	type state struct {
		info   *UserLimitInfo
		bucket *rate.DynamicBucket
		online *sync.Map
	}
	previous := make(map[int]state, len(l.UUIDtoUID))
	for uuid, uid := range l.UUIDtoUID {
		key := format.UserTag(tag, uuid)
		item := state{}
		if value, ok := l.UserLimitInfo.Load(key); ok {
			item.info = value.(*UserLimitInfo)
		}
		if value, ok := l.SpeedLimiter.Load(key); ok {
			item.bucket = value.(*rate.DynamicBucket)
		}
		if value, ok := l.UserOnlineIP.Load(key); ok {
			item.online = value.(*sync.Map)
		}
		previous[uid] = item
	}
	l.UUIDtoUID = make(map[string]int, len(users))
	l.UserLimitInfo = new(sync.Map)
	l.SpeedLimiter = new(sync.Map)
	l.UserOnlineIP = new(sync.Map)
	retained := make(map[int]bool, len(users))
	for _, user := range users {
		item := previous[user.Id]
		if item.info == nil {
			item.info = &UserLimitInfo{UID: user.Id}
		}
		item.info.SpeedLimit = user.SpeedLimit
		item.info.DeviceLimit = user.DeviceLimit
		if item.info.ExpireTime != 0 && item.info.ExpireTime <= time.Now().Unix() {
			item.info.DynamicSpeedLimit = 0
			item.info.ExpireTime = 0
		}
		limit := int64(determineSpeedLimit(l.SpeedLimit, determineSpeedLimit(user.SpeedLimit, item.info.DynamicSpeedLimit))) * 1000000 / 8
		if item.bucket != nil {
			item.bucket.Update(limit)
		}
		if limit > 0 && item.bucket == nil {
			item.bucket = rate.NewDynamicBucket(limit)
		}
		key := format.UserTag(tag, user.Uuid)
		l.UUIDtoUID[user.Uuid] = user.Id
		l.UserLimitInfo.Store(key, item.info)
		if item.bucket != nil {
			l.SpeedLimiter.Store(key, item.bucket)
		}
		if item.online != nil {
			l.UserOnlineIP.Store(key, item.online)
		}
		retained[user.Id] = true
	}
	for uid := range l.AliveList {
		if !retained[uid] {
			delete(l.AliveList, uid)
		}
	}
	l.OldUserOnline.Range(func(key, _ interface{}) bool {
		if !retained[key.(onlineDeviceKey).uid] {
			l.OldUserOnline.Delete(key)
		}
		return true
	})
}

func (l *Limiter) CheckLimit(taguuid string, ip string, noUDPsource bool) (DynamicBucket *rate.DynamicBucket, Reject bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// check if ipv4 mapped ipv6
	ip = strings.TrimPrefix(ip, "::ffff:")

	// check and gen speed limit Bucket
	nodeLimit := l.SpeedLimit
	userLimit := 0
	deviceLimit := 0
	var uid int
	if v, ok := l.UserLimitInfo.Load(taguuid); ok {
		u := v.(*UserLimitInfo)
		deviceLimit = u.DeviceLimit
		uid = u.UID
		if u.ExpireTime < time.Now().Unix() && u.ExpireTime != 0 {
			userLimit = u.SpeedLimit
			u.DynamicSpeedLimit = 0
			u.ExpireTime = 0
			if v, ok := l.SpeedLimiter.Load(taguuid); ok {
				effective := int64(determineSpeedLimit(nodeLimit, userLimit)) * 1000000 / 8
				v.(*rate.DynamicBucket).Update(effective)
				if effective <= 0 {
					l.SpeedLimiter.Delete(taguuid)
				}
			}
		} else {
			userLimit = determineSpeedLimit(u.SpeedLimit, u.DynamicSpeedLimit)
		}
	} else {
		return nil, true
	}
	if noUDPsource || l.Nodetype == "hysteria2" || l.Nodetype == "tuic" {
		var ipMap *sync.Map
		if v, ok := l.UserOnlineIP.Load(taguuid); ok {
			ipMap = v.(*sync.Map)
		}
		known := false
		if ipMap != nil {
			_, known = ipMap.Load(ip)
		}
		if !known {
			// Panel counts lag behind new connections in the current report interval.
			if deviceLimit > 0 && ipMap != nil {
				localDevices := 0
				ipMap.Range(func(_, _ interface{}) bool {
					localDevices++
					return localDevices < deviceLimit
				})
				if localDevices >= deviceLimit {
					return nil, true
				}
			}
			// Continuity is scoped to one user; a shared NAT address is not authorization.
			_, previous := l.OldUserOnline.LoadAndDelete(onlineDeviceKey{uid: uid, ip: ip})
			if !previous && deviceLimit > 0 && deviceLimit <= l.AliveList[uid] {
				return nil, true
			}
			if ipMap == nil {
				ipMap = new(sync.Map)
				l.UserOnlineIP.Store(taguuid, ipMap)
			}
			ipMap.Store(ip, uid)
		}
	}

	limit := int64(determineSpeedLimit(nodeLimit, userLimit)) * 1000000 / 8 // If you need the Speed limit
	if limit > 0 {
		if v, ok := l.SpeedLimiter.Load(taguuid); ok {
			return v.(*rate.DynamicBucket), false
		} else {
			d := rate.NewDynamicBucket(limit)
			l.SpeedLimiter.Store(taguuid, d)
			return d, false
		}
	} else {
		return nil, false
	}
}

func (l *Limiter) GetOnlineDevice() (*[]panel.OnlineUser, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var onlineUser []panel.OnlineUser
	l.OldUserOnline = new(sync.Map)
	l.UserOnlineIP.Range(func(key, value interface{}) bool {
		taguuid := key.(string)
		ipMap := value.(*sync.Map)
		ipMap.Range(func(key, value interface{}) bool {
			uid := value.(int)
			ip := key.(string)
			l.OldUserOnline.Store(onlineDeviceKey{uid: uid, ip: ip}, struct{}{})
			onlineUser = append(onlineUser, panel.OnlineUser{UID: uid, IP: ip})
			return true
		})
		l.UserOnlineIP.Delete(taguuid) // Reset online device
		return true
	})

	return &onlineUser, nil
}

type UserIpList struct {
	Uid    int      `json:"Uid"`
	IpList []string `json:"Ips"`
}

func (l *Limiter) SetAliveList(alive map[int]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.AliveList = alive
}
