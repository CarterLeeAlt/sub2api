# Model Pricing Data

The server embeds `model_prices_and_context_window.json` from this directory. On startup it loads the last usable cached catalog or uses the embedded prices immediately. The configured fallback file fills missing models before the embedded catalog does. The override file has the highest priority.

By default the server checks the [upstream pricing repository](https://github.com/Wei-Shaw/model-price-repo) at startup and every ten minutes. When prices change, it downloads the new catalog and caches it for the next startup. Set `pricing.remote_url` to an empty string for local pricing only.

To update the prices bundled with future builds, edit the local JSON file. It uses the [LiteLLM pricing format](https://github.com/BerriAI/litellm/blob/main/model_prices_and_context_window.json).
