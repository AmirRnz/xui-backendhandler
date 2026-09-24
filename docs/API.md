# Backend API contract (v1)

All routes except `/healthz` require `Authorization: Bearer <deployment-scoped-token>`. Tokens bind to one deployment. User routes also require `X-Actor-Telegram-ID: <telegram user id>`; the backend resolves it inside that deployment and checks account ownership and role. IDs in request bodies and callback data do not grant permission. Responses are JSON. Errors have `{"error":{"code":"...","message":"..."}}` shape.

## User operations

| Method and path | Request | Result / rule |
|---|---|---|
| `GET /healthz` | none | `{"status":"ok"}`; no client auth |
| `POST /v1/actors/resolve` | `{"telegram_id":int64}` | Creates or returns only the actor within the credential's deployment |
| `GET /v1/me` | actor header | Role, approval state, and channel |
| `GET /v1/features` | actor header | Deployment feature flags and user-facing text; unset feature flags default to enabled |
| `GET /v1/plans?kind=paid\|test` | actor header | Only enabled plans in this deployment that are global or granted through `plan_access`; panel IDs and inbound IDs are not exposed |
| `POST /v1/quotes` | `plan_id, months, ip_limit, data_gb, idempotency_key` | Immutable integer-Toman quote with plan/term snapshots; `data_gb:0` means unlimited where the plan permits it |
| `POST /v1/purchases` | `quote_id, payment_method(wallet\|direct), idempotency_key, display_name` | Creates one order. Wallet purchases debit and queue provisioning atomically; direct purchases create a payment intent and wait for review |
| `POST /v1/trials` | `plan_id, idempotency_key` | Reserves one trial and durable provisioning work under the deployment's retail cooldown or reseller UTC quota policy |
| `GET /v1/subscriptions` | actor header | Only subscriptions owned by the resolved account |
| `POST /v1/subscriptions/{id}/cancel` | `idempotency_key` | Ownership-checked request; deletion is durable backend work |
| `POST /v1/subscriptions/{id}/refunds` | `idempotency_key, reason` | Requests a refund. Cap comes from immutable purchase terms; old subscriptions without those terms need an audited manual override |
| `GET /v1/wallet` | actor header | Current Toman balance |
| `GET /v1/wallet/ledger` | actor header | Audited, account-scoped ledger entries |
| `POST /v1/wallet/topups` | `amount_toman, idempotency_key` | Creates one top-up request |
| `POST /v1/wallet/topups/{id}/receipt` | `telegram_file_id` | Submits a receipt for the owned request |
| `POST /v1/payment-intents/{id}/receipt` | `telegram_file_id` | Submits a receipt for the owned payment intent |
| `GET /v1/payment-instructions` | actor header | Deployment-specific instructions; card values are configured in the database |

## Admin operations

The designated admin identity and deployment scope are checked in the backend on every admin or review call; a bot-supplied role is never trusted.

| Method and path | Request | Result |
|---|---|---|
| `GET /v1/admin/payments` | none | Receipt-submitted payment intents in the deployment |
| `POST /v1/payment-intents/{id}/approve` | empty object | Approves the immutable amount once, writes settlement and durable provisioning work |
| `GET /v1/admin/topups` | none | Receipt-submitted wallet top-ups in the deployment |
| `POST /v1/wallet/topups/{id}/approve` | empty object | Credits the wallet once with an audit ledger entry |
| `GET /v1/admin/refunds` | none | Pending refund requests in the deployment |
| `POST /v1/admin/refunds/{id}/approve` | `amount_toman, audit_note, idempotency_key, manual_override` | Credits only after verified cancellation and within the immutable paid cap, unless an explicit reasoned manual override is recorded |
| `GET /v1/admin/work-items` | none | Durable work needing operational attention |
| `GET /v1/admin/config` | admin actor header | Deployment-scoped plan catalog, payment instructions, settings, and panel URL/configured status; never includes a panel token |
| `GET /v1/admin/resellers/pending` | admin actor header | Pending reseller accounts in this reseller deployment only |
| `POST /v1/admin/resellers/{telegram_id}/approve` | `{}` | Approves a pending reseller in this deployment; action and admin actor are audited in the same transaction |
| `POST /v1/admin/resellers/{telegram_id}/reject` | `{}` | Rejects a reseller in this deployment; action and admin actor are audited in the same transaction |
| `POST /v1/admin/config/plans` | full plan object, without `id` | Creates a deployment-owned plan and returns `{id,config}` |
| `PUT /v1/admin/config/plans/{id}` | full plan object | Updates only a plan owned by this deployment and returns refreshed config |
| `PATCH /v1/admin/config/payment-instructions` | `card_number, card_owner, instructions` | Replaces this deployment's payment instructions |
| `PATCH /v1/admin/config/settings` | any subset of `retail_trial_reset_days, unapproved_trial_daily_limit, reseller_approved_required, features, text` | Updates trial controls, reseller approval requirement, feature flags, and user-facing text |
| `PUT /v1/admin/config/panel` | `base_url, token` | Updates the deployment's default panel. Token is encrypted at rest and never returned; requires `BACKEND_PANEL_SECRETS_KEY` |

Admin configuration writes are scoped to the credential's deployment and recorded atomically in `admin_configuration_audit`. `subject_ref` identifies plan mutations as `plan:<id>` and reseller decisions as `telegram:<id>`; secrets and setting values are never recorded. The Telegram ID `96937669` is seeded as the administrator only for `retail-finland` and `reseller-turk1`; it does not gain access to Germany. Feature keys are deployment-specific strings with boolean values. Supported backend gates include `purchases_enabled`, `trials_enabled`, `wallet_enabled`, `topups_enabled`, and `direct_payments_enabled`; missing keys remain enabled.

The backend has no public “set wallet” operation. Financial writes use audited credit/debit ledger entries and bounded idempotency keys. Repeating a key with different input conflicts.

## 3x-ui integration behavior

Only the backend worker calls 3x-ui. API token material comes from `XUI_PANEL_TOKENS_JSON`, keyed by panel ID. The worker requires version `3.8.5`; client updates read and preserve the full remote row. Adds/updates/attaches that are partial or ambiguous are read back and reconciled before any retry. Provisioning obligations are committed before the external call.
