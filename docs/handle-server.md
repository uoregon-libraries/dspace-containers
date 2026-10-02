# Handle server

Production only. Local dev doesn't need it, and the `handle` service stays off
unless its compose profile is enabled.

## What it is, and why we need it

Every item, collection, and community has a handle, e.g. `1794/24587`.
Citations, catalogs, and indexers link to `https://hdl.handle.net/1794/24587`,
not to our hostname, so the links keep working when we move.

`hdl.handle.net` doesn't know where our items live. It resolves a handle like
this:

1. It asks the **global handle registry** who serves prefix `1794`. That's
   the record `0.NA/1794`, which lists an IP address and a public key.
2. It asks the **handle server at that IP**, over the handle protocol on port
   2641 (TCP/UDP) or HTTP on port 8000.
3. That server looks the handle up in the **DSpace database**, using DSpace's
   `HandlePlugin`, and answers with `<dspace.ui.url>/handle/1794/24587`.
4. `hdl.handle.net` redirects the browser there.

Today (October 2026), the registry points at sbprod1 (`184.171.1.3`). When
sbprod1 is shut off, **every handle link breaks**. Getting step 1 to point at
sbprod2 requires two things: running a handle server on sbprod2 (the `handle`
service), and asking handle.net to update the registry.

The handle server doesn't go through Caddy. It speaks its own protocol and must
be reachable from the internet, directly at the registered IP.

You can see the registry record at any time:

```bash
curl -s https://hdl.handle.net/api/handles/0.NA/1794 | python3 -m json.tool
```

## What's in this repo

- The `handle` service in `compose.yml`, under the `handle` profile. It's the
  REST image running the handle server (`conf/rest/handle-server.sh`) instead
  of Tomcat. It publishes 2641/tcp, 2641/udp, and 8000/tcp.
- The `handle-server` volume (`/usr/local/dspace/handle-server`). It holds the
  server's config and **private keys**.
- `conf/rest/handle-setup.sh`, which generates or refreshes the config and
  `sitebndl.zip`, the file handle.net needs.

The handle server refuses to start if the volume hasn't been set up, rather
than silently generating keys that handle.net doesn't know about.

## One-time production setup

### 1. Network

The handle server needs a stable public IPv4 address that the internet can
reach on **2641/tcp, 2641/udp, and 8000/tcp**. Check the host firewall and any
campus or network firewalls. Port 8000 must also be free on the host.

If only TCP can get through (for example, if it's forwarded by a TCP-mode
proxy), that's OK: pass `--no-udp` to the setup script in step 4. UDP must
then *stay* off in the registration, or resolvers will time out on UDP before
falling back to TCP.

### 2. Get sbprod1's keys

Copy sbprod1's whole handle directory. Run
`dspace dsprop --property handle.dir` there to find it, but it's usually
`[dspace]/handle-server`. The important files are `privkey.bin`, `pubkey.bin`,
`admpriv.bin`, and `admpub.bin`.

Reusing the keys means handle.net only has to change the IP. If they're lost,
setup generates new ones and handle.net replaces the keys too. That works, but
it's a bigger change to ask for.

### 3. Storage and config

- Back the `handle-server` volume with a host directory, so the keys are plain
  files you can see and back up. See `compose.override.example.yml`. Put
  sbprod1's files there. If SELinux blocks the container from reading them,
  label the directory: `chcon -R -t container_file_t /opt/dspace/handle-server`.
- In `.env`, set `HANDLE_PREFIX=1794` and add `handle` to `COMPOSE_PROFILES`.
  `MAIL_ADMIN` becomes the contact email in the registration.

### 4. Run setup

```bash
docker compose build handle
docker compose run --rm -T handle /usr/local/scripts/handle-setup.sh <public IP>
```

This keeps any existing keys, writes `config.dct`, and creates `sitebndl.zip`.
You can safely re-run it, for example to change the IP. A warning about
`admin.war` is expected; it's an optional web admin tool we don't use.

### 5. Start and check it

```bash
docker compose up -d handle

# On the host
curl -s http://localhost:8000/api/handles/1794/24587

# From somewhere else, to test the firewall
curl -s http://<public IP>:8000/api/handles/1794/24587
```

The answer should contain the `URL`
`https://scholarsbank.uoregon.edu/handle/1794/24587`. That URL comes from
`PUBLIC_URL`, so **only do step 6 once this stack is running with the
production `PUBLIC_URL`**. Until then, handles would resolve to whatever
`PUBLIC_URL` is set to, such as the test site.

Missing handles return `"responseCode":100` (not found). That's fine.

### 6. Ask handle.net to update the registry

Our prefix's admin key can only add or delete handles under `1794`; it can't
change the server address. So handle.net (CNRI) has to make this change. Send
them `sitebndl.zip` from the handle directory and ask them to update the
`HS_SITE` for prefix 1794. The setup tool's own instructions point to
<http://hdl.handle.net/20.1000/111>, and questions go to
hdladmin@cnri.reston.va.us.

The prefix's registered contacts are libsys@uoregon.edu and
akurzhal@uoregon.edu. Whoever handled sbprod1's registration in September 2024
has been through this before, and probably has the handle.net account.

Lead time is unknown, so make contact early. Don't send the final bundle until
step 5 is true.

### 7. Verify globally

```bash
# Should show sbprod2's IP
curl -s https://hdl.handle.net/api/handles/0.NA/1794 | grep '"address"'

# Should redirect to the production site
curl -sI https://hdl.handle.net/1794/24587 | grep -i location
```

Resolvers cache the registry record for up to a day (its TTL is 86400).

## Cutover timing

A handle server answers from whatever database it's attached to. Before the
registry changes, sbprod1's handle server and Postgres keep answering. Their
URLs point at `scholarsbank.uoregon.edu`, which HAProxy sends to sbprod2, so
**existing handles keep working through go-live**. Only items deposited on
sbprod2 after go-live won't resolve until the registry change lands.

So:

- Keep sbprod1's handle server, and the Postgres it reads from, running
  until the registry shows the new IP, plus a day for caches.
- Then shut sbprod1 down.

## Day to day

- **Nothing to do for new items.** Their handles are in the database, so they
  resolve right away.
- **Back up the handle directory.** It holds private keys, and the DSpace
  backup scripts don't cover it. Losing it means another round with
  handle.net.
- **Logs:** `docker compose logs handle`. A steady stream of
  `HandlePlugin @ Called haveNA` lines at INFO is normal. The handle server
  also writes `error.log` in the handle directory.
- **Changing IPs:** re-run setup (step 4) with the new IP, then repeat steps 6
  and 7.

## Testing locally

This isn't normally needed. If you need to try it, the shell variable
overrides `.env` and points the server at the prefix in an imported production
DB:

```bash
export HANDLE_PREFIX=1794
docker compose --profile handle run --rm -T handle \
  /usr/local/scripts/handle-setup.sh 203.0.113.10   # a documentation-only IP
docker compose --profile handle up -d handle
curl -s http://localhost:8000/api/handles/1794/24587
docker compose --profile handle down
docker volume rm scholarsbank_handle-server  # throwaway keys: delete them
```
