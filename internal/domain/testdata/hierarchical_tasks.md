# Tasks: Project Public Link

Implementation checklist grounded in the approved proposal.md and requirements.md delta. Tasks are ordered by dependency: platform signing first, then webapp persistence/binding, then the worker public branch, then the client, then the share UI, and finally the public page + shared-viewer refactor. Each task cites the requirement codes it satisfies.

## 1. Platform — Batch signing endpoint (Go)

- [ ] 1.1 Add a domain method to sign an explicit set of `{fileID, revisionNumber}` with a caller-supplied `expiry`, generalizing the existing `GetSpecLinksForIDs` in `internal/modules/project/domain/file_service.go` / `domain/service.go`. (AR-PLAT-1, AR-PLAT-4)
- [ ] 1.2 For each pair, route latest-revision requests to the R2 presigned path (`GetDownloadURL` → `storage.GenerateSignedURL(file.FileURI, expiry)`) and historical-revision requests to `GetRevisionDownloadURL`. (AR-PLAT-4, AR-PLAT-5, AR-PLAT-6)
- [ ] 1.3 Change `GetRevisionDownloadURL` to accept and honor a caller-supplied expiry instead of the fixed `revisionURLTTL = 1h`. (CR-PLAT-7)
- [ ] 1.4 Clamp `expiry` to a 7-day (604800s) maximum and default to 7 days when omitted. (AR-PLAT-2, AR-PLAT-3)
- [ ] 1.5 Authorize every requested `fileID` against the caller's org; reject the whole request if any file is inaccessible. (AR-PLAT-8)

## 2. Webapp — Cloudflare D1 binding & schema

- [ ] 2.1 Add a `[[d1_databases]]` binding for each environment (top-level/local, `env.dev`, `env.staging`, `env.production`) in `wrangler.toml`. (AR-D1-1)
- [ ] 2.2 Reflect the D1 binding in the worker env types (`app/env.d.ts` `WindowEnv` is client-only — add to the worker `Env`/`WorkerEnv` in `~/shared/env` and the `react-router` `AppLoadContext.cloudflare.env`). (AR-D1-1)

## 3. Webapp Worker — Public API branch (`workers/app.ts`)

- [ ] 3.1 Add an `/api/public/*` branch dispatched BEFORE the authenticated `/api/v1/*` block, wired to a new handler that reads/writes D1 directly (no auth pass-through). (AR-WW-1)
- [ ] 3.2 Add a public CORS policy for `/api/public/*` appropriate to anonymous cross-origin visitors, distinct from the credentialed `/api/v1/*` policy. (AR-WW-11)
- [ ] 3.3 Implement unauthenticated `GET /api/public/links/:token`: return project header + shared files (metadata + signed URLs); return 404/410 when `status='revoked'` or now > `expires_at`. (AR-WW-2, AR-WW-3)
- [ ] 3.4 Implement authenticated `POST /api/public/links`: enforce the files-per-link cap, call the platform batch signing method with a 7-day expiry, generate a crypto-random token, set `expires_at = created_at + 7d`, insert `PUBLIC_LINK` + `PUBLIC_LINK_FILE` rows, return `/p/:token`. (AR-WW-4, AR-D1-6, AR-D1-7, AR-D1-8)

## 8. Cross-cutting verification

- [ ] 8.1 Confirm no public-link lifetime exceeds the 7-day signed-URL ceiling anywhere in create/regenerate. (AR-X-1)
- [ ] 8.5 Add end-to-end coverage: create → open public link anonymously → revoke → confirm public read now refused; and latest-live vs pinned-frozen content behavior after an edit. (AR-PLAT-5, AR-PLAT-6, AR-WW-3, AR-WW-7)
