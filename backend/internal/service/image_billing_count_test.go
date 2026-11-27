package service

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImagesBillOnlyDeliveredOutputs(t *testing.T) {
	const model = "gpt-image-2"
	for _, count := range []int{0, 1, 3} {
		for _, byImage := range []bool{true, false} {
			t.Run(fmt.Sprintf("delivered_%d/count_billing_%t", count, byImage), func(t *testing.T) {
				body := []byte(`{"model":"gpt-image-2","prompt":"draw","n":3}`)
				c, rec := newOpenAIImagesTestContext(t, body)
				items := make([]string, count)
				for i := range items {
					items[i] = `{"b64_json":"AA=="}`
				}
				resp := openAIImagesJSONResponse()
				resp.Body = io.NopCloser(strings.NewReader(`{"data":[` + strings.Join(items, ",") + `],"usage":{"input_tokens":4,"output_tokens":8}}`))
				svc := newOpenAIImagesTestService(&httpUpstreamRecorder{resp: resp})
				parsed, err := svc.ParseOpenAIImagesRequest(c, body)
				require.NoError(t, err)
				account := newOpenAIImagesAPIKeyAccount()
				result, err := svc.ForwardImages(t.Context(), c, account, body, parsed, "")
				if count == 0 {
					require.ErrorIs(t, err, errNoDeliveredImage)
					require.Equal(t, 502, rec.Code)
				} else {
					require.NoError(t, err)
					require.Equal(t, 200, rec.Code)
				}
				require.Equal(t, count, result.ImageCount)
				require.Equal(t, 4, result.Usage.InputTokens)
				require.Equal(t, 8, result.Usage.OutputTokens)
				price := 0.25
				groupID := int64(9)
				group := &Group{
					ID: groupID, Platform: PlatformOpenAI, Hydrated: true, RateMultiplier: 1,
					ImageCountBilling: byImage, ImagePrice2K: &price,
					ModelPricing: []ChannelModelPricing{{Models: []string{model}, BillingMode: BillingModeToken, InputPrice: &price, OutputPrice: &price}},
				}
				logs := &openAIRecordUsageLogRepoStub{inserted: true}
				debits := &openAIRecordUsageUserRepoStub{}
				billing := newOpenAIRecordUsageServiceForTest(logs, debits, &openAIRecordUsageSubRepoStub{}, nil)
				billing.resolver = NewModelPricingResolver(nil, billing.billingService)
				err = billing.RecordUsage(t.Context(), &OpenAIRecordUsageInput{Result: result, APIKey: &APIKey{ID: 1, GroupID: &groupID, Group: group}, User: &User{ID: 1}, Account: account})
				require.NoError(t, err)
				require.Equal(t, 1, logs.calls)
				require.Equal(t, count, logs.lastLog.ImageCount)
				require.Equal(t, 4, logs.lastLog.InputTokens)
				require.Equal(t, 8, logs.lastLog.OutputTokens)
				want := 12 * price
				if byImage {
					want = float64(count) * price
				}
				require.Equal(t, want, logs.lastLog.ActualCost)
				require.Equal(t, want, debits.lastAmount)
			})
		}
	}
}

func TestCountOpenAIImageOutputs_OfficialMultiImageResponse(t *testing.T) {
	body := []byte(`{
		"created": 1792815229,
		"data": [
			{"generation_id": "b235a66f-decc-444a-9689-4be8a322a6d0", "url": "https://images.example/a.png"},
			{"generation_id": "de2ccab0-ab61-46bb-9e7b-879af4837dee", "url": "https://images.example/b.png"},
			{"generation_id": "18817363-3449-4217-ba6f-9019ef7fd760", "url": "https://images.example/c.png"}
		],
		"usage": {"input_tokens": 54, "output_tokens": 586, "total_tokens": 640},
		"size": "1024x1024",
		"quality": "low"
	}`)

	if got := countOpenAIResponseImageOutputsFromJSONBytes(body); got != 3 {
		t.Fatalf("countOpenAIResponseImageOutputsFromJSONBytes() = %d, want 3", got)
	}
}
