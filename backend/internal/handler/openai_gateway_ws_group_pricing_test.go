package handler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// 分组调价对已打开的 WebSocket 连接生效：同一条连接的两个 turn 之间由管理端
// 改分组倍率（并按分组失效认证缓存），第二个 turn 必须按新倍率计费、按新售价
// 过利润门；HTTP 请求每次经认证缓存取快照，本来就是这个行为。

const (
	wsGroupPricingGroupID = int64(4201) // 与 runOpenAIResponsesWebSocketUsageLogCase 的调度分组一致
	wsGroupPricingKey     = "sk-ws-group-pricing"
	wsGroupPricingFirst   = `{"type":"response.create","model":"gpt-5.6-sol","stream":false}`
	wsGroupPricingSecond  = `{"type":"response.create","model":"gpt-5.6-sol","stream":false}`
)

// wsGroupPricingAPIKeyRepoStub 只实现认证查询与按分组列 Key（认证缓存失效用）。
type wsGroupPricingAPIKeyRepoStub struct {
	service.APIKeyRepository
	mu      sync.Mutex
	apiKey  service.APIKey
	group   service.Group
	err     error
	lookups int
}

func newWSGroupPricingAPIKeyRepoStub(rate float64) *wsGroupPricingAPIKeyRepoStub {
	groupID := wsGroupPricingGroupID
	return &wsGroupPricingAPIKeyRepoStub{
		apiKey: service.APIKey{
			ID:      1801,
			UserID:  1701,
			Key:     wsGroupPricingKey,
			Status:  service.StatusActive,
			GroupID: &groupID,
			User:    &service.User{ID: 1701, Status: service.StatusActive, Concurrency: 1},
		},
		group: service.Group{
			ID:             groupID,
			Name:           "ws-group-pricing",
			Platform:       service.PlatformOpenAI,
			Status:         service.StatusActive,
			RateMultiplier: rate,
			Hydrated:       true,
		},
	}
}

func (s *wsGroupPricingAPIKeyRepoStub) GetByKeyForAuth(ctx context.Context, key string) (*service.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookups++
	if s.err != nil {
		return nil, s.err
	}
	if key != s.apiKey.Key {
		return nil, service.ErrAPIKeyNotFound
	}
	apiKey := s.apiKey
	user := *s.apiKey.User
	apiKey.User = &user
	group := s.group
	apiKey.Group = &group
	return &apiKey, nil
}

func (s *wsGroupPricingAPIKeyRepoStub) ListKeysByGroupID(ctx context.Context, groupID int64) ([]string, error) {
	return []string{s.apiKey.Key}, nil
}

// adminUpdate 模拟管理端改 Key/分组后按分组失效认证缓存（与 admin 分组更新同一入口）。
func (s *wsGroupPricingAPIKeyRepoStub) adminUpdate(apiKeyService *service.APIKeyService, fn func(apiKey *service.APIKey, group *service.Group)) error {
	s.mu.Lock()
	fn(&s.apiKey, &s.group)
	s.mu.Unlock()
	apiKeyService.InvalidateAuthCacheByGroupID(context.Background(), wsGroupPricingGroupID)
	return nil
}

func (s *wsGroupPricingAPIKeyRepoStub) lookupCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookups
}

func newWSGroupPricingAPIKeyService(repo *wsGroupPricingAPIKeyRepoStub) *service.APIKeyService {
	return service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, &config.Config{})
}

func TestOpenAIResponsesWebSocket_RevokedKeyCannotStartNextTurn(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeCtxPool} {
		for _, scenario := range []string{"disabled-key", "deleted-key", "disabled-user", "disabled-group", "exclusive-group", "model-allowlist", "ip-denied", "expired-key", "exhausted-quota", "exhausted-balance", "rpm"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				repo := newWSGroupPricingAPIKeyRepoStub(1)
				cache := &wsTurnBillingCache{}
				cache.balance.Store(100)
				rpm := &wsTurnRPMCache{}
				if scenario == "rpm" {
					repo.apiKey.User.RPMLimit = 1
				}
				apiKeyService := newWSGroupPricingAPIKeyService(repo)
				reason := "authorization is no longer valid"
				if scenario == "model-allowlist" {
					reason = "not available for this group"
				}
				if scenario == "expired-key" || scenario == "exhausted-quota" || scenario == "exhausted-balance" || scenario == "rpm" {
					reason = "billing check failed"
				}
				runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
					firstPayload: wsGroupPricingFirst, secondPayload: wsGroupPricingSecond,
					ingressMode: mode, apiKeyService: apiKeyService, apiKeyCredential: wsGroupPricingKey,
					standardMode: true, billingCache: cache, rpmCache: rpm,
					afterFirstUpstreamRequest: func(*service.ChannelService) error {
						return repo.adminUpdate(apiKeyService, func(key *service.APIKey, group *service.Group) {
							switch scenario {
							case "disabled-key":
								key.Status = service.StatusAPIKeyDisabled
							case "deleted-key":
								repo.err = service.ErrAPIKeyNotFound
							case "disabled-user":
								key.User.Status = service.StatusDisabled
							case "disabled-group":
								group.Status = service.StatusDisabled
							case "exclusive-group":
								group.IsExclusive = true
							case "model-allowlist":
								group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"gpt-other"}}
							case "ip-denied":
								key.IPWhitelist = []string{"192.0.2.1"}
							case "expired-key":
								expired := time.Now().Add(-time.Minute)
								key.ExpiresAt = &expired
							case "exhausted-quota":
								key.Quota = 1
								key.QuotaUsed = 1
							case "exhausted-balance":
								cache.balance.Store(0)
							}
						})
					},
					secondTurnCloseExpected: true, closeReason: reason,
				})
				if scenario == "rpm" {
					require.Equal(t, int64(2), rpm.calls.Load(), "count each turn once, including the rejected second turn")
				}
			})
		}
	}
}

type wsTurnBillingCache struct {
	service.BillingCache
	balance atomic.Int64
}

func (c *wsTurnBillingCache) GetUserBalance(context.Context, int64) (float64, error) {
	return float64(c.balance.Load()), nil
}

func (*wsTurnBillingCache) DeductUserBalance(context.Context, int64, float64) error { return nil }

type wsTurnRPMCache struct {
	service.UserRPMCache
	calls atomic.Int64
}

func (c *wsTurnRPMCache) IncrementUserRPM(context.Context, int64) (int, error) {
	return int(c.calls.Add(1)), nil
}

func TestOpenAIResponsesWebSocket_GroupRateChangeAppliesToNextTurn(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeCtxPool} {
		t.Run(mode, func(t *testing.T) {
			repo := newWSGroupPricingAPIKeyRepoStub(3.0)
			apiKeyService := newWSGroupPricingAPIKeyService(repo)
			got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
				firstPayload:     wsGroupPricingFirst,
				secondPayload:    wsGroupPricingSecond,
				ingressMode:      mode,
				apiKeyService:    apiKeyService,
				apiKeyCredential: wsGroupPricingKey,
				afterFirstUpstreamRequest: func(*service.ChannelService) error {
					return repo.adminUpdate(apiKeyService, func(_ *service.APIKey, group *service.Group) { group.RateMultiplier = 0.3 })
				},
			})

			require.Len(t, got.logs, 2)
			require.InDelta(t, 3.0, got.logs[0].RateMultiplier, 1e-12, "turn 1 bills at the rate in force at connect")
			require.InDelta(t, 0.3, got.logs[1].RateMultiplier, 1e-12, "turn 2 must bill at the group rate in force when it starts")
			require.Greater(t, got.logs[0].ActualCost, 0.0)
			require.InDelta(t, got.logs[0].ActualCost/10, got.logs[1].ActualCost, 1e-15)
		})
	}
}

func TestOpenAIResponsesWebSocket_GroupRateChangeReachesProfitGateOnNextTurn(t *testing.T) {
	repo := newWSGroupPricingAPIKeyRepoStub(3.0)
	repo.group.ProfitControlEnabled = true
	repo.group.ProfitMinMargin = 0.1
	apiKeyService := newWSGroupPricingAPIKeyService(repo)
	accountRate := 1.0
	runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload:          wsGroupPricingFirst,
		secondPayload:         wsGroupPricingSecond,
		apiKeyService:         apiKeyService,
		apiKeyCredential:      wsGroupPricingKey,
		accountRateMultiplier: &accountRate,
		// 3.0 → 0.3：账号倍率 1.0 在新售价下已越线，第二个 turn 必须在转发上游前被拒。
		afterFirstUpstreamRequest: func(*service.ChannelService) error {
			return repo.adminUpdate(apiKeyService, func(_ *service.APIKey, group *service.Group) { group.RateMultiplier = 0.3 })
		},
		secondTurnCloseExpected: true,
		closeStatus:             coderws.StatusTryAgainLater,
		closeReason:             "no longer eligible for this connection",
	})
}

func TestOpenAIResponsesWebSocket_KeyMovedToAnotherGroupClosesConnection(t *testing.T) {
	repo := newWSGroupPricingAPIKeyRepoStub(3.0)
	apiKeyService := newWSGroupPricingAPIKeyService(repo)
	runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload:     wsGroupPricingFirst,
		secondPayload:    wsGroupPricingSecond,
		apiKeyService:    apiKeyService,
		apiKeyCredential: wsGroupPricingKey,
		afterFirstUpstreamRequest: func(*service.ChannelService) error {
			return repo.adminUpdate(apiKeyService, func(apiKey *service.APIKey, group *service.Group) {
				other := wsGroupPricingGroupID + 1
				apiKey.GroupID = &other
				group.ID = other
				group.RateMultiplier = 0.3
			})
		},
		secondTurnCloseExpected: true,
		closeReason:             "authorization is no longer valid",
	})
}

func TestOpenAIResponsesWebSocket_FailedKeyRefreshClosesConnection(t *testing.T) {
	repo := newWSGroupPricingAPIKeyRepoStub(3.0)
	apiKeyService := newWSGroupPricingAPIKeyService(repo)
	runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload:     wsGroupPricingFirst,
		secondPayload:    wsGroupPricingSecond,
		apiKeyService:    apiKeyService,
		apiKeyCredential: wsGroupPricingKey,
		afterFirstUpstreamRequest: func(*service.ChannelService) error {
			return repo.adminUpdate(apiKeyService, func(_ *service.APIKey, group *service.Group) {
				group.RateMultiplier = 0.3
				repo.err = errors.New("auth store unavailable")
			})
		},
		secondTurnCloseExpected: true,
		closeReason:             "authorization is no longer valid",
	})
	require.GreaterOrEqual(t, repo.lookupCount(), 2, "the second turn must have attempted a refresh")
}

type wsTurnAPIKeyLookupFunc func(ctx context.Context, key string) (*service.APIKey, error)

func (f wsTurnAPIKeyLookupFunc) GetByKey(ctx context.Context, key string) (*service.APIKey, error) {
	return f(ctx, key)
}

func TestRefreshOpenAIWSTurnBillingAPIKey(t *testing.T) {
	groupID := int64(77)
	conn := &service.APIKey{
		ID:      5,
		Key:     "sk-conn",
		Status:  service.StatusActive,
		GroupID: &groupID,
		User:    &service.User{ID: 9, Status: service.StatusActive},
		Group:   &service.Group{ID: groupID, Status: service.StatusActive, Platform: service.PlatformOpenAI, SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 3},
	}
	latestWith := func(mutate func(k *service.APIKey)) *service.APIKey {
		gid := groupID
		k := &service.APIKey{
			ID:      5,
			Key:     "sk-conn",
			Status:  service.StatusActive,
			GroupID: &gid,
			User:    &service.User{ID: 9, Status: service.StatusActive, Balance: 123},
			Group:   &service.Group{ID: groupID, Status: service.StatusActive, Platform: service.PlatformOpenAI, SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 0.3},
		}
		if mutate != nil {
			mutate(k)
		}
		return k
	}

	t.Run("same group adopts the latest authorization snapshot", func(t *testing.T) {
		got, err := refreshOpenAIWSTurnBillingAPIKey(context.Background(), wsTurnAPIKeyLookupFunc(func(ctx context.Context, key string) (*service.APIKey, error) {
			require.Equal(t, "sk-conn", key)
			return latestWith(nil), nil
		}), conn)
		require.NoError(t, err)
		require.NotSame(t, conn, got)
		require.InDelta(t, 0.3, got.Group.RateMultiplier, 1e-12)
		require.Equal(t, float64(123), got.User.Balance)
		require.Equal(t, conn.ID, got.ID)
		require.InDelta(t, 3.0, conn.Group.RateMultiplier, 1e-12, "the connection snapshot is never mutated")
	})

	reject := map[string]wsTurnAPIKeyLookupFunc{
		"lookup error": func(ctx context.Context, key string) (*service.APIKey, error) {
			return nil, errors.New("boom")
		},
		"not found": func(ctx context.Context, key string) (*service.APIKey, error) {
			return nil, service.ErrAPIKeyNotFound
		},
		"nil key": func(ctx context.Context, key string) (*service.APIKey, error) { return nil, nil },
		"different key id": func(ctx context.Context, key string) (*service.APIKey, error) {
			return latestWith(func(k *service.APIKey) { k.ID = 6 }), nil
		},
		"moved group": func(ctx context.Context, key string) (*service.APIKey, error) {
			return latestWith(func(k *service.APIKey) { other := int64(78); k.GroupID = &other; k.Group.ID = other }), nil
		},
		"no group": func(ctx context.Context, key string) (*service.APIKey, error) {
			return latestWith(func(k *service.APIKey) { k.Group = nil }), nil
		},
		"platform changed": func(ctx context.Context, key string) (*service.APIKey, error) {
			return latestWith(func(k *service.APIKey) { k.Group.Platform = service.PlatformAnthropic }), nil
		},
		"billing type changed": func(ctx context.Context, key string) (*service.APIKey, error) {
			return latestWith(func(k *service.APIKey) { k.Group.SubscriptionType = service.SubscriptionTypeSubscription }), nil
		},
	}
	for name, lookup := range reject {
		t.Run(name+" rejects the next turn", func(t *testing.T) {
			got, err := refreshOpenAIWSTurnBillingAPIKey(context.Background(), lookup, conn)
			require.Error(t, err)
			require.Nil(t, got)
		})
	}

	t.Run("missing credential or lookup fails closed", func(t *testing.T) {
		called := false
		lookup := wsTurnAPIKeyLookupFunc(func(ctx context.Context, key string) (*service.APIKey, error) {
			called = true
			return latestWith(nil), nil
		})
		noKey := *conn
		noKey.Key = ""
		_, err := refreshOpenAIWSTurnBillingAPIKey(context.Background(), lookup, &noKey)
		require.Error(t, err)
		_, err = refreshOpenAIWSTurnBillingAPIKey(context.Background(), lookup, nil)
		require.Error(t, err)
		_, err = refreshOpenAIWSTurnBillingAPIKey(context.Background(), nil, conn)
		require.Error(t, err)
		require.False(t, called)
	})
}

func TestOpenAIWSTurnBillingAPIKeysKeepPreviousTurnUntilItIsRecorded(t *testing.T) {
	conn := &service.APIKey{ID: 1}
	turn2 := &service.APIKey{ID: 2}
	turn3 := &service.APIKey{ID: 3}
	var keys openAIWSTurnBillingAPIKeys
	require.Same(t, conn, keys.forTurn(1, conn), "a turn without BeforeTurn bills with the connection snapshot")
	keys.set(2, turn2)
	keys.set(3, turn3) // turn 3 starts before turn 2's usage is submitted
	require.Same(t, turn2, keys.forTurn(2, conn))
	require.Same(t, turn3, keys.forTurn(3, conn))
	keys.set(4, conn)
	require.Same(t, conn, keys.forTurn(2, conn), "only the current and previous turn are kept")
	require.Len(t, keys.keys, 2)
}
