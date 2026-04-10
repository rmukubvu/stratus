## Stratus Portal

This is the local operator portal for `stratus`.

It is intentionally a read-only cockpit, not an AWS Console clone. The portal
connects to the Stratus operator API and shows:
- overview and request health
- recent activity
- recent failures
- CloudWatch-style log browsing

## Run It Against Stratus

Start a real Stratus instance first:

```bash
cd /Users/robson/awsdev/stratus-git
go build -o /tmp/stratus-bin ./cmd/stratus
/tmp/stratus-bin --port 4566 --data-dir /tmp/stratus-data --log-format pretty --log-level debug
```

Then start the portal and point it at that Stratus instance:

```bash
cd /Users/robson/awsdev/stratus-git/portal
STRATUS_BASE_URL=http://127.0.0.1:4566 npm run dev
```

Open [http://localhost:3000](http://localhost:3000).

## Important Note

If `STRATUS_BASE_URL` points at the wrong process, the portal will not work. The
common failure mode is pointing it at another local AWS emulator on `:4566`.

When that happens, the portal now shows a connection card instead of crashing.

## Build

```bash
npm run build
```

## Current Pages

- `/` overview
- `/activity`
- `/errors`
- `/logs`
