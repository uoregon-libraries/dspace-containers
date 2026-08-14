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

Core rules (e.g., bot protection) are baked into the image to avoid accidental
breaking when customizing one-offs.

## Adding or changing a rule

Production should back the volume with a host directory (see
`compose.override.example.yml`), so rules are just files:

```bash
# Create / edit a rule
$EDITOR /var/local/dspace/caddy-conf.d/00-my-rule.site.caddyfile

# Make sure Caddy config validates, then restart it
podman compose exec web caddy validate --config /etc/caddy/Caddyfile
podman compose restart web

# Or do a zero-downtime reload
podman compose exec web caddy reload --config /etc/caddy/Caddyfile
```

If you *don't* have a host-directory override (e.g., dev with the plain named
volume), you can still get at the files rootless via `podman unshare` (this
command is new to me, so look it up if you need to use it!)

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

# Bad bot! No biscuit! This app seems to get abused by well-intentioned people
# who just aren't aware how expensive it is on servers.
@badbot {
	header_regexp User-Agent (?i)github\.com/rom1504/img2dataset
}
abort @badbot
```

## Caveats

Compose only uses a volume's `driver_opts` on creation. This can result in
unexpected behavior unless you're very well-versed in the crazy world of
containers and volumes.

- The "device" path cannot be changed just by editing it: you have to remove
  the volume and recreate it
- The "device" path must exist before you start the stack
- When you remove a volume, you're destroying what podman "owns": if it's a
  named volume with no options, podman owns the volume and its contents; if
  it's just a list of options with a host directory in `driver_opts`, podman
  only owns the definition, not the directory and not its contents.

Note that drop-in files are visible read-only inside the container. Edit them
from the host side or else create a one-off container for editing. Never change
the volume to read-write: Caddy should not be allowed to edit its own config.
