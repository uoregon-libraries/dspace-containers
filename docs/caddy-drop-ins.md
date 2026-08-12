# Caddy drop-in rules

One-off Caddy rules (emergency bot blocks, temporary redirects, etc.) can be
added to a running deployment without rebuilding images or redeploying:
create a file, restart (or reload) the `web` service, done.

## How it works

The `web` container mounts the `caddy-conf` named volume (read-only) at
`/etc/caddy/conf.d`, and the Caddyfile imports two globs from it:

- `*.site.caddyfile` — rules imported **inside the public listener**, e.g.,
  `abort`, `handle`, `redir`. These are imported *before* the baked-in rules,
  so an emergency `handle` can preempt bot protection. Within the glob, files
  apply in lexical order — use numeric prefixes (`00-`, `10-`, ...) when
  ordering matters.
- `*.server.caddyfile` — imported at the **top level** of the Caddyfile, for
  whole new listeners / site blocks.

Core rules (the TPS bot-protection snippets) are baked into the image at
`/etc/caddy/core.d` and can never be affected by volume operations. The
image's `/etc/caddy/conf.d` is intentionally empty — podman seeds a new named
volume from the image's directory content (copy-up), and keeping the mount
point empty guarantees that deleting the `caddy-conf` volume only ever
removes custom one-off rules, never core config.

## Adding or changing a rule

Production should back the volume with a host directory (see
`compose.override.example.yml`), so rules are just files:

```bash
$EDITOR /var/local/dspace/caddy-conf.d/00-my-rule.site.caddyfile
podman compose restart web
```

Or, for a zero-downtime apply (validate first!):

```bash
podman compose exec web caddy validate --config /etc/caddy/Caddyfile
podman compose exec web caddy reload --config /etc/caddy/Caddyfile
```

Removing a rule is deleting the file plus the same restart/reload.

If you *don't* have a host-directory override (e.g., dev with the plain named
volume), you can still get at the files rootless:

```bash
podman volume inspect <project>_caddy-conf --format '{{.Mountpoint}}'
podman unshare ls <that path>
```

## Example: blocking bad networks and bots

A real rule set production has used, as a template
(`00-bad-networks.site.caddyfile`):

```caddyfile
# Huawei cloud giving us trouble. Block them for now, drop this eventually.
@blocked client_ip 114.119.141.0/24 114.119.153.0/24
abort @blocked

# Bad bots: we probably want a single rule for these but I need to do some
# research. For now, one rule per bot. These rules must *never* be applied to
# well-behaved bots. The bad bots are those that aren't respecting our sitemap
# **and** are grabbing things with no throttling.
@badbot001 {
	header_regexp User-Agent (?i)github\.com/rom1504/img2dataset
}
@badbot002 {
	header_regexp User-Agent (?i)PetalBot
}
abort @badbot001
abort @badbot002
```

## Caveats

- Compose reads a volume's `driver_opts` only when it **creates** the volume.
  To change the `device` path later: stop the stack, `podman volume rm
  <project>_caddy-conf`, and bring it back up. Files in a host-backed
  directory are untouched by this — only the volume object is removed.
- The host directory must exist before `podman compose up`, or volume
  creation fails.
- On a *plain* named volume (no host-dir override), `podman volume rm` or
  `down -v` deletes your drop-in files. Core rules are never at risk — they
  live in the image.
- Drop-in files are visible read-only inside the container; edit them from
  the host side.
