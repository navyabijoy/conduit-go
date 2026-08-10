# Conduit

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**A self-hosted integration gateway for Slack, GitHub, Stripe, and other third-party APIs — so you stop re-writing OAuth refresh, credential encryption, and rate-limit handling for every connector.**

Every team that integrates third-party APIs ends up building the same boilerplate: OAuth token exchange and refresh, encrypted credential storage, retry/backoff tuned to each provider's rate-limit format, webhook signature verification, and some way to notice when a provider's response shape quietly changes underneath you. Conduit gives you that layer as a small Go service you run yourself — no SaaS subscription, no sending your customers' credentials to a third party.

## Use cases

- **Your SaaS needs customers to connect their own Slack/GitHub/Stripe.** Instead of hand-rolling OAuth install flows and encrypted token storage per provider, point customers at Conduit's install endpoint and let it handle the OAuth dance, token refresh, and credential encryption.
- **You're calling a rate-limited third-party API and tired of writing backoff logic.** Conduit's HTTP client already knows Slack's and GitHub's rate-limit header formats and backs off correctly; call through it instead of re-deriving that per project.
- **You receive webhooks from a provider and need signature verification done correctly.** The HMAC helpers in `sdk/webhook.go` handle this once, instead of once per integration.
- **You want to know when a provider's API response shape changes** before it breaks your integration silently — the drift detector flags it.
- **You don't want a third party holding your customers' OAuth tokens.** Self-hosted means the credentials never leave infrastructure you control.

## Why self-hosted instead of Nango/Merge/Paragon

Those are good products with far more pre-built connectors than Conduit has today (3, vs. their hundreds). The tradeoff is control and cost: no per-API-call pricing, no vendor holding your customers' credentials, and no dependency on a third party staying in business. If you need broad, ready-made connector coverage right now, they're the better fit. If you specifically need Slack/GitHub/Stripe (or want to add your own connector against a straightforward Go interface) and would rather self-host, that's what Conduit is for.

## Try it in 30 seconds

No API keys, no signup, no config. Conduit ships with a built-in mock provider so you can see the whole flow — install a connector, watch it authenticate, execute an endpoint — against fake Slack/GitHub/Stripe backends.

```bash
git clone https://github.com/navyabijoy/conduit-go.git
cd conduit-go
docker build -t conduit .
docker run -p 8080:8080 -p 8081:8081 conduit
```

or without Docker:

```bash
go run ./cmd/gateway
```

Then open **http://localhost:8080/ui/** — install a connector from the catalog, install it, and execute a request against the mock backend, all from the dashboard.

## Walkthrough: your first connector

1. **Catalog tab** — you'll see Slack, GitHub, and Stripe listed, each with the scopes they request. Click **Install** on Stripe (simplest, since it's API-key auth, not OAuth).
2. A modal asks for an API key. Since you're running in mock mode, any non-empty value works (e.g. `sk_test_demo`) — it's validated against the mock Stripe backend, not the real one.
3. Conduit encrypts that key (AES-256-GCM) and stores it, then switches you to the **Instances** tab where your new Stripe instance shows status `active`.
4. Click **Execute** on that instance, or go to the **Execute** tab directly — pick the instance, pick an endpoint (e.g. `list_charges`), and send the request. You'll see the raw response from the mock Stripe backend.
5. Try **Install** on Slack or GitHub instead — those use OAuth2, so you're redirected through a consent flow (against the mock provider) rather than typing a key, landing on a success page once the token exchange completes.
6. Check the **Metrics** tab for links to `/metrics` (Prometheus) and `/docs` (OpenAPI/Swagger UI) — both reflect whatever's actually registered, not a static example.

<img width="1792" height="1026" alt="image" src="https://github.com/user-attachments/assets/144ad3a8-d62f-4792-b7f2-c2b551b478d0" />

<img width="1789" height="1027" alt="image" src="https://github.com/user-attachments/assets/43af3700-c440-43cc-80b8-8f996154ae65" />

<img width="1788" height="1027" alt="image" src="https://github.com/user-attachments/assets/a03be72e-94bf-450f-9815-8d888b73e152" />


## What it actually does

- **Unified connector interface** (`sdk.Connector`) — one Go interface (`AuthConfig`, `Endpoints`, `HandleWebhook`) that Slack, GitHub, and Stripe connectors implement today; add your own by implementing the same interface.
- **OAuth2 + API-key auth, handled for you** — automatic token refresh on expiry or `401`, with credentials encrypted at rest using AES-256-GCM (`gateway/db.go`).
- **Provider-aware rate-limit handling** — the HTTP client (`sdk/client.go`) understands Slack's `Retry-After` header and GitHub's `X-RateLimit-Remaining`/`X-RateLimit-Reset` headers, with exponential backoff and jitter for everything else.
- **Webhook signature verification** — HMAC-SHA256 helpers (`sdk/webhook.go`) so inbound webhooks are verified before they're processed.
- **Drift detection** — a background worker periodically probes each installed connector, diffs the response shape against a stored baseline, and flags/alerts when a provider's API quietly changes.
- **Observability out of the box** — Prometheus metrics (`/metrics`) for request latency, error rates, token refresh outcomes, and drift detections; an auto-generated OpenAPI spec and Swagger UI (`/docs`) reflected from each connector's declared endpoints.
- **Dashboard UI** — a lightweight, no-build-step dashboard (`ui/`) for browsing the connector catalog, managing installed instances, executing endpoints, and checking observability links.

## Architecture

```
┌──────────┐   installs / executes   ┌────────────────────┐
│ Dashboard│ ───────────────────────▶│   Conduit Gateway   │
│  (ui/)   │◀─────────────────────── │  (gateway/, sdk/)   │
└──────────┘                          └─────────┬──────────┘
                                                 │
                        ┌────────────────────────┼────────────────────────┐
                        ▼                        ▼                        ▼
                 ┌─────────────┐          ┌─────────────┐          ┌─────────────┐
                 │   Slack     │          │   GitHub    │          │   Stripe    │
                 │ connector   │          │  connector  │          │  connector  │
                 └─────────────┘          └─────────────┘          └─────────────┘
                        │                        │                        │
                        └────────── real API, or the built-in ────────────┘
                                     mock server (mock/) for local dev
```

Credentials are encrypted with AES-256-GCM and persisted in SQLite (`gateway/db.go`) — no external database required to run it.

## Configuration

Copy `.env.example` to `.env` for a real (non-demo) deployment — see that file for what each variable does. Nothing in it is required to try Conduit locally; the demo path above runs with zero configuration.

## Project layout

| Path | What's there |
|---|---|
| `sdk/` | The core client library — HTTP client with retries/backoff/rate-limiting, auth types, the `Connector` interface, error classification, pagination, webhook verification. |
| `connectors/` | Per-service implementations of `sdk.Connector` (`github/`, `slack/`, `stripe/`). |
| `gateway/` | The HTTP API server — connector catalog, OAuth install flow, instance management, webhook receiver, drift detector, metrics, OpenAPI spec. |
| `mock/` | A mock third-party server standing in for real Slack/GitHub/Stripe APIs, used by the demo path and tests. |
| `ui/` | The dashboard — plain HTML/CSS/JS, no build step. |
| `cmd/gateway/` | The gateway binary entrypoint. |

## Adding a connector

Implement `sdk.Connector` (see `connectors/slack/` for a complete example) and register it in `gateway/server.go`'s `NewGateway`. `cmd/conduit-scaffold` can generate the boilerplate for a new connector package.

## License

MIT — see [LICENSE](LICENSE).
