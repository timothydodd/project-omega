# Omega Analytics

Lightweight, self-hosted, first-party web analytics. **One ~11 MB Go binary, one SQLite file, ~20 MB of RAM.** The dashboard and tracker are embedded in the binary.

- **Cookieless by default**: no consent banner needed. Cookies can be switched on per site, after consent or always.
- **Multiple sites**: each site has its own tracking key, allowed domains and privacy mode.
- **Live view**: who is on the site right now, which page they're on, where they came from.
- **Unique visitors**: rolling 24 hours, 3 days, 7 days and 30 days, each compared with the period before.
- **Traffic**: page views, sessions, bounce rate, visit length, top/entry/exit pages, sources, referrers, UTM campaigns, devices, browsers, OS, countries.
- **Journeys**: every session's pages in order, with time on each page and any events.
- **Users**: `omega.identify()` links visits to your own user ID.
- **Custom events**: `omega.track()` or a `data-omega-event` attribute.

## Run it

Requires **Go 1.26+** to build. The SQLite driver is pure Go, so no C compiler is needed and it cross-compiles to any platform.

```bash
go build -trimpath -ldflags=-s -o omega.exe .   # Windows (use -o omega elsewhere)
./omega.exe                                       # http://localhost:3300
./omega.exe seed                                  # optional: "Demo site" with 90 days of fake traffic
./omega.exe geoip-update                          # optional: download the country database
go test ./...
```

Cross-compile for a Linux server from Windows:

```bat
set GOOS=linux&& set GOARCH=amd64&& go build -trimpath -ldflags=-s -o dist/omega .
```

Open the dashboard. The first visit asks you to create the admin account, then add a site.

| Env var    | Default          | What it does          |
|------------|------------------|-----------------------|
| `PORT`     | `3300`           | HTTP port             |
| `HOST`     | `0.0.0.0`        | Bind address          |
| `OMEGA_DB` | `data/omega.db`  | SQLite database file  |
| `OMEGA_GEOIP_DB` | `dbip-country-lite.mmdb` next to the database | Country database |

To try the tracker locally, open `http://localhost:3300/demo?site=<site key>` (add `&cookies=consent` or `&cookies=always` to test those modes). The site's domains must include `localhost`.

## Add the tracker to a site

```html
<script defer src="https://analytics.yourdomain.com/t.js" data-site="site_xxxxxxxx"></script>
```

```js
omega.track('Signup', { plan: 'pro' });                        // custom event
omega.identify(user.id, { name: user.name, email: user.email }); // after sign-in
```

```html
<button data-omega-event="Download" data-omega-file="guide.pdf">Download</button>
```

To stop counting your own visits, run `localStorage.setItem('omega_ignore', '1')` in your browser's console on the site.

## Privacy modes

Set per site on the Sites page. The snippet shown there matches the mode.

| Mode | Snippet | Stored in the browser | Consent banner |
|---|---|---|---|
| **Cookieless** (default) | *(no extra attribute)* | Nothing | Not needed |
| **Cookies after consent** | `data-cookies="consent"` | Cookies only after `omega.consent(true)` | Your banner calls `omega.consent(true/false)` |
| **Always cookies** | `data-cookies="always"` | First-party cookies from the first page view | Usually needed in the EU/UK |

**How cookieless works.** The server computes `visitor_id = sha256(daily salt + site + IP + user agent)`. The salt is random, lives only in the database, and is replaced every UTC midnight with the old one deleted. After that, nobody (including you) can link IDs across days or reverse them to an IP address.

- Sessions are matched on the server: the same visitor within 30 minutes joins their open session.
- In-app route changes stay in the same session even if the IP changes.

**The trade-off:**
- Today's unique counts and everything per page are accurate.
- Totals over several days count a returning cookieless visitor once per day. The dashboard marks these with **≈**.
- New vs. returning visitors can't be told apart unless the visitor signs in and you call `identify()`.

**Consent mode.** If a visitor accepts part-way through a visit, the cookieless session so far, including its events, is moved to the cookie so it isn't counted twice. `omega.consent(false)` deletes the cookies. If your banner script loads before the tracker, queue calls like this:

```html
<script>window.omega=window.omega||{q:[]};['track','identify','consent'].forEach(function(m){omega[m]=omega[m]||function(){omega.q.push([m,arguments])}})</script>
```

Not legal advice: check the rules that apply to your visitors.

## How it works

```
tracker/t.js   ~2.5 KB gzipped: page views (incl. SPA routes), 15s heartbeat, events, optional cookies
collect.go     ingest: site key + domain check, bot filter, cookie or cookieless identity, sessions
identity.go    daily salt, visitor IP (proxy-aware), cookieless visitor hash
live.go        in-memory presence + Server-Sent Events to open dashboards
stats.go       SQL for the overview, sessions and visitors
public/        dashboard: plain ES modules and SVG charts, embedded into the binary
```

- **Live** means a heartbeat in the last 60 seconds. Background tabs stop sending heartbeats and drop off.
- **Time on page** counts visible time only.
- **Bots**: known bot user agents and automated browsers (`navigator.webdriver`) are ignored.
- **Country**: see [Countries](#countries).

## Container image

The **Container** workflow (`.github/workflows/container.yml`) runs `gofmt`, `go vet` and `go test`, then builds a multi-arch image (`linux/amd64`, `linux/arm64`).
- It pushes the image to `ghcr.io/timothydodd/project-omega` on every push to `main` (tags `main`, `latest`, `sha-<short>`) and on `v*` tags (tag `1.2.3`).
- Pull requests build without pushing.
- It also rebuilds on the 3rd of each month to pick up the new country database.

The image is distroless, runs as non-root and includes the DB-IP country database. Data lives in `/data`.

```bash
docker run -p 3300:3300 -v omega-data:/data ghcr.io/timothydodd/project-omega:main
```

## Deploying to k3s

Manifests are in `deploy/k3s` (Kustomize): namespace, 2 Gi `local-path` volume, a single-replica Deployment, Service, and a Traefik Ingress.

1. **Pull secret.** The repo is private, so its image is too. Create a GitHub token with `read:packages`, then:
   ```bash
   kubectl create namespace omega
   kubectl -n omega create secret docker-registry ghcr-pull \
     --docker-server=ghcr.io --docker-username=timothydodd --docker-password=<token>
   ```
2. **Hostname.** Edit `deploy/k3s/ingress.yaml`: set your host, and uncomment the TLS lines if you use cert-manager.
3. **Keep visitors' real IPs** (once per cluster). k3s's Traefik hides them by default, which would make every cookieless visitor look the same and break country lookup:
   ```bash
   kubectl apply -f deploy/k3s/traefik-client-ip.yaml
   ```
   You can skip this if the site sits behind Cloudflare, because Omega reads `CF-Connecting-IP`.
4. **Deploy:**
   ```bash
   kubectl apply -k deploy/k3s
   kubectl -n omega rollout status deploy/omega
   ```
5. Open the hostname right away and create the admin account. The first visitor to an empty install becomes the admin.

**Updating.** The Deployment uses the `main` tag with `imagePullPolicy: Always`, so pick up a new build with:
```bash
kubectl -n omega rollout restart deploy/omega
```
For pinned releases, set `newTag` in `deploy/k3s/kustomization.yaml` to a `sha-…` or version tag.

**Notes**
- It runs one replica with the `Recreate` strategy on purpose: SQLite allows a single writer.
- Back up the `omega-data` volume. With `local-path` it's a folder under `/var/lib/rancher/k3s/storage` on the node.
- The login cookie is marked Secure when Traefik sends `X-Forwarded-Proto: https`.

## Countries

Country comes from a CDN header when one is present (`CF-IPCountry`, `X-Vercel-IP-Country`). Otherwise it's looked up in [DB-IP's free IP-to-Country Lite database](https://db-ip.com/db/download/ip-to-country-lite) (CC BY 4.0; the dashboard shows the required attribution).
- The IP is used only for the lookup and is never stored.
- The container image includes the database.
- Running the binary directly? Download it with `omega geoip-update`, which saves it next to the database, or point `OMEGA_GEOIP_DB` at it. Restart to load a new month's file.

## Not built yet

- Session recording and replay (the plan is rrweb, stored as compressed chunks next to the database)
- Funnels, goals and retention charts
- Multiple dashboard users and per-site permissions
- Data retention settings
