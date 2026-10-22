# Demo deployment (HostAfrica)

ADR-031 reference Compose stack for `yellowcommerce.co.za` / `demo.yellowcommerce.co.za`.

| File | Purpose |
|------|---------|
| `compose.demo.yml` | Override: GHCR images, no host ports except Caddy 80/443, frontends + edge |
| `Caddyfile` | TLS edge routing (platform → admin, demo shop + webhooks, images CDN) |
| `.env.demo.example` | Secrets template → copy to `.env.demo` on the VM (mode 600) |
| `postgres-init.sh` | DB/users using `POSTGRES_APP_PASSWORD` |
| `seaweedfs-s3.json.example` | Copy to `seaweedfs-s3.json` with real keys |
| `deploy.sh` | Pull tags and `compose up` |
| `backup.sh` / `backup.cron.example` | Nightly `pg_dumpall` + object tarball, 14-day retention |
| `bootstrap.md` | First system admin, `yc-demo`, payment webhooks |

Operator procedures: [`docs/runbooks/demo-environment.md`](../../docs/runbooks/demo-environment.md), [`docs/runbooks/rotate-secrets.md`](../../docs/runbooks/rotate-secrets.md).

```bash
cp deploy/demo/.env.demo.example deploy/demo/.env.demo
# fill secrets; generate JWT keys into deploy/demo/keys/
cp deploy/demo/seaweedfs-s3.json.example deploy/demo/seaweedfs-s3.json
chmod 600 deploy/demo/.env.demo
./deploy/demo/deploy.sh
```
