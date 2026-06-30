# mdns-wildcard-server

A small **mDNS server** that answers `.local` hostnames — including **wildcards** —
from a static config file. No upstream lookup, no service discovery: just fast,
authoritative answers for the names you configure.

Think of it as a tiny *proxy responder*: it can answer `.local` queries for hosts
that don't speak mDNS themselves or live in a different subnet (similar in spirit to
a Bonjour proxy). Handy to make a whole namespace like `*.apps.local` resolvable on
your LAN without per-host configuration.

## Features

- **Wildcards:** `*.apps.local` answers `grafana.apps.local`, `a.b.apps.local`, …
- **Static config**, no upstream DNS dependency.
- **Hot reload:** edit the config and it's picked up instantly (fsnotify) — or send
  `SIGHUP`. An invalid config is rejected and the previous one stays active.
- IPv4 (A) and IPv6 (AAAA) records, auto-detected.
- Correct mDNS behaviour: QU-bit / legacy-unicast handling, cache-flush bit, valid
  source address.
- Coexists with avahi / other mDNS software on port 5353 (`SO_REUSEPORT`).
- Single small static binary (Go).

## Build

```sh
go build -o mdns-wildcard-server .
```

## Configuration

`records.conf` (copy from `records.conf.example`):

```
# <pattern>  <ip>
nas.local        192.168.1.10     # exact name
*.apps.local     192.168.1.20     # wildcard: grafana.apps.local, a.b.apps.local, ...
```

- `*.X` matches `X` itself and any number of sub-labels before it.
- IPv4 → A record, IPv6 → AAAA, detected automatically.
- First matching rule wins.
- **Hot reload:** save the file → applied immediately; or `kill -HUP <pid>`.

> **Keep patterns narrow** (e.g. `*.apps.local`). A broad `*.local` would answer
> *every* `.local` query and can clash with real mDNS devices (printers, Chromecasts,
> host announcements). Use it only if you really mean to.

## Running

```sh
./mdns-wildcard-server -config records.conf -iface eth0 -ttl 120 -v
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-config` | `records.conf` | Path to the records file (hot-reloaded) |
| `-iface` | `(auto)` | Network interface to use (recommended on multi-interface hosts) |
| `-ttl` | `120` | Answer TTL in seconds (legacy-unicast is capped at 10s) |
| `-v` | off | Log every answered query |

## Does it need root?

**No.** Port 5353 is unprivileged and joining the multicast group needs no special
capabilities — run it as an **unprivileged user**. The provided systemd unit uses
`DynamicUser=yes` (no root at runtime) and starts at boot, which is the recommended
way to run it as a server. It can also run as a `systemd --user` service, but that
only runs while the user is logged in (unless lingering is enabled).

## systemd

See [`mdns-wildcard-server.service`](mdns-wildcard-server.service). Run **one
instance per subnet** (mDNS is link-local and not routed).

```ini
[Unit]
Description=mDNS wildcard server
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/mdns-wildcard-server -config /etc/mdns-wildcard-server/records.conf -iface eth0
Restart=on-failure
RestartSec=2
DynamicUser=yes
ReadOnlyPaths=/etc/mdns-wildcard-server
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

## Notes & limitations

- **Link-local:** mDNS multicast is not routed → run one instance per L2 subnet, on a
  host that lives in that subnet.
- **Port 5353 coexistence:** runs alongside avahi etc. As long as you only serve
  patterns no real device owns, there are no conflicts.
- **No probing / conflict defence:** intentionally a pure proxy responder for names
  you "own".
- **IPv4 multicast group only** for now (`224.0.0.251`); the IPv6 group (`ff02::fb`)
  is not joined yet.

## License

[MIT](LICENSE)
