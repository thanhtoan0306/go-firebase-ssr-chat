# Go SSR Chat + Firebase (Firestore)

Server-rendered Go chat UI backed by Firebase Firestore. **No end-user authentication** (the server uses a service account).

## Prereqs

- Go 1.22+ installed
- A Firebase / Google Cloud project with Firestore enabled
- A service account JSON key

## Setup

1) Create a service account key JSON (download from Google Cloud Console) and save it as `serviceAccount.json` in this folder.

2) Export env vars:

```bash
export FIREBASE_PROJECT_ID="your-gcp-project-id"
export GOOGLE_APPLICATION_CREDENTIALS="$PWD/serviceAccount.json"
```

Alternative (no file): put JSON directly into an env var:

```bash
export FIREBASE_PROJECT_ID="your-gcp-project-id"
export FIREBASE_SERVICE_ACCOUNT_JSON='{"type":"service_account", ... }'
```

3) Install deps and run:

```bash
go mod tidy
go run .
```

Open `http://localhost:8080`.

## Firestore data

- Collection: `messages`
- Fields:
  - `author` (string, optional)
  - `text` (string)
  - `createdAt` (timestamp)

## Deploy on Vercel

This repo builds on Vercel's standalone Go runtime. Templates and static files are **embedded** into the binary via `embed.FS`, so no extra config is needed for assets.

In the Vercel project, set these **Environment Variables** (Production / Preview):

- `FIREBASE_SERVICE_ACCOUNT_JSON` — paste the **entire** service-account JSON as the value.
- `FIREBASE_PROJECT_ID` *(optional)* — only needed if it can't be derived from the JSON above.

Then **Redeploy**. The app listens on `$PORT` (provided by Vercel) automatically.

If a request returns 500 with `app init: missing FIREBASE_PROJECT_ID …`, the env vars aren't set on Vercel yet.

## Firestore quota / 500 errors

The UI polls `/messages` for updates. Each poll is a Firestore query, so traffic adds up quickly on the free tier.

This app caches message lists in memory (default **8s**, override with `MESSAGES_CACHE_SECONDS`) and polls every **8s**. If Firestore returns `Quota exceeded`, the server serves the last cached list instead of a 500 when possible.

If you still hit limits: wait for the daily quota reset, enable billing in Google Cloud, reduce open tabs, or increase `MESSAGES_CACHE_SECONDS` and the HTMX `every` interval in `templates/index.html`.

## Notes on “no auth”

This app has **no login**. Anyone who can reach your server can post messages.

If you deploy publicly, add some protection (basic auth, allowlist, rate limits, captcha, etc.).

