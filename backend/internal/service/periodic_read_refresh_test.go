package service

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPeriodicReadRefreshRecoversThenRestoresNormalCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stop := make(chan struct{})
		done := make(chan struct{})
		var calls []time.Time
		go func() {
			defer close(done)
			runPeriodicReadRefresh(stop, 48*time.Hour,
				func() bool { calls = append(calls, time.Now()); return false },
				func() bool { calls = append(calls, time.Now()); return len(calls) >= 3 })
		}()
		synctest.Wait()
		require.Len(t, calls, 1)
		time.Sleep(5*time.Minute - time.Second)
		synctest.Wait()
		require.Len(t, calls, 1)
		time.Sleep(time.Second)
		synctest.Wait()
		require.Len(t, calls, 2)
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		require.Len(t, calls, 3)
		time.Sleep(48*time.Hour - time.Second)
		synctest.Wait()
		require.Len(t, calls, 3)
		time.Sleep(time.Second)
		synctest.Wait()
		require.Len(t, calls, 4)
		close(stop)
		<-done
		time.Sleep(48 * time.Hour)
		require.Len(t, calls, 4)
	})
}
