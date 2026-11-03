# Deploying fixgw

Everything runs from `docker-compose.yml`: the gateway, a demo market maker so the
dashboard is alive, and optionally Caddy for HTTPS. Tested: the image builds,
`docker compose up` gives a gateway with a live book on the dashboard, and the
`public` profile runs on a GCP `e2-micro` (amd64) behind a DuckDNS name with a
Caddy certificate. Not tested: the Oracle steps and the arm64 build.

## Try it locally

```bash
FIXGW_ADMIN_TOKEN=$(openssl rand -hex 24) docker compose up --build
```

Dashboard at <http://127.0.0.1:8080/>. The FIX port is bound to `127.0.0.1:5001`.

## Free hosting options

| Option | FIX port | Notes |
|---|---|---|
| **Oracle Cloud Always Free** (recommended) | yes | A real VM with a public IP. Ampere A1 (arm64) VMs are the generous option; the AMD micro VM is small (1 GB). Sign-up needs a card for verification, and A1 capacity is sometimes unavailable in a region: retry or pick another. Check Oracle's current Always Free terms before relying on this. |
| Google Cloud e2-micro | yes | Always Free in a few US regions, small, and bandwidth is limited. Check current terms. |
| Any small VPS | yes | A few euros a month. The same steps apply. |
| Cloudflare Tunnel from a home machine | no | HTTP only. Fine for showing the dashboard, but FIX is raw TCP and cannot go through it. |

Serverless platforms (Cloud Run, Lambda, and similar) do not fit: the gateway holds
long-lived TCP sessions and in-memory order state.

## Oracle Cloud, step by step

1. **Create the VM.** Compute, Instances, Create. Image: Ubuntu 24.04. Shape: Ampere
   (`VM.Standard.A1.Flex`), 1 OCPU and 6 GB is plenty. Add your SSH key. Note the public IP.
2. **Open the cloud firewall.** Networking, your VCN, Security List: add ingress rules
   for TCP 80 and 443 from `0.0.0.0/0`. Add 5001 only if you want to offer FIX access
   (see the security notes).
3. **Open the VM's own firewall.** Oracle's Ubuntu images ship iptables rules that drop
   everything but SSH, so the cloud rule alone is not enough:
   ```bash
   sudo iptables -I INPUT 6 -p tcp --dport 80 -j ACCEPT
   sudo iptables -I INPUT 6 -p tcp --dport 443 -j ACCEPT
   sudo netfilter-persistent save
   ```
4. **Get a name.** Caddy needs a DNS name to issue a certificate. A free one: create a
   subdomain at duckdns.org and point it at the VM's public IP.
5. **Install Docker and fetch the code.**
   ```bash
   curl -fsSL https://get.docker.com | sudo sh && sudo usermod -aG docker $USER   # log out and in
   git clone https://github.com/manav2701/fixgw.git && cd fixgw
   ```
6. **Configure.**
   ```bash
   cp .env.example .env
   sed -i "s/^FIXGW_ADMIN_TOKEN=.*/FIXGW_ADMIN_TOKEN=$(openssl rand -hex 24)/" .env
   sed -i "s/^DOMAIN=.*/DOMAIN=yourname.duckdns.org/" .env
   ```
7. **Start.**
   ```bash
   docker compose --profile public up -d --build
   ```
   The first build on a small ARM VM takes a few minutes. Then open
   `https://yourname.duckdns.org/`.

## Google Cloud (the demo's host)

Both clouds ask for a card. Check Google's current Always Free terms: at the time of
writing, one `e2-micro` in `us-west1`, `us-central1` or `us-east1` with a standard disk.

1. **Create the VM.** Compute Engine, Create instance: series E2, machine type
   `e2-micro`, region `us-east1` (or one of the other two), boot disk Debian 13 or
   Ubuntu 24.04 on a **Standard** persistent disk, and tick **Allow HTTP traffic** and
   **Allow HTTPS traffic**. Remove the default snapshot schedule.
2. **Reserve a static external IP** (VPC network, IP addresses), then point a DuckDNS
   name at it.
3. **Add swap**, because the Go build needs more than 1 GB:
   `sudo fallocate -l 2G /swapfile && sudo chmod 600 /swapfile && sudo mkswap /swapfile && sudo swapon /swapfile`
4. Install Docker and continue with steps 5 to 7 above (there is no iptables step on GCP).

The first build took about half an hour on the e2-micro, running unattended.

## Operating it

- **Logs:** `docker compose logs -f fixgw`
- **Update:** `git pull && docker compose --profile public up -d --build`
- **Restart nightly.** The gateway keeps every order in memory and never frees them,
  and the demo creates a few thousand a day. A nightly restart keeps a small VM
  healthy; sequence numbers survive it, open orders do not:
  ```
  0 4 * * * cd /home/ubuntu/fixgw && docker compose restart fixgw demo
  ```
- **Kill switch:** from the dashboard (enter `FIXGW_ADMIN_TOKEN`), or
  `curl -X POST -H "Authorization: Bearer $TOKEN" "https://yourname.duckdns.org/api/killswitch?on=true&mass_cancel=true"`.
- **Back up** the `data` volume if you care about the audit log (`/data/audit.jsonl`,
  hash-chained) and the sequence-number database.

## Security notes

- The dashboard feed has no login. It shows the demo market's order flow and the FIX
  tape with passwords redacted. If that is not what you want public, put basic auth
  in `deploy/Caddyfile` (`basicauth`) or leave the `public` profile off.
- The default credentials are the public demo values in `deploy/credentials.example.json`.
  They are fine while the FIX port stays on `127.0.0.1`. Before publishing port 5001,
  run `scripts/gen-credentials.sh`, set `CREDS_FILE=./deploy/credentials.json` and
  `FIX_BIND=0.0.0.0` in `.env`, and note the demo market maker then needs that file
  too (it reads the same one).
- There is no TLS on the FIX port. Real counterparties should come over a VPN or an
  `stunnel`-style wrapper.
- The container runs as an unprivileged user, from a distroless image with no shell.
