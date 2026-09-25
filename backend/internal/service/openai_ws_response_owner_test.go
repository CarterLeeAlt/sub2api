package service

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// H3：previous_response_id 归属校验语义（WS 与 HTTP 共用同一存储与放行规则）。
// WS 铸造路径经 bindOpenAIWSResponseOwner 补记属主，跨租户续链在校验处 fail-closed。
func TestOpenAIWSResponseOwnerValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	s := &OpenAIGatewayService{}
	const groupID = int64(4201)

	newCtxWithKey := func(userID, apiKeyID int64) *gin.Context {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		groupIDCopy := groupID
		c.Set("api_key", &APIKey{ID: apiKeyID, UserID: userID, GroupID: &groupIDCopy})
		return c
	}

	// 铸造侧：WS 路径经 gin ctx 补记属主（user=11, key=22）。
	s.bindOpenAIWSResponseOwner(ctx, newCtxWithKey(11, 22), groupID, "resp_owner_e2e_1")

	t.Run("同 user 同 key 续链放行", func(t *testing.T) {
		owned, err := s.ValidateOpenAIHTTPResponseOwner(ctx, groupID, "resp_owner_e2e_1", 11, 22)
		require.NoError(t, err)
		require.True(t, owned)
	})

	t.Run("同 user 跨 key 续链放行", func(t *testing.T) {
		owned, err := s.ValidateOpenAIHTTPResponseOwner(ctx, groupID, "resp_owner_e2e_1", 11, 33)
		require.NoError(t, err)
		require.True(t, owned)
	})

	t.Run("跨 user 续链拒绝", func(t *testing.T) {
		owned, err := s.ValidateOpenAIHTTPResponseOwner(ctx, groupID, "resp_owner_e2e_1", 99, 22)
		require.NoError(t, err)
		require.False(t, owned)
	})

	t.Run("查无记录按不归属处理（fail-closed）", func(t *testing.T) {
		owned, err := s.ValidateOpenAIHTTPResponseOwner(ctx, groupID, "resp_owner_never_minted", 11, 22)
		require.NoError(t, err)
		require.False(t, owned)
	})

	t.Run("跨分组隔离", func(t *testing.T) {
		owned, err := s.ValidateOpenAIHTTPResponseOwner(ctx, groupID+1, "resp_owner_e2e_1", 11, 22)
		require.NoError(t, err)
		require.False(t, owned)
	})

	t.Run("strip 判定：属主命中不剥离、跨 user 与未知 id 剥离", func(t *testing.T) {
		require.False(t, s.stripOpenAIWSUnownedPreviousResponseID(ctx, newCtxWithKey(11, 22), "resp_owner_e2e_1"))
		require.True(t, s.stripOpenAIWSUnownedPreviousResponseID(ctx, newCtxWithKey(99, 22), "resp_owner_e2e_1"))
		require.True(t, s.stripOpenAIWSUnownedPreviousResponseID(ctx, newCtxWithKey(11, 22), "resp_owner_unknown"))
		require.False(t, s.stripOpenAIWSUnownedPreviousResponseID(ctx, newCtxWithKey(11, 22), ""))
	})
}

// RemovePreviousResponseIDFromBody 是剥离动作的载体，这里钉住剥离语义。
func TestRemovePreviousResponseIDFromBody_StripsOnlyThatField(t *testing.T) {
	body := []byte(`{"model":"gpt-5.1-codex","previous_response_id":"resp_abc","input":[{"type":"message"}]}`)
	stripped := RemovePreviousResponseIDFromBody(body)
	require.NotContains(t, string(stripped), "resp_abc")
	require.Contains(t, string(stripped), `"model":"gpt-5.1-codex"`)
	require.Contains(t, string(stripped), `"input"`)

	untouched := RemovePreviousResponseIDFromBody([]byte(`{"model":"gpt-5.1-codex"}`))
	require.JSONEq(t, `{"model":"gpt-5.1-codex"}`, string(untouched))
}
