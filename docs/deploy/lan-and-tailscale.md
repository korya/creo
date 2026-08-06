# Reaching Creo from your other devices

**Status:** M4 · Laptop profile (trust tier **T1**) · implements PRD open question #4

Creo runs on one machine in your home. This is how your phone, tablet, or
laptop reaches it — and, just as importantly, how it stays out of reach of
everyone else.

Two supported paths. Pick by where you are, not by how technical you feel:

| Where you are | Use | Gets HTTPS? |
|---|---|---|
| Same home network | **LAN** — bind Creo to your machine's local address | No (see the caveat) |
| Anywhere else, or you want HTTPS | **Tailscale** — join the machine to your tailnet | Yes, free |

Putting Creo directly on the public internet is **not supported at T1** and
the server refuses to start that way while passwordless accounts exist. See
"What the platform refuses" below.

---

## Before either path: create accounts

Whoever runs the server creates one account per person. This is the permanent
answer for a household — no passwords, no identity provider to run:

```sh
creo account new Anna --color '#e07a5f'
creo account new Sam
creo account ls
```

Everyone signs in by tapping their name. Be clear-eyed about what that means:
**anyone who can reach the server can be anyone.** Within a home that is
usually the point — you are choosing convenience among people who trust each
other. The app says so on a banner that reappears every time the page loads,
and PRD §5.3 records it as a limit of the T1 tier, not a bug.

If that is not acceptable for your situation, you need a real login, which
means the `oidc` driver — not shipped yet (deferral D6, owed at M5).

---

## Path 1 — Same home network (LAN)

Find your machine's local address:

```sh
ipconfig getifaddr en0        # macOS
hostname -I | awk '{print $1}' # Linux
```

Start Creo bound to it, rather than to loopback:

```sh
creo serve --addr 192.168.1.10:8080 --serve-addr 192.168.1.10:8081 --data ~/creo-data
```

On any device in the house, open `http://192.168.1.10:8080`. Preview and
published links are generated from the address you connected on, so they open
correctly on a phone — nothing points at `127.0.0.1`.

**Keep the address stable.** Ask your router to reserve a fixed IP for the
machine (usually "DHCP reservation" in its settings), or the address will
change and the bookmark will break.

> **Caveat: plain HTTP.** A LAN address has no certificate, so browsers treat
> the page as an insecure context. Sign-in and building work fine. Some browser
> features are HTTPS-only and will not be available; a device on your network
> can in principle observe the traffic. If that matters to you, use Tailscale —
> the same setup, with real certificates.

---

## Path 2 — Anywhere, with HTTPS (Tailscale)

[Tailscale](https://tailscale.com) puts your devices on a private network of
your own, wherever they are. Creo does not integrate with it; it simply binds
to the address Tailscale provides.

**1. Join the machine and your devices to your tailnet:**

```sh
tailscale up
tailscale ip -4          # e.g. 100.101.102.103
```

**2. Bind Creo to the tailnet address:**

```sh
creo serve --addr 100.101.102.103:8080 --serve-addr 100.101.102.103:8081 --data ~/creo-data
```

Open `http://100.101.102.103:8080` from any device on your tailnet, at home or
away. Creo treats Tailscale's address range (100.64.0.0/10) as private, so this
needs no override.

**3. For HTTPS, let Tailscale terminate TLS** using the certificate it issues
for your machine's MagicDNS name:

```sh
tailscale serve --bg --https=443 http://127.0.0.1:8080
tailscale serve --bg --https=8443 http://127.0.0.1:8081
```

Then run Creo on loopback and tell it the public name, so preview and publish
links match what the browser is using:

```sh
creo serve --data ~/creo-data \
  --public-url https://your-machine.tailXXXX.ts.net:8443
```

Now `https://your-machine.tailXXXX.ts.net` is a proper secure context — no
browser warnings, no HTTPS-only features missing.

> Only devices on your tailnet can reach any of this. Do not use
> `tailscale funnel`, which publishes to the whole internet — Creo will refuse
> to start in that shape unless you force it.

---

## What the platform refuses, and why

Creo will not start when **passwordless accounts exist** and the address it
would listen on is **reachable from outside a private network**. The message
names the way out:

```
refusing to start: account-switch login (passwordless) is enabled and public
address 203.0.113.7 is reachable beyond your private network. Anyone on the
internet could open the account picker. Bind a private address (e.g. your LAN
or Tailscale IP), or — only if you truly mean it — start with --allow-unsecured
```

Treated as private, and therefore allowed: loopback, `192.168.x.x`, `10.x.x.x`,
`172.16–31.x.x`, Tailscale's `100.64.0.0/10`, IPv6 unique-local (`fd00::/8`),
and link-local. `0.0.0.0` is judged by the addresses your machine actually has:
fine on a home network, refused on a machine with a public IP.

`--allow-unsecured` exists so a deliberate choice is possible. It is logged
loudly at startup and reported by `/healthz` for as long as it is in effect. If
you find yourself reaching for it, what you probably want is Tailscale.

**This catches mistakes, not attackers.** A machine behind NAT with a
port-forward configured looks private from the inside; Creo cannot see your
router. The check is honest about being a guardrail, not a boundary.

---

## Backup

Everything lives in the data directory you passed to `--data`: the database,
the saved versions of every site, and uploaded files. Copy that directory
while the server is stopped and you have a complete backup.

(A documented, tested backup and restore procedure — including hot backups —
is M5 work. This paragraph is the honest interim answer.)

---

## Troubleshooting

**The picker shows no names.** No accounts exist yet: run `creo account new`.

**Preview links do not open on my phone.** You are probably serving on
loopback. Bind the LAN or tailnet address, or pass `--public-url`.

**"That took a while — please pick your name again."** The sign-in page had
been open too long. Sign-in attempts expire after five minutes; reload.

**A device cannot connect at all.** Check that Creo is bound to a routable
address (`--addr`, not the default loopback) and that a local firewall allows
ports 8080 and 8081.
