package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type cnBalanceResponseUpstream struct {
	statusCode int
	body       string
}

func (u *cnBalanceResponseUpstream) Do(
	_ *http.Request,
	_ string,
	_ int64,
	_ int,
) (*http.Response, error) {
	return &http.Response{
		StatusCode: u.statusCode,
		Body:       io.NopCloser(strings.NewReader(u.body)),
		Header:     make(http.Header),
	}, nil
}

func (u *cnBalanceResponseUpstream) DoWithTLS(
	req *http.Request,
	proxyURL string,
	accountID int64,
	accountConcurrency int,
	_ *tlsfingerprint.Profile,
) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

type cnBalanceProbeRepo struct {
	AccountRepository
	account     *Account
	extraWrites []map[string]any
}

func (r *cnBalanceProbeRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return r.account, nil
}

func (r *cnBalanceProbeRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.extraWrites = append(r.extraWrites, updates)
	return nil
}

func newDeepSeekBalanceProbeAccount() *Account {
	return &Account{
		ID:       42,
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"account_mode": AccountModePayG,
			"api_key":      "sk-test",
			"base_url":     "https://relay.example.com",
		},
	}
}

func TestCNProviderBalanceService_DeepSeekInvalidBalancePayloadDoesNotBecomeZero(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantError string
	}{
		{
			name:      "missing balance infos",
			body:      `{"data":{"models":["deepseek-v4-flash"]}}`,
			wantError: "missing balance_infos",
		},
		{
			name:      "empty balance infos",
			body:      `{"is_available":true,"balance_infos":[]}`,
			wantError: "no valid balance entries",
		},
		{
			name:      "invalid balance value",
			body:      `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"not-a-number"}]}`,
			wantError: "no valid balance entries",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &cnBalanceProbeRepo{account: newDeepSeekBalanceProbeAccount()}
			upstream := &cnBalanceResponseUpstream{statusCode: http.StatusOK, body: tt.body}
			svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

			result, err := svc.QueryBalance(context.Background(), repo.account.ID)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, result.Success)
			require.Contains(t, result.Error, tt.wantError)
			require.Empty(t, result.Balances)
			require.Empty(t, repo.extraWrites, "invalid relay balance payload must not persist a synthetic zero balance")
		})
	}
}

func TestCNProviderBalanceService_DeepSeekValidZeroBalanceRemainsSuccessful(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newDeepSeekBalanceProbeAccount()}
	upstream := &cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"0"}]}`,
	}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.False(t, result.Available)
	require.Equal(t, "CNY", result.Currency)
	require.Zero(t, result.Balance)
	require.Len(t, result.Balances, 1)
	require.Len(t, repo.extraWrites, 1, "a valid upstream zero balance must still be persisted")
}

func TestKimiInvalidBalanceNeverPersistsZero(t *testing.T) {
	for _, body := range []string{`{"data":`, `{}`, `{"code":1,"data":{"available_balance":0}}`, `{"code":"error","data":{"available_balance":0}}`, `{"code":0.5,"data":{"available_balance":0}}`, `{"data":{"available_balance":"invalid"}}`, `{"data":{"available_balance":"NaN"}}`, `{"data":{"available_balance":"+Inf"}}`} {
		t.Run(body, func(t *testing.T) {
			account := newDeepSeekBalanceProbeAccount()
			account.Platform = PlatformKimi
			repo := &cnBalanceProbeRepo{account: account}
			svc := NewCNProviderBalanceService(repo, nil, &cnBalanceResponseUpstream{statusCode: 200, body: body}, nil)
			result, err := svc.QueryBalance(context.Background(), account.ID)
			require.NoError(t, err)
			require.False(t, result.Success)
			require.NotEmpty(t, result.Error)
			require.Empty(t, repo.extraWrites)
		})
	}
}

type brokenBalanceBody struct{}

func (brokenBalanceBody) Read(p []byte) (int, error) { return copy(p, `{"data":`), io.ErrUnexpectedEOF }
func (brokenBalanceBody) Close() error               { return nil }

type interruptedBalanceUpstream struct {
	cnBalanceResponseUpstream
	calls   int
	recover bool
}

func (u *interruptedBalanceUpstream) Do(r *http.Request, p string, id int64, n int) (*http.Response, error) {
	u.calls++
	if u.recover && u.calls == 2 {
		return u.cnBalanceResponseUpstream.Do(r, p, id, n)
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: brokenBalanceBody{}}, nil
}
func TestKimiInterruptedBalanceRetriesWithoutFalseZero(t *testing.T) {
	for _, recover := range []bool{false, true} {
		account := newDeepSeekBalanceProbeAccount()
		account.Platform = PlatformKimi
		repo := &cnBalanceProbeRepo{account: account}
		upstream := &interruptedBalanceUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{200, `{"code":0,"data":{"available_balance":12}}`}, recover: recover}
		result, err := NewCNProviderBalanceService(repo, nil, upstream, nil).QueryBalance(context.Background(), account.ID)
		require.Equal(t, 2, upstream.calls)
		if recover {
			require.NoError(t, err)
			require.True(t, result.Success)
			require.Equal(t, 12.0, result.Balance)
			require.Len(t, repo.extraWrites, 1)
		} else {
			require.Error(t, err)
			require.Nil(t, result)
			require.Empty(t, repo.extraWrites)
		}
	}
}
