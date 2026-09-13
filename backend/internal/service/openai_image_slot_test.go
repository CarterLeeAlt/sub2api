//go:build unit

package service

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fakeImageSlotAcquirer struct {
	acquireCalls int
	release      func()
	acquired     bool
}

func (f *fakeImageSlotAcquirer) AcquireForForward(c *gin.Context) (func(), bool) {
	f.acquireCalls++
	if f.acquired {
		return f.release, true
	}
	return nil, false
}

func newImageSlotTestContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	return c
}

func TestAcquireOpenAIImageSlotForForward_NoAcquirerIsNoOp(t *testing.T) {
	c := newImageSlotTestContext()
	require.True(t, acquireOpenAIImageSlotForForward(c))
	ReleaseOpenAIImageSlotForForward(c)
}

func TestAcquireOpenAIImageSlotForForward_AcquiredThenDeduped(t *testing.T) {
	c := newImageSlotTestContext()
	released := false
	fake := &fakeImageSlotAcquirer{acquired: true, release: func() { released = true }}
	SetOpenAIImageSlotAcquirer(c, fake)

	require.True(t, acquireOpenAIImageSlotForForward(c))
	require.Equal(t, 1, fake.acquireCalls)

	// failover 的后续 attempt 重新进入该路径时必须防重，避免同一请求重复占槽。
	require.True(t, acquireOpenAIImageSlotForForward(c))
	require.Equal(t, 1, fake.acquireCalls)

	ReleaseOpenAIImageSlotForForward(c)
	require.True(t, released)
	ReleaseOpenAIImageSlotForForward(c)
}

func TestAcquireOpenAIImageSlotForForward_AcquireFailedPropagates(t *testing.T) {
	c := newImageSlotTestContext()
	fake := &fakeImageSlotAcquirer{acquired: false}
	SetOpenAIImageSlotAcquirer(c, fake)

	require.False(t, acquireOpenAIImageSlotForForward(c))
	require.Equal(t, 1, fake.acquireCalls)
	ReleaseOpenAIImageSlotForForward(c)
}
