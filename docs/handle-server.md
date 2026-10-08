# Handle server

Production only. Local dev doesn't need it, and the `handle` service stays off
unless its compose profile is enabled.

Site-specific details live in our internal server documentation repo.

## What it is, and why you need it

Every DSpace item, collection, and community has a handle, e.g.
`<prefix>/12345`. Citations, catalogs, and indexers link to
`https://hdl.handle.net/<prefix>/12345` rather than to your hostname, so the
links survive hostname and server changes.

`hdl.handle.net` doesn't know where items live. Handle resolution:

1. It asks the **global handle registry** who serves your prefix. That's the
   record `0.NA/<prefix>`, which lists an IP address, ports, and a public key.
2. It asks the **handle server at that IP** (i.e., the DSpace handle server),
   over the handle protocol on port 2641 (TCP/UDP) or HTTP on port 8000.
3. The handle server looks the handle up in the DSpace database, using DSpace's
   `HandlePlugin`, and answers with `<dspace.ui.url>/handle/<prefix>/12345`.
4. `hdl.handle.net` redirects the browser there.

So the registry must point at a handle server that is running, reachable, and
attached to your current DSpace database. If the registered server goes away,
**every handle link breaks**.

The handle server doesn't go through any HTTP proxy (SB's Caddy server,
external HAProxy, etc.). It speaks its own protocol and must be reachable from
the internet directly at the registered IP.

You can see your registry record at any time:

```bash
curl -s https://hdl.handle.net/api/handles/0.NA/<prefix> | jq
```

## What's in this repo

- The `handle` service in `compose.yml`, under the `handle` profile. It's the
  REST image running the handle server (`conf/rest/handle-server.sh`) instead
  of Tomcat. It publishes 2641/tcp, 2641/udp, and 8000/tcp.
- The `handle-server` volume (`/usr/local/dspace/handle-server`). It holds the
  server's config and **private keys**.
- `conf/rest/handle-setup.sh`, a variant of DSpace's `make-handle-config` that
  generates or refreshes the config and `sitebndl.zip`, the file handle.net
  needs. It takes the public IP as an argument (a DNS lookup of the site's
  hostname would find external proxies, not the internal server) and keeps any
  existing keys.

The "handle" compose service refuses to start if the volume hasn't been set up,
rather than silently generating keys that handle.net doesn't know about.

## One-time setup

### 1. Network

The handle server needs a stable public IPv4 address that the internet can
reach on **2641/tcp, 2641/udp, and 8000/tcp**. Check the host firewall and any
upstream firewalls. Ports 2641 and 8000 must also be free on the host.

If only TCP can get through (for example, if it's forwarded by a TCP-mode
proxy), that's OK: pass `--no-udp` to the setup script in step 4. UDP must
then *stay* off in the registration, or resolvers will time out on UDP before
falling back to TCP.

### 2. Existing keys (migrations only)

If your prefix is already served by another DSpace install, copy that
server's whole handle directory. Run `dspace dsprop --property handle.dir`
there to find it; it's usually `[dspace]/handle-server`. The important files
are `privkey.bin`, `pubkey.bin`, `admpriv.bin`, and `admpub.bin`.

Reusing the keys means handle.net only has to change the IP. Without them,
setup generates new keys and handle.net has to replace those too. That works,
but it's a bigger change to ask for.

### 3. Storage and config

- Back the `handle-server` volume with a host directory, so the keys are plain
  files you can see and back up (see `compose.override.example.yml`). Put any
  existing keys there. If SELinux blocks the container from reading them,
  label the directory, e.g. `chcon -R -t container_file_t <dir>`.
- In `.env`, set `HANDLE_PREFIX` to your real prefix and add `handle` to
  `COMPOSE_PROFILES`. `MAIL_ADMIN` becomes the contact email in the
  registration.

### 4. Run setup

```bash
docker compose build handle
docker compose run --rm -T handle /usr/local/scripts/handle-setup.sh <public IP>
```

This keeps any existing keys, writes `config.dct`, and creates `sitebndl.zip`.
It's safe to re-run, e.g. to change the IP. A warning about `admin.war` is
expected; it's an optional web admin tool this setup doesn't use.

### 5. Start and check it

```bash
docker compose up -d handle

# On the host
curl -s http://localhost:8000/api/handles/<prefix>/<id>

# From somewhere else, to test the firewall
curl -s http://<public IP>:8000/api/handles/<prefix>/<id>
```

The answer should contain a `URL` value of `<PUBLIC_URL>/handle/<prefix>/<id>`.
That URL comes from `PUBLIC_URL`, so **only do step 6 once this stack is
running with the production `PUBLIC_URL`**. Until then, handles would resolve
to whatever `PUBLIC_URL` is set to, such as a test site.

Handles that don't exist return `"responseCode":100` (not found). That's fine.

### 6. Register with handle.net

A prefix's admin key can only add or delete handles under that prefix; it
can't change the prefix's server address. Handle.net (CNRI) has to make that
change: send them `sitebndl.zip` from the handle directory and ask them to
update the `HS_SITE` for your prefix. The setup tool points to
<http://hdl.handle.net/20.1000/111> for instructions.

Lead time varies, so make contact early. Don't send the final bundle until
step 5 checks out.

### 7. Verify globally

```bash
# Should show this server's IP
curl -s https://hdl.handle.net/api/handles/0.NA/<prefix> | grep '"address"'

# Should redirect to the production site
curl -sI https://hdl.handle.net/<prefix>/<id> | grep -i location
```

Resolvers cache the registry record for up to a day (its TTL is 86400).

## Cutover when migrating

A handle server answers from whatever database it's attached to. Until the
registry changes, the old handle server and its database keep answering. If
their URLs already point at the production hostname, and that hostname now
reaches the new stack, **existing handles keep working through go-live**.
Only items deposited on the new stack after go-live won't resolve until the
registry change lands.

So:

- Keep the old handle server, and the database it reads from, running until
  the registry shows the new IP, plus a day for caches.
- Then shut the old server down.

## Day to day

- **Nothing to do for new items.** Their handles are in the database, so they
  resolve right away.
- **Back up the handle directory.** It holds private keys, and DSpace's own
  backups (database, assetstore) don't cover it. Losing it means another round
  with handle.net.
- **Logs:** `docker compose logs handle`. A steady stream of
  `HandlePlugin @ Called haveNA` lines at INFO is normal. The handle server
  also writes `error.log` in the handle directory.
- **Changing IPs:** re-run setup (step 4) with the new IP, then repeat steps 6
  and 7.

## Testing locally

This isn't normally needed. If you want to try it against an imported
production database, a shell variable overrides `.env` and points the server
at the real prefix:

```bash
export HANDLE_PREFIX=<prefix>
docker compose --profile handle run --rm -T handle \
  /usr/local/scripts/handle-setup.sh 203.0.113.10   # a documentation-only IP
docker compose --profile handle up -d handle
curl -s http://localhost:8000/api/handles/<prefix>/<id>
docker compose --profile handle down
docker volume rm <project>_handle-server   # throwaway keys: delete them
```
