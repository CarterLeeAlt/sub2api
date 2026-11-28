package openai

import "github.com/tidwall/gjson"

// CacheWriteUsage reads the aggregate and the two lifetime buckets reported
// by the upstream. Incomplete or inconsistent buckets use the aggregate price.
func CacheWriteUsage(usage gjson.Result) (total, short, long int, reported bool) {
	for _, key := range []string{"input_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_write_tokens", "input_tokens_details.cache_creation_tokens", "prompt_tokens_details.cache_creation_tokens"} {
		if value := usage.Get(key); value.Exists() {
			total, reported = max(0, int(value.Int())), true
			break
		}
	}
	if !reported {
		for _, key := range []string{"cache_write_tokens", "cache_creation_input_tokens", "cache_write_input_tokens", "cache_creation_tokens"} {
			value := usage.Get(key)
			reported = reported || value.Exists()
			if value.Int() > 0 {
				total = int(value.Int())
				break
			}
		}
	}
	for _, key := range []string{"input_tokens_details.cache_write_tokens_details", "prompt_tokens_details.cache_write_tokens_details"} {
		details := usage.Get(key)
		if !details.Exists() {
			continue
		}
		a, b := details.Get("ttl_30m_tokens"), details.Get("ttl_12h_tokens")
		if a.Type == gjson.Number && b.Type == gjson.Number && a.Int() >= 0 && a.Int() <= int64(total) && b.Int() == int64(total)-a.Int() && float64(a.Int()) == a.Float() && float64(b.Int()) == b.Float() {
			short, long = int(a.Int()), int(b.Int())
		}
		break
	}
	return total, short, long, reported
}
