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

## Notes on “no auth”

This app has **no login**. Anyone who can reach your server can post messages.

If you deploy publicly, add some protection (basic auth, allowlist, rate limits, captcha, etc.).

