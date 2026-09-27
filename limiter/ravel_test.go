package limiter

import (
	"sync"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
)

func TestRavelDeviceLimitAndRemovingSpeedLimit(t *testing.T) {
	Init()
	user := panel.UserInfo{Id: 42, Uuid: "ravel-user", SpeedLimit: 1, DeviceLimit: 1}
	limit := AddLimiter("ravel", "ravel-test", []panel.UserInfo{user}, map[int]int{42: 1})
	tag := format.UserTag("ravel-test", user.Uuid)
	if _, rejected := limit.CheckLimit(tag, "192.0.2.1", true); !rejected {
		t.Fatal("new device was admitted over the limit")
	}
	limit.SetAliveList(map[int]int{})
	bucket, rejected := limit.CheckLimit(tag, "192.0.2.1", true)
	if rejected || bucket == nil || bucket.Get() == nil {
		t.Fatal("user with available device slot was rejected or unthrottled")
	}
	user.SpeedLimit = 0
	limit.UpdateUser("ravel-test", nil, nil, []panel.UserInfo{user})
	if bucket.Get() != nil {
		t.Fatal("existing stream retained a removed speed limit")
	}
	online, err := limit.GetOnlineDevice()
	if err != nil || len(*online) != 1 || (*online)[0].UID != 42 {
		t.Fatal("incorrect online identity")
	}
}

func TestRavelLimitConcurrentAdmissionAndUpdates(t *testing.T) {
	Init()
	user := panel.UserInfo{Id: 42, Uuid: "ravel-user"}
	limit := AddLimiter("ravel", "ravel-test", []panel.UserInfo{user}, map[int]int{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			limit.CheckLimit(format.UserTag("ravel-test", user.Uuid), "192.0.2.1", true)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			next := user
			next.SpeedLimit = i % 2
			limit.UpdateUser("ravel-test", nil, nil, []panel.UserInfo{next})
			limit.SetAliveList(map[int]int{})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			limit.GetOnlineDevice()
		}
	}()
	wg.Wait()
}
