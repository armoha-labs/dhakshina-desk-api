# Dhakshina Desk API

The backend for Dhakshina Desk: one Go binary, built as a modular monolith,
that serves the API for the Android app and the website and runs the
background workers. It owns MongoDB and every call to WhatsApp and the
payment gateways; no client talks to those directly.

The front ends live in a separate repo (`dhakshina-desk`: `mobile/` and
`web/`). The wire contract is that repo's `docs/api-contract.md`; this
service implements it without changes.

## Layout

| Path | What it owns |
| --- | --- |
| `cmd/dhakshina` | `serve`, `indexes`, `admin-hash` |
| `internal/identity` | WhatsApp OTP login, access and refresh tokens, `DELETE /v1/me` |
| `internal/tenancy` | Class setup, editable class fields |
| `internal/plans` | Plan card, limits, read-only and trial rules, entitlements |
| `internal/syncer` | `POST /v1/sync` (push in one transaction, pull by change number), `GET /v1/class` |
| `internal/changelog` | Per-class change numbers, assigned inside the transaction |
| `internal/collections` | Family page read model, "I've paid" claims, consent, data-rights requests, parent links |
| `internal/billing` | Plans, quotes, checkout, Razorpay and Cashfree webhooks, upgrade links |
| `internal/messaging` | Queue worker that sends WhatsApp messages, Meta webhook, parent replies, daily scheduler |
| `internal/jobs` | Jobs collection with leases; written in the same transaction as the change that causes them |
| `internal/admin` | Platform console API; reads only platform collections |
| `internal/platform/*` | Config, tenant-scoped store, field encryption, signed links, JWT, UPI links, HTTP helpers |

Tenant isolation lives in one place: `platform/store`. Every class read and
write goes through `store.For(db, tenantID)`, which adds `tenant_id` to every
filter, insert, update and aggregation. The end-to-end test tries to read and
write across classes.

## Running locally

Transactions need a replica set, so use the compose file (a one-node replica
set plus the API) or any Atlas dev cluster. Never point a dev run at the live
database.

```bash
cp .env.example .env
docker compose up --build        # API on http://localhost:8080
docker compose run --rm indexes  # once per database
```

Or with Go 1.26+ and your own MongoDB:

```bash
export MONGODB_URI='mongodb://localhost:27017/?replicaSet=rs0'
go run ./cmd/dhakshina indexes
go run ./cmd/dhakshina serve
```

Outside production (`APP_ENV` not `production`): OTP codes come back as
`dev_code`, WhatsApp sends are logged instead of sent when `WHATSAPP_TOKEN` is
empty, and `PAYMENT_GATEWAY=mock` completes payments through
`GET /v1/billing/mock-pay`.

Staff login for the admin API:

```bash
go run ./cmd/dhakshina admin-hash 'a long password'
# ADMIN_USERS=you@example.com=<printed hash>
```

Hashes made with the old website script (`npm run admin:hash`) still work.

## Tests

```bash
go test ./...                                                     # compat tests only
MONGODB_TEST_URI='mongodb://localhost:27017/?replicaSet=rs0' go test ./...   # plus end to end
```

The end-to-end test (`internal/server/e2e_test.go`) creates and drops its own
database. It covers login, class setup, sync (all rejection codes, last-writer
wins, partial upserts, receipt numbers, message cancel, gzip), the parent page,
claims and consent, plan limits, the mock checkout, the WhatsApp worker and
status webhook, the daily scheduler, every admin call, the income report,
tenant isolation and account deletion. `internal/compat` checks that phone ciphertexts, blind
indexes, signed links, admin hashes and charge ids made by the Node server
read the same in Go.

## Configuration

All settings come from the environment (see `.env.example`). The MongoDB
connection string is only ever set through the environment or the host's
secret store, never committed. The database follows `APP_ENV`:
`dhakshinadeskdev` on a local replica set in development, and `dhakshina_desk`
in production (`MONGODB_DB` overrides this), on the same
Atlas cluster as Yummverse.

Moving from the Next.js server: reuse its `JWT_SECRET`, `LINK_SECRET` and
`FIELD_ENC_KEY`, or existing sessions, links already sent on WhatsApp and
encrypted phone numbers stop working.

For the admin console, set `MONGODB_ADMIN_URI` to a database user whose role
can read only `tenants`, `plans`, `subscriptions`, `invoices`,
`usage_counters`, `audit_log` and `platform_settings`, insert into
`audit_log`, update `tenants` and write `platform_settings` (the payment
gateway settings, secrets encrypted with `FIELD_ENC_KEY`). The database then refuses student, guardian, ledger and message
reads even if the admin code has a bug.

## Routes

Every route is served at `/v1/...` and also at `/api/v1/...`.

| Route | Who |
| --- | --- |
| `POST /v1/auth/otp/start`, `/otp/verify`, `/refresh`, `/switch`, `/logout`; `DELETE /v1/me` | App and website |
| `POST /v1/tenants`, `POST /v1/sync`, `GET /v1/class` | Teacher (Bearer) |
| `POST /v1/links/parent`, `POST /v1/links/upgrade` | Teacher (Bearer) |
| `GET /v1/parent/{token}`; `POST .../claims`, `.../consent`, `.../requests` | Parent page (signed token) |
| `GET /v1/plans`, `GET /v1/billing/quote`, `POST /v1/billing/checkout`, `GET /v1/billing/invoices/{id}` | Website |
| `POST /v1/billing/webhook/{razorpay,cashfree}` | Gateways |
| `GET, POST /v1/webhooks/whatsapp` | Meta |
| `POST /v1/admin/login`, `/v1/admin/*` | Platform staff (staff token) |
| `GET /v1/health` | Uptime checks |

## Messaging

- The app inserts `messages` rows through sync; the worker sends them and
  writes `sent`, `delivered`, `read` or `failed` back through the change log,
  so the phone sees the status on its next sync. Receipts wait 60 s for Undo.
- Messages are held, not dropped, when the plan does not include WhatsApp
  (trial), outside 8 am to 9 pm IST, or over the monthly quota. A held message
  is released when the plan is paid or changed, or the next day for quota.
- DPDP: reminders and fee notices wait until the guardian consents. The first
  one sends a consent request with "I agree" and "No" buttons; the answer
  (or the consent page) releases them. Receipts for a payment the parent made
  are sent without waiting. Withdrawn consent fails the message.
- The daily scheduler (from 7 am IST, once per day across instances) queues
  automatic reminders for classes whose plan has `reminders`, on the due date,
  3 days after and 10 days after, once each, spread over 9 to 11 am. A class
  opts out with `tenants.auto_reminders: false` (no screen for it yet). It also
  sends the owner a renewal link 3 days and 1 day before the trial ends.
- Parents who write "pay", "balance" or "receipt" to the number get their
  family page link; anything else gets one daily pointer to message the
  teacher directly.

Templates to create in WhatsApp Manager are listed in
`internal/messaging/templates.go`.

## Hosting

The image is a static binary on distroless (about 23 MB) and runs anywhere
that runs a container: `docker build -t dhakshina-desk-api .`, then run with
the environment set. It listens on `$PORT` (default 8080). At launch one
process runs both roles (`ROLE=all`); in production run `ROLE=api` and
`ROLE=worker` from the same image. Point the gateway webhooks at
`https://<api host>/v1/billing/webhook/razorpay` (or `/cashfree`) and Meta's
webhook at `https://<api host>/v1/webhooks/whatsapp`.

### Render

`render.yaml` is a Blueprint that deploys the Docker image as two services —
a web service (`ROLE=api`, health-checked at `/v1/health`) and a worker
(`ROLE=worker`) — sharing one env group so secrets stay identical across both.
The web service runs `dhakshina indexes` as a pre-deploy step on each deploy.

1. In Render, create a Blueprint from this repo (**New → Blueprint**).
2. Fill every `sync: false` value in the `dhakshina-desk` env group: at least
   `MONGODB_URI`, `API_BASE_URL`, `WEB_BASE_URL`, the four secrets, and
   `ADMIN_USERS`. Set `API_BASE_URL` to the web service's own URL
   (`https://dhakshina-desk-api.onrender.com`).
3. Deploy. MongoDB is external — point `MONGODB_URI` at the Atlas cluster;
   Render does not host it.

## Not built yet

- SMS fallback for OTP; Play Integrity check at login.
- Receipt and report PDFs on S3; the "receipt" keyword replies with the page link.
- Co-teacher filtering in sync (G4) and time-limited support access for staff.
- The DPDP erasure and retention jobs: `data_requests` rows are created and
  wait for a handler.
- MongoDB client-side field-level encryption: phones and dates of birth are
  encrypted in the service with AES-256-GCM (the format the Node server used).
- OpenTelemetry traces and a metrics endpoint; logs are structured JSON.
- Google Workspace sign-in for staff (email and password per the website's
  admin pages for now).
