# Demo Environment (HostAfrica)

Operate the public demonstration stack on one Ubuntu VPS (ADR-031): Compose + Caddy, MailHog as the mail sink until a transactional provider is chosen. Only ports 80 and 443 are reachable from the internet.

## Hosts

| Hostname | Role |
|----------|------|
| `yellowcommerce.co.za`, `www.yellowcommerce.co.za` | Platform / console (`yc-platform-admin`) |
| `demo.yellowcommerce.co.za` | Demo shop `yc-demo` (`yc-platform-shop`); `/webhooks/*` → yc-gateway |
| `images.yellowcommerce.co.za` | Public image CDN → SeaweedFS |

## Prerequisites

- HostAfrica Ubuntu LTS VPS (~4 vCPU / 16 GB), key-only SSH, UFW: 22 (your IPs), 80, 443; unattended-upgrades; Docker Engine + Compose plugin
- DNS A/AAAA for the four names above → VPS public IP
- GHCR read access for `ghcr.io/slaghuis/yc-platform/*` (and frontend packages)
- Off-VM backup target (rclone remote or rsync host) before announce
- Clone of this repo on the VM (e.g. `/opt/yc-platform`) as the deploy user

## First-time setup

1. **Harden the VM** (if not already): deploy user with sudo, disable password SSH, enable UFW and unattended-upgrades.
2. **Clone and secrets:**
   ```bash
   cd /opt/yc-platform
   cp deploy/demo/.env.demo.example deploy/demo/.env.demo
   cp deploy/demo/seaweedfs-s3.json.example deploy/demo/seaweedfs-s3.json
   chmod 600 deploy/demo/.env.demo deploy/demo/seaweedfs-s3.json
   ```
   Fill every blank in `.env.demo` (see `rotate-secrets.md` for how to generate values). Align SeaweedFS JSON keys with `S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY`.
3. **JWT keys:**
   ```bash
   mkdir -p deploy/demo/keys
   openssl genrsa -out deploy/demo/keys/jwt-private.pem 2048
   openssl rsa -in deploy/demo/keys/jwt-private.pem -pubout -out deploy/demo/keys/jwt-public.pem
   chmod 600 deploy/demo/keys/jwt-private.pem
   ```
4. **GHCR login** (once): `echo $GHCR_TOKEN | docker login ghcr.io -u USER --password-stdin`
5. **Deploy:** `VERSION=… ./deploy/demo/deploy.sh` (or set `VERSION` in `.env.demo`).
6. **Wait for TLS:** Caddy obtains certificates once DNS answers. Check `docker logs yc-edge`.
7. **Bootstrap** per [`deploy/demo/bootstrap.md`](../../deploy/demo/bootstrap.md): system admin → provision `yc-demo` → accept invite (MailHog tunnel) → Stripe/Yoco test webhooks → catalogue seed.
8. **Backup cron:** install [`deploy/demo/backup.cron.example`](../../deploy/demo/backup.cron.example); set `BACKUP_REMOTE` in `.env.demo`.
9. **Restore drill** (below) once before announcing the environment.

## Deploy / upgrade

```bash
cd /opt/yc-platform
git fetch --tags && git checkout <tag>   # e.g. v1.1.0
# bump VERSION in deploy/demo/.env.demo to match
./deploy/demo/deploy.sh
```

Before releases that touch the saga, event bus or gateway: run `task load` and `scripts/chaos-test.sh` against a stack built from that tag (ADR-026 rule 9), then deploy.

## Observability and MailHog

Nothing on Jaeger, yc-monitor UIs or MailHog is published. Tunnel from your laptop:

```bash
ssh -L 16686:127.0.0.1:16686 -L 8025:127.0.0.1:8025 deploy@VPS
# Then from the VPS (or add socat/docker publish to 127.0.0.1 only):
docker port   # prefer: ssh -N -L 8025:$(docker inspect -f '{{.NetworkSettings.Networks.yc-platform_default.IPAddress}}' yc-mailhog):8025 …
```

Simpler pattern — forward to Docker-published localhost binds if you temporarily add them for ops, or:

```bash
ssh -N \
  -L 8025:$(docker inspect -f '{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}}' yc-mailhog):8025 \
  -L 16686:$(docker inspect -f '{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}}' yc-jaeger):16686 \
  deploy@VPS
```

Open `http://127.0.0.1:8025` for MailHog and `http://127.0.0.1:16686` for Jaeger.

## Restore

1. Stop the app profile (keep volumes until you decide to wipe):  
   `docker compose --env-file deploy/demo/.env.demo -f docker-compose.yml -f deploy/demo/compose.demo.yml --profile app --profile frontends stop`
2. Pick a stamp under `$BACKUP_DIR` (or pull from `BACKUP_REMOTE`).
3. Restore Postgres:
   ```bash
   gunzip -c "$BACKUP_DIR/<stamp>/pg.dump.gz" | docker exec -i yc-postgres psql -U postgres
   ```
4. Restore SeaweedFS data (stop seaweedfs first, replace `/data`, start again) using `s3data.tar.gz` from that stamp.
5. `./deploy/demo/deploy.sh` and smoke-test login + a product image.

## Rebuild from scratch

1. `docker compose … down -v` (destroys Postgres and object data).
2. Redeploy, re-run bootstrap and catalogue seed.
3. Re-register payment webhooks if secrets rotated.

## Exit checks (announce)

- [ ] Only 80/443 open from the internet (`nmap` / HostAfrica firewall)
- [ ] HTTPS on platform and demo hosts; test Yoco checkout + webhook + email in MailHog
- [ ] Nightly backup ran; restore drill recorded
- [ ] Frontends pinned to the announced platform tag (`v1.1.0`)

## Related

- [`rotate-secrets.md`](rotate-secrets.md)
- [`onboarding-tenant.md`](onboarding-tenant.md), [`yoco-setup.md`](yoco-setup.md)
- [`deploy/demo/README.md`](../../deploy/demo/README.md)
- ADR-026, ADR-027, ADR-031
